package config

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func resolveProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	effective := append([]string(nil), cfg.Proxies...)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !isRemoteProxyFile(resolved) && !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		effective = append(effective, proxies...)
	}

	effective = uniqueStrings(effective)
	if len(effective) == 0 {
		effective = []string{"direct"}
	}
	cfg.effectiveProxies = effective
	return nil
}

func readProxyFile(path string) ([]string, error) {
	if isRemoteProxyFile(path) {
		return readRemoteProxyFile(path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return readProxyLines(file)
}

func isRemoteProxyFile(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Host != ""
}

func readRemoteProxyFile(rawURL string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("remote proxy file returned HTTP %s", resp.Status)
	}

	return readProxyLines(resp.Body)
}

func readProxyLines(file io.Reader) ([]string, error) {
	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

func uniqueStrings(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}
