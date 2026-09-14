package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The env file carries the secrets a systemd --user service cannot get from a
// login shell. It is written 0600, holds only the env_passthrough names, and
// is reported by name alone.
func TestSyncEnvFileWritesOnlyWhatPassthroughAsksFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", `ghp_secret"with\quotes`)
	t.Setenv("LINEAR_API_KEY", "lin_api_123")
	os.Unsetenv("NOT_SET_ANYWHERE")

	cfg := &Config{EnvPassthrough: []string{
		"GH_TOKEN", "LINEAR_API_KEY", "NOT_SET_ANYWHERE", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "",
	}}
	res, err := syncEnvFile(cfg)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	if strings.Join(res.Found, " ") != "GH_TOKEN LINEAR_API_KEY" {
		t.Errorf("found = %v, want the two that are set", res.Found)
	}
	if strings.Join(res.Missing, " ") != "NOT_SET_ANYWHERE" {
		t.Errorf("missing = %v, want the one that is not set", res.Missing)
	}
	// The report is the only thing the owner sees, and it must be names only.
	if strings.Contains(res.String(), "ghp_secret") || strings.Contains(res.String(), "lin_api_123") {
		t.Errorf("the sync report leaked a value:\n%s", res.String())
	}
	if !strings.Contains(res.String(), "bash -lc 'ccc env sync'") {
		t.Errorf("a missing name should point at a login shell:\n%s", res.String())
	}

	info, err := os.Stat(envFilePath())
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file mode = %v, want 0600: it holds live secrets", perm)
	}
	if want := filepath.Join(os.Getenv("HOME"), ".config", "ccc", "env"); envFilePath() != want {
		t.Errorf("env file path = %q, want %q", envFilePath(), want)
	}

	body, err := os.ReadFile(envFilePath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `GH_TOKEN="ghp_secret\"with\\quotes"`) {
		t.Errorf("a value with quotes and backslashes was not escaped:\n%s", text)
	}
	for _, unwanted := range []string{"NOT_SET_ANYWHERE", "CLAUDE_CONFIG_DIR", "ANTHROPIC"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("%q must not be in the env file:\n%s", unwanted, text)
		}
	}
}

// A value that cannot be written as one line is named, not mangled.
func TestSyncEnvFileSkipsMultiLineValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PEM_KEY", "-----BEGIN-----\nabc\n-----END-----")
	_, res := renderEnvFile(&Config{EnvPassthrough: []string{"PEM_KEY"}}, os.LookupEnv)
	if len(res.Skipped) != 1 || res.Skipped[0] != "PEM_KEY" {
		t.Errorf("skipped = %v, want the multi-line value to be reported", res.Skipped)
	}
	if len(res.Found) != 0 {
		t.Error("a skipped value must not count as found")
	}
}

// The file is what makes a service started without a login shell work; a value
// that IS in the environment already wins over the file.
func TestLoadEnvFileFillsOnlyTheGaps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "from-the-file")
	t.Setenv("LINEAR_API_KEY", "from-the-file-too")
	cfg := &Config{EnvPassthrough: []string{"GH_TOKEN", "LINEAR_API_KEY"}}
	if _, err := syncEnvFile(cfg); err != nil {
		t.Fatal(err)
	}

	// Simulate the service: one variable set by the shell, the other absent.
	t.Setenv("GH_TOKEN", "from-the-shell")
	os.Unsetenv("LINEAR_API_KEY")

	loaded := loadEnvFile(cfg)
	if strings.Join(loaded, " ") != "LINEAR_API_KEY" {
		t.Errorf("loaded = %v, want only the variable that was missing", loaded)
	}
	if os.Getenv("GH_TOKEN") != "from-the-shell" {
		t.Error("the file overwrote a variable the shell had already set")
	}
	if os.Getenv("LINEAR_API_KEY") != "from-the-file-too" {
		t.Errorf("the missing variable was not filled in: %q", os.Getenv("LINEAR_API_KEY"))
	}
}

// A name that is not in env_passthrough is never loaded, even if the file has
// it (a stale file after the list shrank).
func TestLoadEnvFileIgnoresNamesNoLongerAskedFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLD_TOKEN", "value")
	if _, err := syncEnvFile(&Config{EnvPassthrough: []string{"OLD_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("OLD_TOKEN")
	if loaded := loadEnvFile(&Config{EnvPassthrough: []string{"SOMETHING_ELSE"}}); len(loaded) != 0 {
		t.Errorf("loaded %v from a stale file", loaded)
	}
	if _, ok := os.LookupEnv("OLD_TOKEN"); ok {
		t.Error("a name no longer in env_passthrough was still set")
	}
}

func TestParseEnvLine(t *testing.T) {
	cases := []struct{ line, name, value string }{
		{`GH_TOKEN="abc"`, "GH_TOKEN", "abc"},
		{`GH_TOKEN="a\"b\\c"`, "GH_TOKEN", `a"b\c`},
		{`PLAIN=value`, "PLAIN", "value"},
	}
	for _, c := range cases {
		name, value, ok := parseEnvLine(c.line)
		if !ok || name != c.name || value != c.value {
			t.Errorf("parseEnvLine(%q) = %q/%q/%v", c.line, name, value, ok)
		}
	}
	for _, bad := range []string{"", "   ", "# a comment", "no-equals-here"} {
		if _, _, ok := parseEnvLine(bad); ok {
			t.Errorf("parseEnvLine(%q) should not parse", bad)
		}
	}
}

// /status names what is present and what is missing, and never a value.
func TestEnvPassthroughStatusNamesOnly(t *testing.T) {
	in, _, _ := testInstance(t)
	t.Setenv("GH_TOKEN", "ghp_do_not_print_me")
	os.Unsetenv("MISSING_TOKEN")
	in.cfg.EnvPassthrough = []string{"GH_TOKEN", "MISSING_TOKEN"}

	present, missing := envPassthroughStatus(in.cfg)
	if strings.Join(present, " ") != "GH_TOKEN" || strings.Join(missing, " ") != "MISSING_TOKEN" {
		t.Errorf("present = %v, missing = %v", present, missing)
	}
	card := in.renderStatus()
	if !strings.Contains(card, "GH_TOKEN") || !strings.Contains(card, "MISSING_TOKEN") {
		t.Errorf("/status does not report the passthrough names:\n%s", card)
	}
	if strings.Contains(card, "ghp_do_not_print_me") {
		t.Errorf("/status leaked a secret:\n%s", card)
	}
}
