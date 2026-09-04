package main

import (
	"encoding/json"
	"testing"
)

func TestIsStaleReasoningReference(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"message":"Upstream request failed: [invalid_request_error] Referenced reasoning item 'rs_7145a5ca29c1537d081efdb1' was not found or has expired.", "type":"invalid_request_error"}}`, true},
		{`{"error":{"message":"reasoning item rs_abc has expired", "type":"invalid_request_error"}}`, true},
		{`{"error":{"message":"Model is unavailable.", "type":"upstream_error"}}`, false},
		{`{"error":{"message":"invalid_request_error: missing field 'model'", "type":"invalid_request_error"}}`, false},
		{`not json at all`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isStaleReasoningReference([]byte(tc.body)); got != tc.want {
			t.Errorf("isStaleReasoningReference(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestStripStaleReasoningInputs(t *testing.T) {
	route := modelRoute{
		ID:        "test-model",
		Tier:      TierZen,
		Protocol:  ProtocolResponses,
		Protocols: map[Tier]Protocol{TierZen: ProtocolResponses, TierGo: ProtocolChat},
	}
	responsesBody := []byte(`{"model":"test-model","previous_response_id":"resp_123","input":[{"type":"reasoning","id":"rs_old","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)
	chatBody := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	bodies := map[Tier][]byte{TierZen: responsesBody, TierGo: chatBody}

	stripped, changed := stripStaleReasoningInputs(route, bodies)
	if !changed {
		t.Fatal("expected changed=true when reasoning references exist")
	}
	var payload map[string]any
	if err := json.Unmarshal(stripped[TierZen], &payload); err != nil {
		t.Fatalf("stripped zen body is not valid JSON: %v", err)
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Error("previous_response_id was not removed")
	}
	input, _ := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("expected 1 input item left, got %d", len(input))
	}
	if m, _ := input[0].(map[string]any); stringAt(m, "type") != "message" {
		t.Errorf("unexpected remaining input item: %v", input[0])
	}
	if string(stripped[TierGo]) != string(chatBody) {
		t.Error("non-Responses tier body must pass through untouched")
	}

	cleanBodies := map[Tier][]byte{TierZen: []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)}
	if _, changed := stripStaleReasoningInputs(route, cleanBodies); changed {
		t.Error("expected changed=false when no reasoning references exist")
	}
}
