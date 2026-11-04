package gateway

import (
	_ "embed"
	"encoding/json"
	"runtime"
	"strings"
	"sync"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
)

// The free tier only accepts agent-shaped traffic, and each lane validates the
// tool definitions against its own CLI generation. These assets are the
// genuine definitions captured from the official clients: opencode_tools.json
// for the 2.x lanes (/inference and /zen/go) and opencode_tools_v1.json for the
// 1.x lane (/zen).
//
//go:embed opencode_tools.json
var opencodeToolsV2JSON []byte

//go:embed opencode_tools_v1.json
var opencodeToolsV1JSON []byte

// opencodeToolDefinition is the description and schema of one official tool.
type opencodeToolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// v2OfficialToolOrder is the key order of opencode_tools.json, made explicit
// because Go maps do not preserve it. Tool order is not validated upstream, but
// keeping the captured order makes the wire body stable.
var v2OfficialToolOrder = []string{
	"edit", "execute", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "patch",
}

var (
	opencodeToolsOnce    sync.Once
	opencodeToolsV2      map[string]opencodeToolDefinition
	opencodeToolsV1      map[string]opencodeToolDefinition
	opencodeToolsV1Order []string
)

// loadOfficialTools decodes both embedded tool sets once. A file that fails to
// decode leaves that generation with no known definitions, which only degrades
// the definition back to a placeholder; the tool names are still appended.
func loadOfficialTools() {
	opencodeToolsOnce.Do(func() {
		opencodeToolsV2 = map[string]opencodeToolDefinition{}
		if err := json.Unmarshal(opencodeToolsV2JSON, &opencodeToolsV2); err != nil {
			opencodeToolsV2 = map[string]opencodeToolDefinition{}
		}
		opencodeToolsV1 = map[string]opencodeToolDefinition{}
		opencodeToolsV1Order = nil
		var entries []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(opencodeToolsV1JSON, &entries); err == nil {
			for _, entry := range entries {
				name := entry.Function.Name
				if name == "" {
					continue
				}
				opencodeToolsV1[name] = opencodeToolDefinition{
					Description: entry.Function.Description,
					Parameters:  entry.Function.Parameters,
				}
				opencodeToolsV1Order = append(opencodeToolsV1Order, name)
			}
		}
	})
}

// officialToolNames returns the names of the official set for the lane.
func officialToolNames(v1 bool) []string {
	loadOfficialTools()
	if v1 {
		return opencodeToolsV1Order
	}
	return v2OfficialToolOrder
}

// officialToolDefinition returns the captured definition for an official name.
func officialToolDefinition(name string, v1 bool) (opencodeToolDefinition, bool) {
	loadOfficialTools()
	if v1 {
		definition, ok := opencodeToolsV1[name]
		return definition, ok
	}
	definition, ok := opencodeToolsV2[name]
	return definition, ok
}

// officialTool renders one official definition in the wire shape the protocol
// expects. The 2.x Chat shape marks every tool strict:false, as the client does;
// the captured 1.x entries carry no strict field and are rendered verbatim.
func officialTool(protocol wire.Protocol, name string, v1 bool) map[string]any {
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if definition, ok := officialToolDefinition(name, v1); ok {
		description = definition.Description
		if definition.Parameters != nil {
			parameters = definition.Parameters
		}
	}
	if v1 {
		if name == "bash" {
			description = v1BashDescription(description)
		}
	} else if name == "shell" {
		description = v2ShellDescription(description)
	}
	switch protocol {
	case wire.Chat:
		function := map[string]any{
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}
		if !v1 {
			function["strict"] = false
		}
		return map[string]any{"type": "function", "function": function}
	case wire.Anthropic:
		return map[string]any{
			"name":         name,
			"description":  description,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}
	}
}

// officialToolset renders the full official set for a lane.
func officialToolset(protocol wire.Protocol, v1 bool) []any {
	names := officialToolNames(v1)
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, officialTool(protocol, name, v1))
	}
	return tools
}

// v2ShellDescription renders the shell tool description for the host platform.
// The 2.x client describes the shell it actually has, so a Windows host says
// powershell where a Unix host says bash.
func v2ShellDescription(base string) string {
	if runtime.GOOS != "windows" {
		return base
	}
	return strings.Replace(base, "on Linux using bash", "on Windows using powershell", 1)
}

// v1BashDescription applies the same platform rewrite to the 1.x bash
// definition, which states its OS and shell inline.
func v1BashDescription(base string) string {
	if runtime.GOOS != "windows" {
		return base
	}
	return strings.Replace(base, "OS: linux, Shell: bash", "OS: windows, Shell: powershell", 1)
}

// renameAndCompleteTools makes the tool list read as an official CLI session:
// every named client tool is renamed to an opaque wire name, legacy functions
// arrays are converted, and the full official set for the lane is appended.
// The returned map restores the client names in the upstream response.
func renameAndCompleteTools(payload map[string]any, protocol wire.Protocol, v1 bool) (*wire.ToolRenames, bool) {
	switch protocol {
	case wire.Chat, wire.Responses, wire.Anthropic:
	default:
		return nil, false
	}
	renames := wire.NewToolRenames()
	used := map[string]bool{}
	var items []any
	if raw, exists := payload["tools"]; exists {
		if list, ok := raw.([]any); ok {
			items = list
		}
	}
	if protocol == wire.Chat || protocol == wire.Responses {
		if legacy, ok := payload["functions"].([]any); ok {
			items = append(items, convertLegacyFunctions(legacy, protocol, renames, used)...)
			delete(payload, "functions")
		}
	}
	shaped := make([]any, 0, len(items)+len(officialToolNames(v1)))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			shaped = append(shaped, item)
			continue
		}
		name, ok := renamableToolName(protocol, entry)
		if !ok {
			shaped = append(shaped, item)
			continue
		}
		if wireName := clientWireName(renames, used, name); wireName != name {
			setToolName(protocol, entry, wireName)
		}
		shaped = append(shaped, entry)
	}
	shaped = append(shaped, officialToolset(protocol, v1)...)
	payload["tools"] = shaped
	rewriteToolReferences(payload, protocol, renames)
	return renames, true
}

// renamableToolName returns the client tool name of one definition. Responses
// built-in tools (web search and similar) have no client name and are left
// untouched.
func renamableToolName(protocol wire.Protocol, entry map[string]any) (string, bool) {
	switch protocol {
	case wire.Chat:
		name := jsonutil.StringAt(entry, "function", "name")
		return name, name != ""
	case wire.Responses:
		switch jsonutil.StringAt(entry, "type") {
		case "", "function", "custom":
		default:
			return "", false
		}
		name := jsonutil.StringAt(entry, "name")
		return name, name != ""
	default:
		// Anthropic server tools (web search and similar) carry a type; a
		// plain client tool has only name/description/input_schema.
		switch jsonutil.StringAt(entry, "type") {
		case "", "custom":
		default:
			return "", false
		}
		name := jsonutil.StringAt(entry, "name")
		return name, name != ""
	}
}

// clientWireName returns the existing wire name for a client tool, or allocates
// a new one. A duplicate client definition reuses the first allocation so every
// reference keeps pointing at one name.
func clientWireName(renames *wire.ToolRenames, used map[string]bool, client string) string {
	if wireName, ok := renames.Wire(client); ok {
		return wireName
	}
	wireName := wire.ClientToolWireName(client, used)
	renames.Add(client, wireName)
	return wireName
}

// setToolName writes the wire name into one tool definition in place.
func setToolName(protocol wire.Protocol, entry map[string]any, name string) {
	if protocol == wire.Chat {
		if function, ok := entry["function"].(map[string]any); ok {
			function["name"] = name
		}
		return
	}
	entry["name"] = name
}

// convertLegacyFunctions turns the pre-tools "functions" array into entries of
// the protocol's tool shape and renames their names.
func convertLegacyFunctions(entries []any, protocol wire.Protocol, renames *wire.ToolRenames, used map[string]bool) []any {
	converted := make([]any, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := jsonutil.StringAt(entry, "name")
		if name == "" {
			continue
		}
		wireName := clientWireName(renames, used, name)
		description := jsonutil.StringAt(entry, "description")
		switch protocol {
		case wire.Chat:
			function := map[string]any{
				"name":        wireName,
				"description": description,
				"parameters":  legacyParameters(entry),
			}
			if strict, ok := entry["strict"]; ok {
				function["strict"] = strict
			}
			converted = append(converted, map[string]any{"type": "function", "function": function})
		case wire.Responses:
			tool := map[string]any{
				"type":        "function",
				"name":        wireName,
				"description": description,
				"parameters":  legacyParameters(entry),
			}
			if strict, ok := entry["strict"]; ok {
				tool["strict"] = strict
			}
			converted = append(converted, tool)
		}
	}
	return converted
}

// legacyParameters returns a legacy function's parameters, defaulting to an
// empty object schema.
func legacyParameters(entry map[string]any) any {
	if parameters, ok := entry["parameters"]; ok && parameters != nil {
		return parameters
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// rewriteToolReferences renames every outbound reference to a client tool so
// the request stays self-consistent: named tool_choice (including legacy Chat
// and Responses allowed_tools forms), legacy function_call, and the tool-call
// history each protocol keeps. String choices pass through untouched.
func rewriteToolReferences(payload map[string]any, protocol wire.Protocol, renames *wire.ToolRenames) {
	if renames.Empty() {
		return
	}
	rename := func(container map[string]any) {
		name, ok := container["name"].(string)
		if !ok {
			return
		}
		if wireName, ok := renames.Wire(name); ok {
			container["name"] = wireName
		}
	}
	renameAllowedTools := func(choice map[string]any) {
		for _, rawEntry := range jsonutil.SliceAt(choice, "allowed_tools") {
			if entry, ok := rawEntry.(map[string]any); ok {
				rename(entry)
			}
		}
	}
	switch protocol {
	case wire.Chat:
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			if function, ok := choice["function"].(map[string]any); ok {
				rename(function)
			} else {
				rename(choice)
			}
		}
		if call, ok := payload["function_call"].(map[string]any); ok {
			rename(call)
		}
		for _, rawMessage := range jsonutil.SliceAt(payload, "messages") {
			message, _ := rawMessage.(map[string]any)
			if call, ok := message["function_call"].(map[string]any); ok {
				rename(call)
			}
			for _, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				if function, ok := call["function"].(map[string]any); ok {
					rename(function)
				}
			}
		}
	case wire.Responses:
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			rename(choice)
			renameAllowedTools(choice)
		}
		for _, rawItem := range jsonutil.SliceAt(payload, "input") {
			item, _ := rawItem.(map[string]any)
			switch jsonutil.StringAt(item, "type") {
			case "function_call", "custom_tool_call":
				rename(item)
			}
		}
	case wire.Anthropic:
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			rename(choice)
		}
		for _, rawMessage := range jsonutil.SliceAt(payload, "messages") {
			message, _ := rawMessage.(map[string]any)
			for _, rawBlock := range jsonutil.SliceAt(message, "content") {
				block, _ := rawBlock.(map[string]any)
				if jsonutil.StringAt(block, "type") == "tool_use" {
					rename(block)
				}
			}
		}
	}
}
