package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v1.3.6", "v1.3.7", true},
		{"1.3.7", "v1.3.7", false},
		{"v1.3.7-rc.1", "v1.3.7", true},
		{"v1.4.0", "v1.3.7", false},
		{"dev", "v1.3.7", false},
	}
	for _, test := range cases {
		if got := versionLess(test.current, test.latest); got != test.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "payload")
	data := []byte("hello updater")
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	checksumPath := filepath.Join(dir, "payload.sha256")
	if err := os.WriteFile(checksumPath, []byte(hex.EncodeToString(digest[:])+"  payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifySHA256(filePath, checksumPath); err != nil {
		t.Fatalf("verifySHA256() error = %v", err)
	}
	if err := os.WriteFile(checksumPath, []byte(stringsRepeat("0", 64)+"  payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifySHA256(filePath, checksumPath); err == nil {
		t.Fatal("verifySHA256() accepted an incorrect checksum")
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "release.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	entry, err := writer.Create("opencode2api_vtest_windows_amd64/opencode2api.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "opencode2api.exe")
	if err := extractZip(archivePath, "opencode2api.exe", output); err != nil {
		t.Fatalf("extractZip() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("extracted data = %q, want %q", got, "binary")
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "release.tar.gz")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(archiveFile)
	tarWriter := tar.NewWriter(gzipWriter)
	data := []byte("binary")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "opencode2api_vtest_linux_amd64/opencode2api", Mode: 0755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "opencode2api")
	if err := extractTarGz(archivePath, "opencode2api", output); err != nil {
		t.Fatalf("extractTarGz() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("extracted data = %q, want %q", got, data)
	}
}

func stringsRepeat(s string, n int) string {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}
