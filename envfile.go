package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// envfile.go owns <config_dir>/env: the file that carries the secrets named by
// `env_passthrough` (DESIGN §3.1) from the owner's login shell to the ccc
// service.
//
// It exists because of two facts that pull in opposite directions:
//
//   - `systemctl --user` starts a unit with a bare environment and never
//     sources ~/.profile or ~/.zshrc, so a token that is only exported there is
//     invisible to the service. Earlier versions solved that by baking
//     `Environment="NAME=value"` lines into the unit at install time, which put
//     live secrets into a 0644 file under ~/.config/systemd and showed them to
//     anything that can run `systemctl --user cat ccc`.
//   - The values still have to come from somewhere, and the only place that
//     has them is a login shell.
//
// So: `ccc install` and `ccc env sync` (run from a login shell) snapshot the
// values into a 0600 file, and the unit reads it with `EnvironmentFile=-`. The
// unit itself holds no secrets, and the file holds only what env_passthrough
// asks for. `ccc listen` also reads the file itself, which is what makes the
// same mechanism work under launchd on macOS, where a plist cannot source
// anything.
//
// Nothing here ever prints a VALUE — not to stdout, not to Telegram, not to the
// log. Only names.

// envFilePath is the file the service reads its passthrough secrets from.
func envFilePath() string { return filepath.Join(configDir(), "env") }

// envSyncResult is what one sync saw, by name only.
type envSyncResult struct {
	Path string
	// Found are the env_passthrough names this process could read.
	Found []string
	// Missing are the names env_passthrough asks for that are not set here —
	// almost always "ccc env sync was not run from a login shell".
	Missing []string
	// Skipped are names whose value cannot be expressed in the file format
	// (a value containing a newline); they are named, never shown.
	Skipped []string
}

// String is the one line `ccc install` / `ccc env sync` / `ccc config set`
// print. Names only.
func (r envSyncResult) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "env file: %s (0600)\n", r.Path)
	fmt.Fprintf(&sb, "  found:   %s\n", namesOrNone(r.Found))
	fmt.Fprintf(&sb, "  missing: %s\n", namesOrNone(r.Missing))
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&sb, "  skipped (multi-line value): %s\n", namesOrNone(r.Skipped))
	}
	if len(r.Missing) > 0 {
		sb.WriteString("  run this from a login shell so ~/.profile and ~/.zshrc are loaded:\n")
		sb.WriteString("    bash -lc 'ccc env sync'\n")
	}
	return sb.String()
}

func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, " ")
}

// passthroughNames is env_passthrough with the names that may never be passed
// through removed: a bot's CLAUDE*/ANTHROPIC* environment is built per profile
// by claudeEnv, and letting one be set here would break profile isolation.
func passthroughNames(config *Config) []string {
	if config == nil {
		return nil
	}
	out := make([]string, 0, len(config.EnvPassthrough))
	for _, name := range config.EnvPassthrough {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// renderEnvFile builds the file body and the report. lookup is os.LookupEnv in
// production and a stub in the tests.
func renderEnvFile(config *Config, lookup func(string) (string, bool)) (string, envSyncResult) {
	res := envSyncResult{Path: envFilePath()}
	var body strings.Builder
	body.WriteString("# Written by `ccc env sync`. Values come from the shell that ran it.\n")
	body.WriteString("# Read by the ccc service (EnvironmentFile) and by `ccc listen`. Do not edit by hand:\n")
	body.WriteString("# the next sync overwrites it. Names come from config.json's env_passthrough.\n")
	for _, name := range passthroughNames(config) {
		value, ok := lookup(name)
		if !ok {
			res.Missing = append(res.Missing, name)
			continue
		}
		if strings.ContainsAny(value, "\n\r") {
			// systemd's EnvironmentFile is line-based; a value with a newline in
			// it cannot be written without changing what it means.
			res.Skipped = append(res.Skipped, name)
			continue
		}
		res.Found = append(res.Found, name)
		// systemd (and this file's own reader) accept a double-quoted value with
		// backslashes and quotes escaped.
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		fmt.Fprintf(&body, "%s=\"%s\"\n", name, escaped)
	}
	return body.String(), res
}

// syncEnvFile writes the env file from the current process environment. The
// file is 0600 and is replaced atomically, so a service reading it never sees
// half of it.
func syncEnvFile(config *Config) (envSyncResult, error) {
	body, res := renderEnvFile(config, os.LookupEnv)
	path := res.Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "env.tmp-*")
	if err != nil {
		return res, err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: same
		return res, err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()        // safe-ignore: same
		os.Remove(tmpName) // safe-ignore: same
		return res, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) // safe-ignore: same
		return res, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) // safe-ignore: same
		return res, err
	}
	return res, nil
}

// loadEnvFile puts the env file's values into this process's environment, for
// the names env_passthrough asks for and only where the variable is not set
// already. A value inherited from the shell always wins: the file is a fallback
// for a service that was started without one, not a source of truth.
//
// This is what makes the mechanism work under launchd as well as systemd — a
// plist cannot source a file — and it is why the file is 0600.
func loadEnvFile(config *Config) []string {
	wanted := map[string]bool{}
	for _, name := range passthroughNames(config) {
		wanted[name] = true
	}
	if len(wanted) == 0 {
		return nil
	}
	f, err := os.Open(envFilePath())
	if err != nil {
		return nil // safe-ignore: no file simply means nothing to fall back on
	}
	defer f.Close()

	var loaded []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name, value, ok := parseEnvLine(scanner.Text())
		if !ok || !wanted[name] {
			continue
		}
		if _, already := os.LookupEnv(name); already {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			hookLog("env file: could not set %s: %v", name, err)
			continue
		}
		loaded = append(loaded, name)
	}
	return loaded
}

// parseEnvLine reads one `NAME="value"` line, undoing the quoting renderEnvFile
// applied. Comments and blank lines yield ok=false.
func parseEnvLine(line string) (name, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(line[:eq])
	value = strings.TrimSpace(line[eq+1:])
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		value = unescapeEnvValue(value[1 : len(value)-1])
	}
	return name, value, name != ""
}

// unescapeEnvValue undoes the \\ and \" escaping of renderEnvFile.
func unescapeEnvValue(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '"') {
			i++
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// envPassthroughStatus is what /status shows: which passthrough names the
// running instance can actually see. Names only — a value is never rendered.
func envPassthroughStatus(config *Config) (present, missing []string) {
	for _, name := range passthroughNames(config) {
		if _, ok := os.LookupEnv(name); ok {
			present = append(present, name)
			continue
		}
		missing = append(missing, name)
	}
	return present, missing
}

// runEnvSyncCommand is `ccc env sync`.
func runEnvSyncCommand(args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return fmt.Errorf("usage: ccc env sync   (run it from a login shell: bash -lc 'ccc env sync')")
	}
	config, err := loadConfig()
	if err != nil || config == nil {
		return fmt.Errorf("not configured yet: set env_passthrough first (ccc config set env_passthrough GH_TOKEN,…)")
	}
	res, err := syncEnvFile(config)
	if err != nil {
		return fmt.Errorf("write the env file: %w", err)
	}
	fmt.Print(res.String())
	return nil
}
