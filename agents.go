package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// What is left of the v2 "background agent" backend after DESIGN §11: v3 never
// spawns `claude --bg`, never reads the fleet view and never parses job state.
// Only the two primitives every other file needs survive — where the `claude`
// binary is, and how to run a read-only `claude` subcommand under a profile's
// scrubbed environment.

func claudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err == nil {
		// common install location
		cand := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "claude"
}

// runClaudeOutput runs a `claude` subcommand under a profile's scrubbed
// environment and hands back stdout even when the command exits non-zero:
// some subcommands report state through the exit code while still printing
// valid JSON (`auth status --json` exits 1 for a logged-out config dir, which
// is exactly how ccc detects a profile that needs a new login). Callers that
// can read the payload should prefer it over the error.
//
// Every claude exec in ccc goes through claudeEnv — see envWhitelist for why
// inheriting os.Environ() is unsafe.
func runClaudeOutput(p Profile, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBin(), args...)
	cmd.Env = claudeEnv(p)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("claude %s failed: %w (%s)", strings.Join(args, " "), err, truncate(msg, 300))
	}
	return stdout.Bytes(), nil
}
