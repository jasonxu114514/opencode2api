package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const validQuota = `{"usage":{"rolling":{"status":"ok","percent":19,"resetsAt":"2026-09-20T12:00:00Z"},"weekly":{"status":"rate-limited","percent":100,"resetsAt":"2026-09-21T00:00:00Z"},"monthly":{"status":"ok","percent":0,"resetsAt":"2026-10-20T00:00:00Z"}}}`

func TestQuotaDecode(t *testing.T) {
	windows, err := decodeQuota(strings.NewReader(validQuota))
	if err != nil {
		t.Fatal(err)
	}
	if windows["rolling"].RemainingPercent != 81 || windows["weekly"].RemainingPercent != 0 || windows["monthly"].RemainingPercent != 100 {
		t.Fatal("incorrect percentage semantics")
	}
	for _, raw := range []string{`{}`, strings.Replace(validQuota, `"percent":19`, `"percent":null`, 1), strings.Replace(validQuota, `"percent":19`, `"percent":101`, 1), strings.Replace(validQuota, "2026-09-20T12:00:00Z", "invalid", 1), strings.Replace(validQuota, `"status":"ok"`, `"status":"unknown"`, 1)} {
		if _, err := decodeQuota(strings.NewReader(raw)); err == nil {
			t.Fatal("malformed quota accepted")
		}
	}
}

func TestQuotaFailureRetainsLastGoodAndRedacts(t *testing.T) {
	failing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-upstream-secret" {
			t.Error("missing authorization")
		}
		if r.Header.Get("User-Agent") != "opencode2api-admin/1.0" {
			t.Error("incorrect identity")
		}
		if failing {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"test-upstream-secret"}`))
			return
		}
		w.Write([]byte(validQuota))
	}))
	defer srv.Close()
	first := refreshQuota(context.Background(), srv.URL, "test-upstream-secret", quotaCacheEntry{})
	if first.Account.Status != "ok" {
		t.Fatal(first.Account.Error)
	}
	failing = true
	stale := refreshQuota(context.Background(), srv.URL, "test-upstream-secret", first)
	if stale.Account.Status != "stale" || stale.Account.Windows["rolling"].RemainingPercent != 81 || !stale.Account.FetchedAt.Equal(*first.Account.FetchedAt) {
		t.Fatal("last known quota lost")
	}
	encoded, _ := json.Marshal(stale.Account)
	if strings.Contains(string(encoded), "test-upstream-secret") {
		t.Fatal("secret leaked")
	}
	missing := refreshQuota(context.Background(), srv.URL, "test-upstream-secret", quotaCacheEntry{})
	if missing.Account.Status != "unavailable" || missing.Account.Windows != nil {
		t.Fatal("invented unavailable balance")
	}
}

func TestQuotaDoesNotFollowRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	if _, err := fetchQuota(context.Background(), source.URL, "secret"); err == nil || reached {
		t.Fatal("redirect leaked credentials")
	}
}
