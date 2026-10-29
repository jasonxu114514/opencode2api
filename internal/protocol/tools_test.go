package protocol

import (
	"strings"
	"testing"
)

// The free tier refuses a shaped body whose tools still name another client's
// session: Claude Code declaring both ReportFindings and ScheduleWakeup is
// answered with FreeUsageLimitError. The wire name therefore may not echo the
// client name, which is what these tests pin down.
func TestClientToolWireNameIsOpaque(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash", "mcp__server__do_thing"} {
		wireName := ClientToolWireName(client, used)
		if !strings.HasPrefix(wireName, ClientToolPrefix) {
			t.Errorf("wire name %q does not carry the client prefix", wireName)
		}
		if len(wireName) > toolWireNameLimit {
			t.Errorf("wire name %q is longer than %d bytes", wireName, toolWireNameLimit)
		}
		if strings.Contains(strings.ToLower(wireName), strings.ToLower(client)) {
			t.Errorf("wire name %q still spells the client name %q", wireName, client)
		}
		if strings.ContainsAny(wireName, " \t\r\n") {
			t.Errorf("wire name %q is not one token", wireName)
		}
	}
}

func TestClientToolWireNameIsStable(t *testing.T) {
	first := ClientToolWireName("ReportFindings", map[string]bool{})
	second := ClientToolWireName("ReportFindings", map[string]bool{})
	if first != second {
		t.Fatalf("same client tool got %q then %q", first, second)
	}
	other := ClientToolWireName("ScheduleWakeup", map[string]bool{})
	if first == other {
		t.Fatalf("two client tools share the wire name %q", first)
	}
}

func TestClientToolWireNameSuffixesCollisions(t *testing.T) {
	used := map[string]bool{}
	first := ClientToolWireName("Read", used)
	// Hand the allocator a used set that already holds this tool's name, the
	// way a second definition of the same client tool would.
	retry := ClientToolWireName("Read", used)
	if retry == first {
		t.Fatalf("a taken wire name was handed out twice: %q", first)
	}
	if !strings.HasSuffix(retry, "_2") {
		t.Errorf("collision suffix missing from %q", retry)
	}
}

func TestToolRenamesRestoreOpaqueNames(t *testing.T) {
	renames := NewToolRenames()
	used := map[string]bool{}
	for _, client := range []string{"ReportFindings", "schedule_wakeup"} {
		wireName := ClientToolWireName(client, used)
		if wireName == client {
			t.Fatalf("%q was not renamed", client)
		}
		renames.Add(client, wireName)
		if got := renames.Restore(wireName); got != client {
			t.Errorf("Restore(%q) = %q, want %q", wireName, got, client)
		}
	}
}
