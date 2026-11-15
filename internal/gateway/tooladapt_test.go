package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

// codexRenames builds the rename set a shaped Codex request produces: the
// official names are bound to exec_command and apply_patch, and apply_patch is
// marked custom.
func codexRenames() *wire.ToolRenames {
	renames := wire.NewToolRenames()
	for _, client := range []string{"exec_command", "apply_patch"} {
		renames.Add(client, wire.ClientToolWireName(client, nil))
	}
	renames.MarkCustom("apply_patch")
	for _, official := range []string{"read", "write", "edit", "glob", "grep", "webfetch", "websearch", "skill", "shell"} {
		renames.BindOfficial(official, "exec_command")
	}
	renames.BindOfficial("patch", "apply_patch")
	return renames
}

// TestBindExecCommandAdaptersAlwaysBindsPatch covers the platform-independent
// adapters: a custom patch rewrite only reshapes JSON, so it must be registered
// on every host, while the PowerShell file adapters wait for a Windows client.
func TestBindExecCommandAdaptersAlwaysBindsPatch(t *testing.T) {
	for _, platform := range []string{"windows", "linux", "darwin", ""} {
		renames := codexRenames()
		bindExecCommandAdapters(renames, platform)
		for _, official := range []string{"patch", "shell", "todowrite"} {
			if _, ok := renames.ArgumentTransform(official); !ok && official != "todowrite" {
				t.Fatalf("platform %q: %s has no argument transform", platform, official)
			}
		}
		assertTransformJSON(t, renames, "patch", `{"patchText":"*** Begin Patch\n*** End Patch"}`, map[string]any{
			"input": "*** Begin Patch\n*** End Patch",
		})
	}
}

// TestBindExecCommandAdaptersWindowsFiles checks the PowerShell adapters are
// registered for a Windows client and left out for every other platform.
func TestBindExecCommandAdaptersWindowsFiles(t *testing.T) {
	for _, official := range []string{"read", "write", "edit", "glob", "grep", "webfetch", "websearch", "skill"} {
		windows := codexRenames()
		bindExecCommandAdapters(windows, "windows")
		if _, ok := windows.ArgumentTransform(official); !ok {
			t.Fatalf("windows: %s has no argument transform", official)
		}
		linux := codexRenames()
		bindExecCommandAdapters(linux, "linux")
		if _, ok := linux.ArgumentTransform(official); ok {
			t.Fatalf("linux: %s has a PowerShell transform", official)
		}
	}
}

// TestWindowsToolCandidatesFollowTheClientPlatform checks the exec_command
// fallback follows the reported client platform instead of the gateway host.
func TestWindowsToolCandidatesFollowTheClientPlatform(t *testing.T) {
	candidates := func(platform string) []string {
		return OfficialToolCandidates("read", platform)
	}
	if got := candidates("windows"); len(got) != 2 || got[1] != "exec_command" {
		t.Fatalf("windows read candidates = %v, want Read and exec_command", got)
	}
	for _, platform := range []string{"linux", "darwin", "unknown"} {
		if got := candidates(platform); len(got) != 1 {
			t.Fatalf("%s read candidates = %v, want only Read", platform, got)
		}
	}
}

// TestFileAdaptersAcceptBothPathSpellings covers the 1.x lane, which names the
// file argument filePath, and the 2.x lane, which names it path.
func TestFileAdaptersAcceptBothPathSpellings(t *testing.T) {
	for _, key := range []string{"path", "filePath"} {
		raw := `{"` + key + `":"C:\\work\\notes.txt"}`
		for name, transform := range map[string]wire.ArgumentTransform{
			"read":  readToExecCommand,
			"write": writeToExecCommand,
			"edit":  editToExecCommand,
		} {
			if name == "write" {
				raw = `{"` + key + `":"C:\\work\\notes.txt","content":"hi"}`
			}
			if name == "edit" {
				raw = `{"` + key + `":"C:\\work\\notes.txt","oldString":"a","newString":"b"}`
			}
			out, err := transform(raw)
			if err != nil {
				t.Fatalf("%s(%s): %v", name, key, err)
			}
			var arguments map[string]any
			if err := json.Unmarshal([]byte(out), &arguments); err != nil {
				t.Fatalf("%s(%s): invalid arguments %q: %v", name, key, out, err)
			}
			cmd, _ := arguments["cmd"].(string)
			if !strings.Contains(cmd, "notes.txt") {
				t.Fatalf("%s(%s): synthesized command does not use the path: %q", name, key, cmd)
			}
		}
	}
}

// TestEditAdapterEmitsPowerShellBooleans checks the replaceAll flag reaches the
// script as $true/$false: a bare true is parsed as a command name in
// PowerShell.
func TestEditAdapterEmitsPowerShellBooleans(t *testing.T) {
	for _, testCase := range []struct {
		arguments string
		want      string
	}{
		{`{"path":"a.txt","oldString":"a","newString":"b"}`, "$all = $false"},
		{`{"path":"a.txt","oldString":"a","newString":"b","replaceAll":false}`, "$all = $false"},
		{`{"path":"a.txt","oldString":"a","newString":"b","replaceAll":true}`, "$all = $true"},
	} {
		out, err := editToExecCommand(testCase.arguments)
		if err != nil {
			t.Fatalf("%s: %v", testCase.arguments, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		cmd, _ := decoded["cmd"].(string)
		if !strings.Contains(cmd, testCase.want) {
			t.Fatalf("%s: command is missing %q:\n%s", testCase.arguments, testCase.want, cmd)
		}
		for _, bare := range []string{"= true", "= false"} {
			if strings.Contains(cmd, bare) {
				t.Fatalf("%s: command uses a bare PowerShell boolean %q:\n%s", testCase.arguments, bare, cmd)
			}
		}
	}
}

// TestSkillAdapterAcceptsBothIDSpellings covers the 1.x skill tool, which names
// its argument name, and the 2.x tool, which names it id.
func TestSkillAdapterAcceptsBothIDSpellings(t *testing.T) {
	for _, raw := range []string{`{"id":"deploy"}`, `{"name":"deploy"}`} {
		out, err := skillToExecCommand(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		cmd, _ := decoded["cmd"].(string)
		if !strings.Contains(cmd, "'deploy'") {
			t.Fatalf("%s: command does not search for the skill:\n%s", raw, cmd)
		}
	}
}

// TestPatchFoldsBackOnTheOneXLane covers a model that answers with a "patch"
// call although the 1.x lane declares no patch tool: the call still has to come
// back as the client's custom apply_patch tool.
func TestPatchFoldsBackOnTheOneXLane(t *testing.T) {
	for _, v1 := range []bool{true, false} {
		payload := map[string]any{"tools": []any{map[string]any{
			"type":        "custom",
			"name":        "apply_patch",
			"description": "Patch files",
			"format":      map[string]any{"type": "grammar"},
		}}}
		renames, ok := renameAndCompleteTools(payload, wire.Responses, v1, "linux")
		if !ok {
			t.Fatalf("v1=%v: request was not shaped", v1)
		}
		if bound, _ := renames.Official("patch"); bound != "apply_patch" {
			t.Fatalf("v1=%v: patch is bound to %q, want apply_patch", v1, bound)
		}
		client, found := renames.Resolve("patch")
		if !found || client != "apply_patch" || !renames.IsCustom(client) {
			t.Fatalf("v1=%v: patch resolves to %q (custom %v)", v1, client, renames.IsCustom(client))
		}
		if !renames.HasCustom() {
			t.Fatalf("v1=%v: custom marker was not recorded", v1)
		}
	}
}

// TestPatchFoldsOntoExecCommand covers Codex 0.17x, which declares no
// apply_patch tool and expects the apply_patch command instead.
func TestPatchFoldsOntoExecCommand(t *testing.T) {
	payload := map[string]any{"tools": []any{map[string]any{
		"type":       "function",
		"name":       "exec_command",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}},
	}}}
	renames, ok := renameAndCompleteTools(payload, wire.Responses, false, "linux")
	if !ok {
		t.Fatalf("request was not shaped")
	}
	if bound, _ := renames.Official("patch"); bound != "exec_command" {
		t.Fatalf("patch is bound to %q, want exec_command", bound)
	}
	transform, ok := renames.ArgumentTransform("patch")
	if !ok {
		t.Fatalf("patch has no argument transform")
	}
	out, err := transform(`{"patchText":"*** Begin Patch\n*** Add File: hi.txt\n+hi\n*** End Patch"}`)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(out), &arguments); err != nil {
		t.Fatalf("transform argued %q: %v", out, err)
	}
	cmd, _ := arguments["cmd"].(string)
	if !strings.HasPrefix(cmd, "apply_patch <<'PATCH_EOF'\n") || !strings.HasSuffix(cmd, "\nPATCH_EOF\n") {
		t.Fatalf("command is not an apply_patch heredoc: %q", cmd)
	}
	if !strings.Contains(cmd, "+hi\n") {
		t.Fatalf("command lost the patch text: %q", cmd)
	}
}

// TestPatchTransformEscapesHeredocDelimiter checks a patch containing the
// default delimiter cannot end the heredoc early.
func TestPatchTransformEscapesHeredocDelimiter(t *testing.T) {
	out, err := patchToExecCommand(`{"patchText":"PATCH_EOF\n*** End Patch"}`)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(out), &arguments); err != nil {
		t.Fatalf("transform argued %q: %v", out, err)
	}
	cmd, _ := arguments["cmd"].(string)
	if !strings.HasPrefix(cmd, "apply_patch <<'PATCH_EOF_EOF'\n") {
		t.Fatalf("delimiter did not grow: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "\nPATCH_EOF_EOF\n") {
		t.Fatalf("command does not close with the grown delimiter: %q", cmd)
	}
	// The patch body only ever holds the shorter delimiter, so the quotes
	// make it inert for the shell.
	if !strings.Contains(cmd, "\nPATCH_EOF\n") {
		t.Fatalf("patch body was altered: %q", cmd)
	}
}

// TestWindowsEnvironmentWiresFileAdapters checks the client platform Codex
// reports in its environment block reaches the adapters, so a Windows Codex
// gets the PowerShell rewrites while a Linux one does not.
func TestWindowsEnvironmentWiresFileAdapters(t *testing.T) {
	for _, testCase := range []struct {
		platform string
		reported string
		want     bool
	}{
		{"windows", "win32", true},
		{"linux", "linux", false},
	} {
		text := "<environment_context>\n  <cwd>C:\\work</cwd>\n  <shell>powershell</shell>\n  <platform>" + testCase.platform + "</platform>\n</environment_context>"
		environment, _, ok := parseCodexEnvironment(text)
		if !ok || environment.Platform != testCase.reported {
			t.Fatalf("%s: environment = %+v (ok %v)", testCase.platform, environment, ok)
		}
		payload := map[string]any{"tools": []any{map[string]any{
			"type": "function",
			"name": "exec_command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"cmd": map[string]any{"type": "string"},
			}},
		}}}
		renames, ok := renameAndCompleteTools(payload, wire.Responses, false, environment.Platform)
		if !ok {
			t.Fatalf("%s: request was not shaped", testCase.platform)
		}
		if _, bound := renames.ArgumentTransform("read"); bound != testCase.want {
			t.Fatalf("%s: read transform registered = %v, want %v", testCase.platform, bound, testCase.want)
		}
	}
}
