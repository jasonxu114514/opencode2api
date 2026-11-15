package gateway

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	wire "opencode2api/internal/protocol"
)

// Codex declares one general executor, exec_command, where the other agents
// have dedicated shell and file tools. The free tier always advertises the
// official OpenCode set, so a model can call shell/read/edit/... even when the
// downstream client has no handler for them. These adapters bind those official
// names to exec_command and rewrite their arguments into a command Codex can
// run. They are registered per request and only when the client actually
// declared exec_command (or, for patch, its custom apply_patch tool), so Claude
// Code and OpenCode clients keep their native tools unchanged.

// bindExecCommandAdapters registers the argument rewrites for official tools
// that a shaped Codex request maps to exec_command or apply_patch. The shell
// rewrite is platform-neutral because the command text is passed through to the
// client's own shell; the file rewrites synthesize an executor command and run
// only on Windows, where the gateway and the client share a host.
func bindExecCommandAdapters(renames *wire.ToolRenames) {
	if renames == nil {
		return
	}
	bind := func(official, client string, transform wire.ArgumentTransform) bool {
		declared, ok := renames.Official(official)
		if !ok || !sameToolName(declared, client) {
			return false
		}
		renames.SetArgumentTransform(official, transform)
		return true
	}
	bind("shell", "exec_command", shellToExecCommand)
	bind("bash", "exec_command", shellToExecCommand)
	bind("todowrite", "update_plan", todoWriteToUpdatePlan)
	if runtime.GOOS != "windows" {
		return
	}
	bind("read", "exec_command", readToExecCommand)
	bind("write", "exec_command", writeToExecCommand)
	bind("edit", "exec_command", editToExecCommand)
	bind("glob", "exec_command", globToExecCommand)
	bind("grep", "exec_command", grepToExecCommand)
	bind("webfetch", "exec_command", webfetchToExecCommand)
	bind("websearch", "exec_command", websearchToExecCommand)
	bind("skill", "exec_command", skillToExecCommand)
	if declared, ok := renames.Official("patch"); ok && sameToolName(declared, "apply_patch") && renames.Custom(declared) {
		renames.SetArgumentTransform("patch", patchToCustomInput)
	}
}

// sameToolName folds case and the separators agents use in tool names, so
// exec_command, exec-command and ExecCommand name one tool.
func sameToolName(left, right string) bool {
	return foldToolName(left) == foldToolName(right)
}

func foldToolName(name string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		switch char {
		case '_', '.', '-', ' ', '\t', '\r', '\n':
			continue
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// decodeArguments reads one tool call's arguments object. Empty arguments are
// an empty object, and malformed JSON is returned to the caller unchanged.
func decodeArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return arguments, nil
}

func encodeArguments(arguments map[string]any) (string, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func stringArgument(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return value
}

func intArgument(arguments map[string]any, key string) int {
	value, ok := arguments[key]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		number, err := typed.Int64()
		if err == nil {
			return int(number)
		}
	}
	return 0
}

func boolArgument(arguments map[string]any, key string) bool {
	value, _ := arguments[key].(bool)
	return value
}

// execCommandArguments renders the exec_command call for a synthesized command.
func execCommandArguments(command, workdir string) (string, error) {
	arguments := map[string]any{"cmd": command}
	if workdir != "" {
		arguments["workdir"] = workdir
	}
	return encodeArguments(arguments)
}

// shellToExecCommand maps the official shell arguments (command, workdir,
// timeout, background) to exec_command's cmd and workdir. The command text is
// already written for the client's shell, so it is passed through verbatim.
func shellToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	command := stringArgument(arguments, "command")
	if command == "" {
		return raw, nil
	}
	return execCommandArguments(command, stringArgument(arguments, "workdir"))
}

// readToExecCommand renders the official read arguments as a PowerShell read,
// including directory listings and offset/limit windows.
func readToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	path := stringArgument(arguments, "path")
	if path == "" {
		return raw, nil
	}
	literal := psLiteral(path)
	script := fmt.Sprintf(`if (Test-Path -LiteralPath %s -PathType Container) { Get-ChildItem -LiteralPath %s | ForEach-Object { $_.Name } } else `, literal, literal)
	offset := intArgument(arguments, "offset")
	limit := intArgument(arguments, "limit")
	switch {
	case offset <= 0 && limit <= 0:
		script += fmt.Sprintf(`{ Get-Content -LiteralPath %s -Raw -Encoding UTF8 }`, literal)
	default:
		lines := []string{fmt.Sprintf(`$lines = @(Get-Content -LiteralPath %s -Encoding UTF8)`, literal)}
		if skip := offset - 1; skip > 0 {
			lines = append(lines, fmt.Sprintf(`if ($lines.Count -gt %d) { $lines = $lines[%d..($lines.Count - 1)] } else { $lines = @() }`, skip, skip))
		}
		if limit > 0 {
			lines = append(lines, fmt.Sprintf(`if ($lines.Count -gt %d) { $lines = $lines[0..%d] }`, limit, limit-1))
		}
		lines = append(lines, "$lines -join \"`n\"")
		script += "{ " + strings.Join(lines, "; ") + " }"
	}
	return execCommandArguments(script, "")
}

// writeToExecCommand renders the official write arguments as a PowerShell
// write that creates parent directories and keeps UTF-8 without a BOM.
func writeToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	path := stringArgument(arguments, "path")
	if path == "" {
		return raw, nil
	}
	content, ok := arguments["content"].(string)
	if !ok {
		content = ""
	}
	script := fmt.Sprintf(`$p = %s
$c = %s
$d = Split-Path -Parent $p
if ($d -and -not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
Set-Content -LiteralPath $p -Value $c -NoNewline -Encoding utf8
Write-Output ('Wrote ' + $p)`, psLiteral(path), psQuoted(content))
	return execCommandArguments(script, "")
}

// editToExecCommand renders the official edit arguments as an exact-string
// replacement in PowerShell, failing loudly when the target is missing or not
// unique.
func editToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	path := stringArgument(arguments, "path")
	oldString, hasOld := arguments["oldString"].(string)
	newString, hasNew := arguments["newString"].(string)
	if path == "" || !hasOld || !hasNew {
		return raw, nil
	}
	script := fmt.Sprintf(`$p = %s
$old = %s
$new = %s
$all = %t
try {
  if ($old -eq '') { throw 'oldString must not be empty' }
  if (-not (Test-Path -LiteralPath $p)) { throw ('file not found: ' + $p) }
  $text = Get-Content -LiteralPath $p -Raw -Encoding UTF8
  if ($null -eq $text) { $text = '' }
  $index = $text.IndexOf($old)
  if ($index -lt 0) { throw 'oldString not found' }
  if ($all) { $text = $text.Replace($old, $new) } else {
    if ($text.IndexOf($old, $index + $old.Length) -ge 0) { throw 'oldString is not unique; set replaceAll' }
    $text = $text.Substring(0, $index) + $new + $text.Substring($index + $old.Length)
  }
  Set-Content -LiteralPath $p -Value $text -NoNewline -Encoding utf8
  Write-Output ('Edited ' + $p)
} catch {
  Write-Error $_.Exception.Message
  exit 1
}`, psLiteral(path), psQuoted(oldString), psQuoted(newString), boolArgument(arguments, "replaceAll"))
	return execCommandArguments(script, "")
}

// globToExecCommand renders the official glob arguments as a PowerShell file
// search whose pattern is translated to a .NET regular expression.
func globToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	pattern := stringArgument(arguments, "pattern")
	if pattern == "" {
		return raw, nil
	}
	path := stringArgument(arguments, "path")
	if path == "" {
		path = "."
	}
	limit := intArgument(arguments, "limit")
	if limit <= 0 {
		limit = 100
	}
	expression := globToRegexp(pattern)
	force := ""
	if boolArgument(arguments, "hidden") {
		force = " -Force"
	}
	script := fmt.Sprintf(`$root = %s
$rx = %s
Get-ChildItem -LiteralPath $root -Recurse -File%s -ErrorAction SilentlyContinue | Where-Object { $_.FullName.Replace('\','/') -match $rx } | Select-Object -First %d -ExpandProperty FullName`, psLiteral(path), psLiteral(expression), force, limit)
	return execCommandArguments(script, "")
}

// grepToExecCommand renders the official grep arguments as a PowerShell
// Select-String search that keeps the file:line:text output shape.
func grepToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	pattern := stringArgument(arguments, "pattern")
	if pattern == "" {
		return raw, nil
	}
	path := stringArgument(arguments, "path")
	if path == "" {
		path = "."
	}
	limit := intArgument(arguments, "limit")
	if limit <= 0 {
		limit = 100
	}
	simple := ""
	if boolArgument(arguments, "literal") {
		simple = " -SimpleMatch"
	}
	caseSensitive := ""
	if boolArgument(arguments, "caseSensitive") {
		caseSensitive = " -CaseSensitive"
	}
	script := fmt.Sprintf(`$root = %s
$pattern = %s
$include = %s
$files = if (Test-Path -LiteralPath $root -PathType Container) { Get-ChildItem -LiteralPath $root -Recurse -File -ErrorAction SilentlyContinue } else { Get-Item -LiteralPath $root -ErrorAction SilentlyContinue }
if ($include) { $files = $files | Where-Object { $_.Name -like $include } }
$files | Select-String -Pattern $pattern%s%s -ErrorAction SilentlyContinue | Select-Object -First %d | ForEach-Object { '{0}:{1}:{2}' -f $_.Path, $_.LineNumber, $_.Line }`, psLiteral(path), psLiteral(pattern), psLiteral(stringArgument(arguments, "include")), simple, caseSensitive, limit)
	return execCommandArguments(script, "")
}

// websearchToExecCommand runs the official websearch query through Bing and
// returns the result page text. It is a fallback for clients that have no web
// search of their own; the model can fetch a result with webfetch. curl handles
// the query escaping without the .NET calls that constrained language mode
// blocks.
func websearchToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	query := strings.TrimSpace(stringArgument(arguments, "query"))
	if query == "" {
		return raw, nil
	}
	limit := intArgument(arguments, "contextMaxCharacters")
	if limit <= 0 || limit > 20000 {
		limit = 8000
	}
	script := fmt.Sprintf(`$q = %s
$html = & curl.exe -s -L --max-time 30 -A 'Mozilla/5.0 (Windows NT 10.0; Win64; x64)' -G --data-urlencode ('q=' + $q) --data-urlencode 'count=10' 'https://www.bing.com/search'
$text = ($html -join ' ')
$text = $text -replace '(?s)<script.*?</script>|<style.*?</style>', ' '
$text = $text -replace '<[^>]+>', ' '
$text = $text -replace '\s+', ' '
if ($text.Length -gt %d) { $text = $text.Substring(0, %d) }
Write-Output $text`, psLiteral(query), limit, limit)
	return encodeArguments(map[string]any{"cmd": script})
}

func webfetchToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	url := stringArgument(arguments, "url")
	if url == "" {
		return raw, nil
	}
	timeout := intArgument(arguments, "timeout")
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 120 {
		timeout = 120
	}
	script := fmt.Sprintf(`$response = Invoke-WebRequest -Uri %s -UseBasicParsing -TimeoutSec %d
$response.Content`, psLiteral(url), timeout)
	return execCommandArguments(script, "")
}

// skillToExecCommand renders the official skill arguments as a search for the
// skill's SKILL.md in the agent skill roots, falling back to a recursive
// lookup by directory name.
func skillToExecCommand(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	id := stringArgument(arguments, "id")
	if id == "" {
		return raw, nil
	}
	script := fmt.Sprintf(`$id = %s; $roots = @((Join-Path $env:USERPROFILE '.codex\skills'), (Join-Path $env:USERPROFILE '.codex\skills\.system'), (Join-Path $env:USERPROFILE '.claude\skills'), (Join-Path $env:USERPROFILE '.config\opencode\skills'), (Join-Path $env:USERPROFILE '.pi\agent\skills'), '.codex\skills', '.claude\skills', '.opencode\skills') | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -Unique; $found = $null; foreach ($root in $roots) { $candidate = Join-Path $root (Join-Path $id 'SKILL.md'); if (Test-Path -LiteralPath $candidate) { $found = $candidate; break } }; if (-not $found) { $found = Get-ChildItem -Path $roots -Recurse -Filter SKILL.md -ErrorAction SilentlyContinue | Where-Object { $_.Directory.Name -eq $id } | Select-Object -First 1 -ExpandProperty FullName }; if ($found) { Get-Content -LiteralPath $found -Raw -Encoding UTF8 } else { Write-Output ('Skill not found: ' + $id); Get-ChildItem -Path $roots -Recurse -Filter SKILL.md -ErrorAction SilentlyContinue | ForEach-Object { $_.Directory.Name } | Sort-Object -Unique }`, psLiteral(id))
	return execCommandArguments(script, "")
}

// patchToCustomInput maps the official patch tool's patchText to the freeform
// input envelope the Responses bridge uses for Codex's custom apply_patch tool.
// todoWriteToUpdatePlan converts the official todo list into the plan the Codex
// update_plan tool expects. Cancelled entries are dropped because Codex has no
// cancelled status; the other statuses are shared.
func todoWriteToUpdatePlan(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	todos, ok := arguments["todos"].([]any)
	if !ok {
		return raw, nil
	}
	plan := make([]any, 0, len(todos))
	for _, rawTodo := range todos {
		todo, _ := rawTodo.(map[string]any)
		step := strings.TrimSpace(stringArgument(todo, "content"))
		if step == "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(stringArgument(todo, "status")))
		switch status {
		case "in_progress", "completed":
		case "cancelled":
			continue
		default:
			status = "pending"
		}
		plan = append(plan, map[string]any{"step": step, "status": status})
	}
	return encodeArguments(map[string]any{"plan": plan})
}

func patchToCustomInput(raw string) (string, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return raw, err
	}
	patch := stringArgument(arguments, "patchText")
	if patch == "" {
		return raw, nil
	}
	return encodeArguments(map[string]any{"input": patch})
}

// psLiteral embeds a value in a PowerShell single-quoted string. Single-quoted
// strings take backslashes, dollar signs and backticks literally, so only a
// quote needs doubling.
func psLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// psQuoted embeds arbitrary text, including newlines, in a PowerShell
// double-quoted string. Backticks, dollar signs, quotes and line breaks are
// escaped so the text is neither expanded nor reinterpreted. The Codex sandbox
// runs PowerShell in constrained language mode, where .NET static methods such
// as base64 decoding are blocked, so string literals are the portable encoding.
func psQuoted(value string) string {
	replacer := strings.NewReplacer(
		"\x60", "\x60\x60",
		"$", "\x60$",
		"\"", "\x60\"",
		"\r", "\x60r",
		"\n", "\x60n",
		"\t", "\x60t",
	)
	return "\"" + replacer.Replace(value) + "\""
}

// globToRegexp translates a glob into an anchored .NET regular expression over
// paths normalized to forward slashes. ** spans directories, * stays within
// one segment, ? matches one character, and {a,b} alternates.
func globToRegexp(pattern string) string {
	var builder strings.Builder
	builder.WriteString("^(?:.*/)?")
	runes := []rune(strings.ReplaceAll(pattern, "\\", "/"))
	for index := 0; index < len(runes); index++ {
		char := runes[index]
		switch char {
		case '*':
			if index+1 < len(runes) && runes[index+1] == '*' {
				index++
				if index+1 < len(runes) && runes[index+1] == '/' {
					index++
					builder.WriteString("(?:.*/)?")
				} else {
					builder.WriteString(".*")
				}
				continue
			}
			builder.WriteString("[^/]*")
		case '?':
			builder.WriteString("[^/]")
		case '{':
			end := index + 1
			for end < len(runes) && runes[end] != '}' {
				end++
			}
			if end < len(runes) {
				builder.WriteString("(?:")
				for optionIndex, option := range strings.Split(string(runes[index+1:end]), ",") {
					if optionIndex > 0 {
						builder.WriteString("|")
					}
					builder.WriteString(regexpQuote(option))
				}
				builder.WriteString(")")
				index = end
				continue
			}
			builder.WriteString("\\{")
		default:
			builder.WriteString(regexpQuote(string(char)))
		}
	}
	builder.WriteString("$")
	return builder.String()
}

// regexpQuote escapes one regular-expression character. It uses the .NET
// escape set shared with Go's regexp for the punctuation globs care about.
func regexpQuote(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, char) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(char)
	}
	return builder.String()
}
