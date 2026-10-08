package config

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestResolveProxyFilesRemoteURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("http://127.0.0.1:8080\n\nhttps://127.0.0.1:8443 # comment\n"))
	}))
	defer server.Close()

	cfg := Config{ProxyFile: server.URL}
	if err := resolveProxyFiles("/tmp/config.json", &cfg); err != nil {
		t.Fatalf("resolveProxyFiles() error = %v", err)
	}

	want := []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8443",
	}
	if !reflect.DeepEqual(cfg.effectiveProxies, want) {
		t.Fatalf("effectiveProxies = %#v, want %#v", cfg.effectiveProxies, want)
	}
}

func TestReadProxyFileRemoteHTTPError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, err := readProxyFile(server.URL)
	if err == nil {
		t.Fatal("readProxyFile() error = nil, want HTTP error")
	}
}

func TestIsRemoteProxyFile(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   bool
	}{
		{name: "http", rawURL: "http://example.com/proxies.txt", want: true},
		{name: "https", rawURL: "https://example.com/proxies.txt", want: true},
		{name: "local path", rawURL: "./proxies.txt", want: false},
		{name: "unsupported scheme", rawURL: "ftp://example.com/proxies.txt", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRemoteProxyFile(tt.rawURL); got != tt.want {
				t.Fatalf("isRemoteProxyFile(%q) = %v, want %v", tt.rawURL, got, tt.want)
			}
		})
	}
}
