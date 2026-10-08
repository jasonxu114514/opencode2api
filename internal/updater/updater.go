// Package updater provides in-process self-updating for released binaries.
package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRepository   = "jasonxu114514/opencode2api"
	DefaultCheckEvery   = 6 * time.Hour
	DefaultInitialDelay = 2 * time.Minute
	maxDownloadBytes    = 100 << 20
	helperFlag          = "-internal-update-helper"
)

func IsHelper(args []string) bool {
	return len(args) > 1 && args[1] == helperFlag
}

type Options struct {
	Repository     string
	CurrentVersion string
	Executable     string
	Args           []string
	Logger         *slog.Logger
	OnRestart      func()
	CheckInterval  time.Duration
	InitialDelay   time.Duration
}

type latestRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type updateAsset struct {
	archive  releaseAsset
	checksum releaseAsset
	binary   string
}

// Start runs the background update loop. It never blocks application startup.
func Start(ctx context.Context, opts Options) {
	if ctx == nil || !enabled() || !isReleaseVersion(opts.CurrentVersion) {
		return
	}
	if opts.Repository == "" {
		opts.Repository = DefaultRepository
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = configuredInterval()
	}
	if opts.InitialDelay <= 0 {
		opts.InitialDelay = DefaultInitialDelay
	}
	if opts.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return
		}
		opts.Executable = executable
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	go func() {
		timer := time.NewTimer(opts.InitialDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		checkAndUpdate(ctx, opts)
		ticker := time.NewTicker(opts.CheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkAndUpdate(ctx, opts)
			}
		}
	}()
}

func checkAndUpdate(ctx context.Context, opts Options) {
	latest, err := fetchLatest(ctx, opts.Repository)
	if err != nil {
		opts.Logger.Warn("auto update check failed", "component", "updater", "event", "update_check_failed", "error", err)
		return
	}
	if !versionLess(opts.CurrentVersion, latest.TagName) {
		return
	}
	asset, err := selectAsset(latest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		opts.Logger.Warn("auto update skipped", "component", "updater", "event", "update_asset_missing", "version", latest.TagName, "error", err)
		return
	}
	newBinary, cleanup, err := downloadAndPrepare(ctx, asset)
	if err != nil {
		opts.Logger.Warn("auto update download failed", "component", "updater", "event", "update_download_failed", "version", latest.TagName, "error", err)
		return
	}
	if err := launchHelper(opts.Executable, newBinary, opts.Args); err != nil {
		cleanup()
		opts.Logger.Warn("auto update launch failed", "component", "updater", "event", "update_launch_failed", "version", latest.TagName, "error", err)
		return
	}
	opts.Logger.Info("auto update scheduled", "component", "updater", "event", "update_scheduled", "from", opts.CurrentVersion, "to", latest.TagName)
	if opts.OnRestart != nil {
		opts.OnRestart()
	}
}

func fetchLatest(ctx context.Context, repository string) (latestRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return latestRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "opencode2api-auto-updater")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return latestRelease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return latestRelease{}, fmt.Errorf("GitHub API returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var release latestRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release); err != nil {
		return latestRelease{}, err
	}
	if release.TagName == "" {
		return latestRelease{}, errors.New("latest release has no tag")
	}
	return release, nil
}

func selectAsset(release latestRelease, goos, goarch string) (updateAsset, error) {
	binaryName := "opencode2api"
	extension := ".tar.gz"
	if goos == "windows" {
		binaryName += ".exe"
		extension = ".zip"
	}
	base := fmt.Sprintf("opencode2api_%s_%s_%s", release.TagName, goos, goarch)
	archiveName := base + extension
	checksumName := archiveName + ".sha256"
	var result updateAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case archiveName:
			result.archive = asset
		case checksumName:
			result.checksum = asset
		}
	}
	if result.archive.Name == "" || result.archive.BrowserDownloadURL == "" {
		return updateAsset{}, fmt.Errorf("release %s does not contain %s", release.TagName, archiveName)
	}
	if result.checksum.Name == "" || result.checksum.BrowserDownloadURL == "" {
		return updateAsset{}, fmt.Errorf("release %s does not contain %s", release.TagName, checksumName)
	}
	result.binary = binaryName
	return result, nil
}

func downloadAndPrepare(ctx context.Context, asset updateAsset) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "opencode2api-update-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	archivePath := filepath.Join(tempDir, filepath.Base(asset.archive.Name))
	checksumPath := filepath.Join(tempDir, filepath.Base(asset.checksum.Name))
	if err := downloadFile(ctx, asset.archive.BrowserDownloadURL, archivePath); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := downloadFile(ctx, asset.checksum.BrowserDownloadURL, checksumPath); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := verifySHA256(archivePath, checksumPath); err != nil {
		cleanup()
		return "", func() {}, err
	}
	stagingPath := filepath.Join(tempDir, asset.binary)
	if err := extractBinary(archivePath, asset.binary, stagingPath); err != nil {
		cleanup()
		return "", func() {}, err
	}
	targetDir, err := os.Executable()
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	targetDir = filepath.Dir(targetDir)
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	newPath := filepath.Join(targetDir, fmt.Sprintf(".opencode2api.new.%d%s", os.Getpid(), extension))
	if err := copyFile(stagingPath, newPath, 0755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	_ = os.RemoveAll(tempDir)
	// The helper consumes this path after the current process exits.
	return newPath, func() { _ = os.Remove(newPath) }, nil
}

func downloadFile(ctx context.Context, url, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "opencode2api-auto-updater")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", response.Status)
	}
	if response.ContentLength > maxDownloadBytes {
		return fmt.Errorf("download exceeds %d bytes", maxDownloadBytes)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(response.Body, maxDownloadBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if info, err := os.Stat(path); err != nil {
		return err
	} else if info.Size() > maxDownloadBytes {
		return fmt.Errorf("download exceeds %d bytes", maxDownloadBytes)
	}
	return nil
}

func verifySHA256(filePath, checksumPath string) error {
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(checksumData))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return errors.New("invalid SHA256 checksum file")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return fmt.Errorf("invalid SHA256 checksum: %w", err)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, fields[0]) {
		return fmt.Errorf("SHA256 mismatch: got %s want %s", actual, fields[0])
	}
	return nil
}

func extractBinary(archivePath, binaryName, outputPath string) error {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZip(archivePath, binaryName, outputPath)
	}
	return extractTarGz(archivePath, binaryName, outputPath)
}

func extractZip(archivePath, binaryName, outputPath string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != binaryName {
			continue
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, maxDownloadBytes))
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		return outputCloseErr
	}
	return fmt.Errorf("archive does not contain %s", binaryName)
}

func extractTarGz(archivePath, binaryName, outputPath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != binaryName {
			continue
		}
		if header.Size < 0 || header.Size > maxDownloadBytes {
			return fmt.Errorf("invalid binary size %d", header.Size)
		}
		output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(output, tarReader, header.Size)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return fmt.Errorf("archive does not contain %s", binaryName)
}

func launchHelper(executable, newBinary string, restartArgs []string) error {
	helperPath := filepath.Join(os.TempDir(), fmt.Sprintf("opencode2api-updater-%d%s", os.Getpid(), filepath.Ext(executable)))
	if err := copyFile(executable, helperPath, 0755); err != nil {
		return err
	}
	encodedArgs, err := json.Marshal(restartArgs)
	if err != nil {
		_ = os.Remove(helperPath)
		return err
	}
	encoded := base64.RawStdEncoding.EncodeToString(encodedArgs)
	command := exec.Command(helperPath, helperFlag, "-target", executable, "-new", newBinary, "-args", encoded)
	command.Env = os.Environ()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = os.Remove(helperPath)
		return err
	}
	return nil
}

// RunHelper is called by the copied helper process before normal startup.
func RunHelper(args []string) error {
	set := flag.NewFlagSet("opencode2api-update-helper", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	target := set.String("target", "", "target executable")
	newBinary := set.String("new", "", "new executable")
	encodedArgs := set.String("args", "", "base64 encoded restart arguments")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *target == "" || *newBinary == "" {
		return errors.New("update helper requires target and new executable")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(*encodedArgs)
	if err != nil {
		return err
	}
	var restartArgs []string
	if err := json.Unmarshal(decoded, &restartArgs); err != nil {
		return err
	}
	return replaceAndRestart(*target, *newBinary, restartArgs)
}

func replaceAndRestart(target, newBinary string, restartArgs []string) error {
	deadline := time.Now().Add(90 * time.Second)
	backup := target + ".bak"
	var lastErr error
	for time.Now().Before(deadline) {
		_ = os.Remove(backup)
		if err := os.Rename(target, backup); err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err := os.Rename(newBinary, target); err != nil {
			_ = os.Rename(backup, target)
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err := startReplacement(target, backup, restartArgs); err != nil {
			return err
		}
		return nil
	}
	_ = os.Remove(newBinary)
	return fmt.Errorf("could not replace executable before timeout: %w", lastErr)
}

func startReplacement(target, backup string, restartArgs []string) error {
	command := exec.Command(target, restartArgs...)
	command.Env = os.Environ()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	if err := command.Start(); err != nil {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
		return fmt.Errorf("new binary failed to start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		// A process that exits during the startup grace period is treated as a
		// failed upgrade. Restore the known-good binary and restart it once.
		_ = os.Remove(target)
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return fmt.Errorf("new binary exited (%v), and rollback failed: %w", err, restoreErr)
		}
		oldCommand := exec.Command(target, restartArgs...)
		oldCommand.Env = os.Environ()
		oldCommand.Stdout = os.Stdout
		oldCommand.Stderr = os.Stderr
		oldCommand.Stdin = os.Stdin
		if oldErr := oldCommand.Start(); oldErr != nil {
			return fmt.Errorf("new binary exited (%v), old binary restart failed: %w", err, oldErr)
		}
		return fmt.Errorf("new binary exited during startup: %w", err)
	case <-time.After(5 * time.Second):
		_ = os.Remove(backup)
		return nil
	}
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(destination, mode)
}

func isReleaseVersion(version string) bool {
	_, ok := parseVersion(version)
	return ok
}

type semver struct {
	major, minor, patch int
	prerelease          bool
}

func parseVersion(value string) (semver, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.SplitN(value, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semver{}, false
	}
	values := make([]int, 3)
	for i, raw := range numbers {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return semver{}, false
		}
		values[i] = parsed
	}
	return semver{major: values[0], minor: values[1], patch: values[2], prerelease: len(parts) == 2}, true
}

func versionLess(current, latest string) bool {
	a, okA := parseVersion(current)
	b, okB := parseVersion(latest)
	if !okA || !okB {
		return false
	}
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	return a.prerelease && !b.prerelease
}

func enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE2API_AUTO_UPDATE"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func configuredInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("OPENCODE2API_UPDATE_INTERVAL_HOURS"))
	if raw == "" {
		return DefaultCheckEvery
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours < 1 || hours > 168 {
		return DefaultCheckEvery
	}
	return time.Duration(hours) * time.Hour
}
