package hook

import (
	"bytes"
	"strings"
	"testing"
)

func TestShouldOffload(t *testing.T) {
	yes := []string{"go test ./...", "cargo test -p foo", "npm test", "pnpm run test -- --run", "cd pkg && go test -run TestX .",
		"go test ./... 2>&1 | tail -50", "CGO_ENABLED=0 go test ./x", "python -m pytest -x", "npx vitest run src/a.test.ts"}
	no := []string{"ls", "git status", "go mod tidy", "rm -rf x && go test ./...", "offload run -- go test", "npm install", "go test $(go list ./...)"}
	for _, c := range yes {
		if !ShouldOffload(c) {
			t.Errorf("expected offload: %q", c)
		}
	}
	for _, c := range no {
		if ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
}

func TestClaudeHook(t *testing.T) {
	var out bytes.Buffer
	ClaudePreToolUse(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"go test ./... -run 'A|B'","description":"run tests"}}`), &out, "/usr/local/bin/offload")
	want := `'/usr/local/bin/offload' run -- 'go test ./... -run '\''A|B'\'''`
	if !strings.Contains(out.String(), "updatedInput") || !strings.Contains(out.String(), `"description":"run tests"`) {
		t.Fatalf("got %s", out.String())
	}
	if Rewrite("/usr/local/bin/offload", "go test ./... -run 'A|B'") != want {
		t.Fatalf("rewrite: %s", Rewrite("/usr/local/bin/offload", "go test ./... -run 'A|B'"))
	}
	out.Reset()
	ClaudePreToolUse(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), &out, "offload")
	if out.Len() != 0 {
		t.Fatalf("expected no output, got %s", out.String())
	}
}
