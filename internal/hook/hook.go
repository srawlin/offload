// Package hook rewrites agents' test/build shell commands to go through
// offload: interception beats instruction.
package hook

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// testCmd matches one segment of a shell command that runs tests or builds.
var testCmd = regexp.MustCompile(`^(?:[A-Z_][A-Z0-9_]*=\S*\s+)*(?:` + strings.Join([]string{
	`go (?:test|build|vet)\b`,
	`cargo (?:test|nextest|build|check|clippy)\b`,
	`(?:npm|pnpm|yarn|bun) (?:run )?(?:test|build|typecheck|lint)\b`,
	`npx (?:vitest|jest|mocha|tsc)\b`,
	`(?:python3? -m )?pytest\b`,
	`make (?:test|check|build)\b`,
	`(?:\./)?gradlew? test\b`,
	`mvn (?:-\S+ )*test\b`,
}, "|") + `)`)

// harmless segments that may accompany a test command.
var allowedSeg = regexp.MustCompile(`^(?:cd \S+|true|echo .*|(?:tail|head|grep|sort|uniq|wc)(?: .*)?)$`)

// splitShell splits on &&, ||, ; and | outside quotes.
func splitShell(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote rune
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteRune(c)
		case c == ';' || c == '|' || (c == '&' && i+1 < len(rs) && rs[i+1] == '&'):
			if i+1 < len(rs) && (rs[i+1] == '|' || rs[i+1] == '&') {
				i++
			}
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	return append(segs, cur.String())
}

// ShouldOffload reports whether every part of cmd is either a test/build
// command or a harmless helper, and at least one part is a test/build.
func ShouldOffload(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.HasPrefix(cmd, "offload ") || strings.Contains(cmd, "$(") || strings.ContainsAny(cmd, "`\n") {
		return false
	}
	found := false
	for _, seg := range splitShell(cmd) {
		seg = strings.TrimSuffix(strings.TrimSpace(seg), " 2>&1")
		switch {
		case testCmd.MatchString(seg):
			found = true
		case allowedSeg.MatchString(seg):
		default:
			return false
		}
	}
	return found
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Rewrite wraps the command so it runs through offload.
func Rewrite(bin, cmd string) string { return shQuote(bin) + " run -- " + shQuote(cmd) }

type preToolUse struct {
	ToolName  string                 `json:"tool_name"`
	ToolInput map[string]interface{} `json:"tool_input"`
}

// ClaudePreToolUse implements a Claude Code PreToolUse hook for the Bash
// tool. It prints nothing (no change) unless the command should be offloaded.
func ClaudePreToolUse(in io.Reader, out io.Writer, bin string) error {
	var ev preToolUse
	if err := json.NewDecoder(in).Decode(&ev); err != nil {
		return nil // never block the agent
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if ev.ToolName != "Bash" || !ShouldOffload(cmd) {
		return nil
	}
	input := map[string]interface{}{}
	for k, v := range ev.ToolInput {
		input[k] = v
	}
	input["command"] = Rewrite(bin, cmd)
	return json.NewEncoder(out).Encode(map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "allow",
			"permissionDecisionReason": "offload: running test/build on the remote node",
			"updatedInput":             input,
		},
	})
}
