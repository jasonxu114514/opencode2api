package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAttemptDeadlineCutsHungHeaders(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = doInferenceAttempt(srv.Client(), req, 5*time.Second, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an attempt timeout, got nil error")
	}
	if !errors.Is(err, errAttemptTimeout) {
		t.Fatalf("expected errAttemptTimeout, got %v", err)
	}
	if got := classifyAttemptError(err); got != "attempt_timeout_headers" {
		t.Fatalf("classify = %q, want attempt_timeout_headers", got)
	}
	if elapsed > time.Second {
		t.Fatalf("attempt deadline did not cut a hung exit in time: %v", elapsed)
	}
}

// The attempt deadline guards only the wait for the first event; it must never
// leak into the established stream that follows.
func TestAttemptDeadlineReleasesAfterFirstEvent(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"x\":1}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := doInferenceAttempt(srv.Client(), req, 5*time.Second, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("first event arrived, expected success, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected a response")
	}
	defer resp.Body.Close()
	if elapsed > 2*time.Second {
		t.Fatalf("attempt deadline leaked into the established stream: %v", elapsed)
	}
}



func TestClassifyAttemptError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"attempt timeout in write phase", &phasedError{err: errAttemptTimeout, phase: "write"}, "attempt_timeout_write"},
		{"attempt timeout in dial phase", &phasedError{err: errAttemptTimeout, phase: "dial"}, "attempt_timeout_dial"},
		{"first event timeout", &phasedError{err: errFirstEventTimeout, phase: "first_event"}, "first_event"},
		{"request deadline with headers phase", &phasedError{err: context.DeadlineExceeded, phase: "headers"}, "timeout_headers"},
		{"plain transport error", &phasedError{err: errors.New("connection reset"), phase: "dial"}, "transport_dial"},
		{"bare context deadline", context.DeadlineExceeded, "timeout"},
	}
	for _, tc := range cases {
		if got := classifyAttemptError(tc.err); got != tc.want {
			t.Errorf("%s: classifyAttemptError() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPhasedErrorUnwrapKeepsIs(t *testing.T) {
	err := &phasedError{err: errAttemptTimeout, phase: "write"}
	if !errors.Is(err, errAttemptTimeout) {
		t.Fatal("phasedError must unwrap to errAttemptTimeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("errAttemptTimeout must remain a DeadlineExceeded for the retry loop")
	}
}

func TestAttemptTracePhase(t *testing.T) {
	var trace attemptTrace
	if got := trace.phase(); got != "dial" {
		t.Fatalf("fresh trace phase = %q, want dial", got)
	}
	trace.gotConn = true
	if got := trace.phase(); got != "write" {
		t.Fatalf("after GotConn phase = %q, want write", got)
	}
	trace.wroteRequest = true
	if got := trace.phase(); got != "headers" {
		t.Fatalf("after WroteRequest phase = %q, want headers", got)
	}
	trace.gotFirstByte = true
	if got := trace.phase(); got != "first_event" {
		t.Fatalf("after GotFirstResponseByte phase = %q, want first_event", got)
	}
}
