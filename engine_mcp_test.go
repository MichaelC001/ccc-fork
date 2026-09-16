package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertCCCMCPBlockAppendsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[mcp_servers.playwright]\ncommand = \"npx\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upsertCCCMCPBlock(path, "/opt/ccc"); err != nil {
		t.Fatal(err)
	}
	if err := upsertCCCMCPBlock(path, "/opt/ccc"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if strings.Count(body, "[mcp_servers.ccc]") != 1 {
		t.Errorf("expected one ccc block:\n%s", body)
	}
	if !strings.Contains(body, "command = \"/opt/ccc\"") || !strings.Contains(body, "playwright") {
		t.Errorf("merge lost a section:\n%s", body)
	}
}

func TestInsertCodexMCPArgsAfterExec(t *testing.T) {
	old := cccPath
	cccPath = "/opt/ccc"
	t.Cleanup(func() { cccPath = old })

	got := insertCodexMCPArgs([]string{"exec", "--json", "hello"}, 3, 9)
	joined := strings.Join(got, " ")
	if !strings.HasPrefix(joined, "exec -c ") {
		t.Errorf(" -c should follow exec: %v", got)
	}
	if !strings.Contains(joined, `"mcp","--bot","3","--turn","9"`) {
		t.Errorf("missing bot/turn args: %v", got)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("prompt should stay last, got %v", got)
	}
}

func TestEngineHasMCP(t *testing.T) {
	if !engineHasMCP(engineClaude) || !engineHasMCP(engineGrok) || !engineHasMCP(engineCodex) {
		t.Fatal("claude/grok/codex should have MCP")
	}
	if engineHasMCP(engineAntigravity) {
		t.Fatal("agy should not")
	}
}
