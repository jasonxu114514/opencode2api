package identity

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeriveRequestIDsMarksSynthesizedSessions(t *testing.T) {
	body := map[string]any{"system": "sys", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	ids := DeriveRequestIDs(r, body, "scope")
	if !ids.SessionDerived {
		t.Fatal("expected a conversation-derived session to be marked derived")
	}
	if !canonicalSessionPattern.MatchString(ids.Session) {
		t.Fatalf("session %q does not match the canonical pattern", ids.Session)
	}
	if !descendingTimePartPattern.MatchString(ids.Session[4:16]) {
		t.Fatalf("session %q time part %q does not match the descending pattern", ids.Session, ids.Session[4:16])
	}

	const official = "ses_e9c3455c3ffe47zLLfuYyviLPd"
	r.Header.Set("x-opencode-session", official)
	ids2 := DeriveRequestIDs(r, body, "scope")
	if ids2.SessionDerived {
		t.Fatal("expected an official client session not to be marked derived")
	}
	if ids2.Session != official {
		t.Fatalf("official session changed: got %q want %q", ids2.Session, official)
	}
}

func TestWithFreshSessionIsStableAndDistinct(t *testing.T) {
	body := map[string]any{"system": "sys", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	ids := DeriveRequestIDs(r, body, "scope")
	escaped := ids.WithFreshSession()
	if escaped.Session == ids.Session {
		t.Fatal("re-minted session equals the original")
	}
	if !canonicalSessionPattern.MatchString(escaped.Session) {
		t.Fatalf("re-minted session %q does not match the canonical pattern", escaped.Session)
	}
	if escaped.Request != ids.Request || escaped.Message != ids.Message {
		t.Fatal("re-minting changed the request or message identity")
	}
	if ids.WithFreshSession().Session != escaped.Session {
		t.Fatal("re-minting is not deterministic")
	}
	if escaped.WithFreshSession().Session == escaped.Session {
		t.Fatal("re-minting an already re-minted session did not advance")
	}

	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r2.Header.Set("x-opencode-session-id", "client-child")
	r2.Header.Set("x-parent-session-id", "client-parent")
	ids2 := DeriveRequestIDs(r2, body, "scope")
	escaped2 := ids2.WithFreshSession()
	if ids2.ChildSession == "" || ids2.ParentSession == "" {
		t.Fatalf("expected child and parent sessions, got %q and %q", ids2.ChildSession, ids2.ParentSession)
	}
	if escaped2.ChildSession == ids2.ChildSession {
		t.Fatal("child session was not re-minted")
	}
	if escaped2.ParentSession == ids2.ParentSession {
		t.Fatal("parent session was not re-minted")
	}
}
