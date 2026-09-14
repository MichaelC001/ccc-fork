package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file implements the "background agent" backend that replaces the old
// tmux-based session model. Sessions are now real Claude Code background agents
// managed by the local `claude` daemon, so they show up in `claude agents`
// (the fleet view). ccc becomes a Telegram mirror of that fleet.
//
// Primitives:
//   - dispatchAgent: `claude --bg ...`      → start a new bg session, return its id
//   - resumeAgent:   `claude --resume ...`   → send a follow-up message (stop+resume)
//   - stopAgent:     `claude stop <id>`      → stop a bg session (keeps conversation)
//   - listAgents:    `claude agents --json`  → snapshot of the fleet + state
//   - attach is done directly from the terminal with `claude attach <id>`

// AgentInfo mirrors one entry of `claude agents --json --all`.
type AgentInfo struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`        // short daemon id (changes on every resume)
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`      // "background"
	StartedAt int64  `json:"startedAt"` // unix millis
	SessionID string `json:"sessionId"` // full conversation UUID
	Name      string `json:"name"`      // display name (-n/--name)
	Status    string `json:"status"`    // "idle" | "busy"
	State     string `json:"state"`     // "working" | "done" | ... | ""
}

// jobState mirrors ~/.claude/jobs/<shortID>/state.json
type jobState struct {
	State        string `json:"state"`
	Detail       string `json:"detail"`
	Needs        string `json:"needs"`
	Tempo        string `json:"tempo"`
	SessionID    string `json:"sessionId"`
	LinkScanPath string `json:"linkScanPath"`
	Intent       string `json:"intent"`
	Tokens       int    `json:"tokens"`
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var attachRe = regexp.MustCompile(`attach\s+([0-9a-f]{6,})`)

func claudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	// common install location
	cand := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return "claude"
}

// runClaudeJSON runs a read-only `claude` subcommand under a profile's scrubbed
// environment and returns its stdout. Every claude exec in ccc goes through
// claudeEnv — see envWhitelist for why inheriting os.Environ() is unsafe.
func runClaudeJSON(p Profile, timeout time.Duration, args ...string) ([]byte, error) {
	out, err := runClaudeOutput(p, timeout, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runClaudeOutput is runClaudeJSON's underlying form: it hands back stdout even
// when the command exits non-zero, because some claude subcommands report state
// through the exit code while still printing valid JSON (`auth status --json`
// exits 1 for a logged-out config dir). Callers that can read the payload
// should prefer it over the error.
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

// listAgents returns the fleet snapshot for one profile. includeDone controls
// --all. The fleet is per config dir: a profile only ever sees its own
// sessions, and a config dir that has never been used returns [].
//
// `claude agents` without a TTY refuses unless --json, which is the documented
// stable interface (unlike the job state files).
func listAgents(p Profile, includeDone bool) ([]AgentInfo, error) {
	args := []string{"agents", "--json"}
	if includeDone {
		args = append(args, "--all")
	}
	out, err := runClaudeJSON(p, 30*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var agents []AgentInfo
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, fmt.Errorf("cannot parse agents json: %w", err)
	}
	return agents, nil
}

// profileSnapshot is one profile's fleet listing for a single poll tick. OK
// distinguishes "this profile has no sessions" from "we could not ask" — the
// difference between a session that was dismissed and one whose daemon was
// briefly unreachable, i.e. between reaping a topic and leaving it alone.
type profileSnapshot struct {
	Agents []AgentInfo
	OK     bool
}

// snapshotAllProfiles lists every profile's fleet, keyed by profile name. A
// failure for one profile never hides the others: its entry is simply !OK.
func snapshotAllProfiles(config *Config) map[string]profileSnapshot {
	snaps := map[string]profileSnapshot{}
	for _, p := range listProfiles(config) {
		agents, err := listAgents(p, true)
		if err != nil {
			snaps[p.Name] = profileSnapshot{OK: false}
			continue
		}
		snaps[p.Name] = profileSnapshot{Agents: agents, OK: true}
	}
	return snaps
}

// agentByID finds an agent by its short daemon id.
func agentByID(agents []AgentInfo, shortID string) (*AgentInfo, bool) {
	for i := range agents {
		if agents[i].ID == shortID {
			return &agents[i], true
		}
	}
	return nil, false
}

// agentBySessionID finds an agent by its full conversation UUID.
func agentBySessionID(agents []AgentInfo, sessionID string) (*AgentInfo, bool) {
	if sessionID == "" {
		return nil, false
	}
	for i := range agents {
		if agents[i].SessionID == sessionID {
			return &agents[i], true
		}
	}
	return nil, false
}

// parseBgShortID extracts the short id printed by `claude --bg`.
// Banner looks like: "backgrounded · <id> · <name>\n  claude attach <id> ...".
func parseBgShortID(out string) string {
	clean := ansiRe.ReplaceAllString(out, "")
	if m := attachRe.FindStringSubmatch(clean); len(m) == 2 {
		return m[1]
	}
	// Fallback: "backgrounded · <id> · name"
	for _, line := range strings.Split(clean, "\n") {
		if strings.Contains(line, "backgrounded") {
			parts := strings.Split(line, "·")
			if len(parts) >= 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// ccSettingsJSON builds the inline --settings payload that installs a
// PreToolUse hook for AskUserQuestion, scoped to ccc's agents only (so we never
// touch the user's global ~/.claude/settings.json). When Claude is about to ask
// a question, the hook (`ccc hook-question`) renders the options as Telegram
// buttons and blocks until the user taps one — see hooks.go.
func ccSettingsJSON() string {
	cmd := cccPath + " hook-question"
	b, _ := json.Marshal(map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "AskUserQuestion",
					"hooks": []any{
						map[string]any{"type": "command", "command": cmd},
					},
				},
			},
		},
	})
	return string(b)
}

// dispatchArgs builds the argv for a `claude --bg` invocation.
func dispatchArgs(name, resumeID, prompt string) []string {
	args := []string{"--bg", "--dangerously-skip-permissions", "--settings", ccSettingsJSON()}
	if name != "" {
		args = append(args, "--name", name)
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	args = append(args, prompt)
	return args
}

// dispatchAgent starts a new background Claude session in workDir with an
// initial prompt. Returns the short daemon id.
func dispatchAgent(p Profile, name, workDir, prompt string) (string, error) {
	return runDispatch(p, name, workDir, "", prompt)
}

// resumeAgent sends a follow-up message to an existing conversation. Because
// background agents cannot be injected live, this stops the current bg job (a
// still-resident idle agent still counts as "running") and relaunches with
// --resume carrying the new user message. Returns the NEW short daemon id.
//
// stopShortID is the current live bg job to stop (empty if already settled).
// resumeID MUST be the stable conversation UUID, not the short id: after the bg
// job is stopped, `--resume <shortId>` fails with "source session not found",
// whereas `--resume <uuid>` reloads the conversation from disk.
//
// It waits for the previous session to fully stop before resuming: dispatching
// --resume while the old worker is still alive makes claude report "<id> is
// currently running as a background agent" and the worker crashes/respawns.
func resumeAgent(p Profile, stopShortID, resumeID, name, workDir, prompt string) (string, error) {
	if stopShortID != "" {
		_ = stopAgent(p, stopShortID) // safe-ignore: best-effort stop before relaunch; a failure surfaces on the resume itself
		waitAgentStopped(p, stopShortID, 8*time.Second)
	}
	return runDispatch(p, name, workDir, resumeID, prompt)
}

// waitAgentStopped blocks until the given short id is no longer actively
// running (gone from the fleet, or settled to a terminal state), so a
// subsequent --resume does not race the still-alive worker.
func waitAgentStopped(p Profile, shortID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		agents, err := listAgents(p, true)
		if err == nil {
			a, ok := agentByID(agents, shortID)
			if !ok {
				return // gone entirely
			}
			st := strings.ToLower(a.State)
			if a.Status != "busy" && st != "working" {
				return // settled (done/failed/idle)
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func runDispatch(p Profile, name, workDir, resumeID, prompt string) (string, error) {
	cmd := exec.Command(claudeBin(), dispatchArgs(name, resumeID, prompt)...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = claudeEnv(p)
	out, err := cmd.CombinedOutput()
	clean := strings.TrimSpace(ansiRe.ReplaceAllString(string(out), ""))
	// The bypass-permissions disclaimer must be accepted once per config dir or
	// --bg refuses to launch. Surface the actionable instruction: the caller
	// reports it to the topic instead of failing with an opaque exit code.
	if strings.Contains(clean, "requires accepting the disclaimer") {
		return "", fmt.Errorf("profile %q: %s %s", p.Name, bypassDisclaimerMsg, bypassDisclaimerHint(p))
	}
	if err != nil {
		return "", fmt.Errorf("claude --bg failed: %w (%s)", err, clean)
	}
	shortID := parseBgShortID(string(out))
	if shortID == "" {
		return "", fmt.Errorf("could not parse background session id from: %s", clean)
	}
	return shortID, nil
}

// stopAgent stops a background session (its conversation is kept).
func stopAgent(p Profile, shortID string) error {
	if shortID == "" {
		return nil
	}
	cmd := exec.Command(claudeBin(), "stop", shortID)
	cmd.Env = claudeEnv(p)
	return cmd.Run()
}

// resolveSessionUUID resolves the full conversation UUID for a short id by
// polling the fleet snapshot (the job state file is not populated immediately).
func resolveSessionUUID(p Profile, shortID string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if agents, err := listAgents(p, true); err == nil {
			if a, ok := agentByID(agents, shortID); ok && a.SessionID != "" {
				return a.SessionID
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readJobState reads <config_dir>/jobs/<shortID>/state.json (best effort).
// This file is explicitly NOT a stable interface — `claude agents --json` is.
// Keep it as a fallback only, for the detail/needs strings the JSON listing
// does not carry.
func readJobState(p Profile, shortID string) *jobState {
	if shortID == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(profileJobsDir(p), shortID, "state.json"))
	if err != nil {
		return nil
	}
	var js jobState
	if json.Unmarshal(data, &js) != nil {
		return nil
	}
	// Derive sessionId from linkScanPath if the explicit field is empty.
	if js.SessionID == "" && js.LinkScanPath != "" {
		base := filepath.Base(js.LinkScanPath)
		js.SessionID = strings.TrimSuffix(base, ".jsonl")
	}
	return &js
}

// transcriptPathForUUID locates the JSONL transcript for a conversation UUID
// inside a profile: <config_dir>/projects/<cwd-slug>/<uuid>.jsonl. Since
// 2.1.259 there is also a sidecar DIRECTORY <uuid>/ next to each file, so the
// glob must stay anchored on the .jsonl suffix.
func transcriptPathForUUID(p Profile, uuid string) string {
	if uuid == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(profileProjectsDir(p), "*", uuid+".jsonl")) // safe-ignore: Glob only fails on a malformed pattern; ours is built from a UUID
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

var cccMarkerRe = regexp.MustCompile(`ccc-session:t(\d+)`)

// transcriptTopicMarker scans a conversation transcript for the ccc marker
// (see cccMarker) and returns the Telegram TopicID it encodes, or 0 if none.
// The marker survives resume because it lives in the conversation's message
// history, so this re-links a resumed agent (new UUID/short) to its session.
func transcriptTopicMarker(transcriptPath string) int64 {
	if transcriptPath == "" {
		return 0
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return 0
	}
	// Use the LAST marker: if a topic was deleted and the conversation later
	// re-tagged with a new topic id, the most recent one is authoritative.
	ms := cccMarkerRe.FindAllSubmatch(data, -1)
	if len(ms) == 0 {
		return 0
	}
	var id int64
	_, _ = fmt.Sscanf(string(ms[len(ms)-1][1]), "%d", &id) // safe-ignore: the regex already guarantees digits; id stays 0 on failure
	return id
}

// classifyState maps daemon state/status fields to a coarse ccc status.
// Returns one of: "working", "needs_input", "done", "failed".
func classifyState(state, status string) string {
	l := strings.ToLower(state)
	switch l {
	case "working":
		return "working"
	case "done":
		return "done"
	case "failed", "error":
		return "failed"
	case "":
		if status == "idle" {
			return "done"
		}
		return "working"
	}
	if strings.Contains(l, "input") || strings.Contains(l, "block") ||
		strings.Contains(l, "wait") || strings.Contains(l, "question") {
		return "needs_input"
	}
	return "working"
}
