package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Multi-profile support: one ccc can drive several Claude accounts at once.
//
// A profile is just a CLAUDE_CONFIG_DIR. That env var fully scopes a Claude
// Code installation: credentials (the macOS Keychain service name gets a hash
// suffix derived from the dir), .claude.json, projects/ and settings.json all
// move with it. Everything ccc reads off disk or asks the CLI for must
// therefore be addressed per profile.
//
// v3 deliberately points every profile's projects/ at the SAME shared
// directory (DESIGN §4), so a turn that fails over to another account can
// resume the same conversation UUID.
//
// Verified against Claude Code 2.1.270.

// defaultProfileName is the name of the synthesized profile used when the user
// has not configured any. It keeps single-account setups working unchanged.
const defaultProfileName = "default"

// Profile is one Claude account sandbox.
type Profile struct {
	// Name is the config-map key; it is not stored inside the object.
	Name string `json:"-"`
	// ConfigDir is the value of CLAUDE_CONFIG_DIR for this profile. An EMPTY
	// config_dir means "whatever claude uses by default" — ccc then passes no
	// CLAUDE_CONFIG_DIR at all. That distinction matters: setting the variable
	// to ~/.claude is NOT the same as leaving it unset, because claude then
	// starts a fresh <dir>/.claude.json instead of using ~/.claude.json.
	ConfigDir string `json:"config_dir"`
	// Label is a human hint (usually the account email). Cosmetic only.
	Label string `json:"label,omitempty"`
	// Implicit marks the profile synthesized for a config without any
	// `profiles` block. For it ccc passes no CLAUDE_CONFIG_DIR at all (unless
	// the var was already set in ccc's own environment at startup), so a
	// single-account install behaves exactly as it did before profiles existed.
	Implicit bool `json:"-"`
}

// inheritedConfigDir is $CLAUDE_CONFIG_DIR as it was when ccc started, captured
// before anything scrubs the environment. "" means the user did not set one.
var inheritedConfigDir string

func initProfiles() {
	inheritedConfigDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
}

// implicitProfile is the profile used when no `profiles` block is configured:
// $CLAUDE_CONFIG_DIR if ccc inherited one, else ~/.claude.
func implicitProfile() Profile {
	dir := inheritedConfigDir
	implicit := true
	if dir == "" {
		home, _ := os.UserHomeDir() // safe-ignore: an empty home yields a relative .claude path, which is the least-bad fallback here
		dir = filepath.Join(home, ".claude")
	} else {
		// ccc was started with an explicit dir — pass it through to children.
		implicit = false
	}
	return Profile{Name: defaultProfileName, ConfigDir: dir, Implicit: implicit}
}

// listProfiles returns every configured profile sorted by name, or the single
// implicit profile when none are configured.
func listProfiles(config *Config) []Profile {
	if config == nil || len(config.Profiles) == 0 {
		return []Profile{implicitProfile()}
	}
	names := make([]string, 0, len(config.Profiles))
	for name := range config.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		p := config.Profiles[name]
		if p == nil {
			continue
		}
		if strings.TrimSpace(p.ConfigDir) == "" {
			// Explicitly registered, but pinned to claude's own default layout.
			imp := implicitProfile()
			out = append(out, Profile{Name: name, ConfigDir: imp.ConfigDir, Label: p.Label, Implicit: imp.Implicit})
			continue
		}
		out = append(out, Profile{Name: name, ConfigDir: expandPath(p.ConfigDir), Label: p.Label})
	}
	if len(out) == 0 {
		return []Profile{implicitProfile()}
	}
	return out
}

// profileByName resolves a profile by name. An empty name means "the default".
func profileByName(config *Config, name string) (Profile, bool) {
	if name == "" {
		return defaultProfile(config), true
	}
	for _, p := range listProfiles(config) {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// defaultProfile is the profile new sessions fall back to: config.DefaultProfile
// when it resolves, else the first profile by name.
func defaultProfile(config *Config) Profile {
	all := listProfiles(config)
	if config != nil && config.DefaultProfile != "" {
		for _, p := range all {
			if p.Name == config.DefaultProfile {
				return p
			}
		}
	}
	return all[0]
}

// claudeHome is the on-disk root of a profile: the directory that holds
// .claude.json, settings.json and projects/.
func claudeHome(p Profile) string {
	if p.ConfigDir != "" {
		return p.ConfigDir
	}
	return implicitProfile().ConfigDir
}

func profileProjectsDir(p Profile) string { return filepath.Join(claudeHome(p), "projects") }
func profileSettings(p Profile) string    { return filepath.Join(claudeHome(p), "settings.json") }

// profileClaudeJSON is the odd one out. Everything else lives INSIDE the config
// dir, but .claude.json only moves in when CLAUDE_CONFIG_DIR is actually set:
//
//	CLAUDE_CONFIG_DIR unset → ~/.claude.json   (beside ~/.claude, not in it)
//	CLAUDE_CONFIG_DIR=X     → X/.claude.json
//
// Verified on 2.1.259: running any claude command with CLAUDE_CONFIG_DIR
// pointed at ~/.claude creates a SECOND, empty ~/.claude/.claude.json rather
// than reusing ~/.claude.json. Getting this wrong makes every profile's usage
// cache read as "unknown".
func profileClaudeJSON(p Profile) string {
	home, _ := os.UserHomeDir() // safe-ignore: an empty home yields a relative path, the same least-bad fallback as implicitProfile
	legacy := filepath.Join(home, ".claude.json")
	if p.Implicit {
		return legacy
	}
	inDir := filepath.Join(claudeHome(p), ".claude.json")
	// A profile registered against the default ~/.claude only grows its own
	// .claude.json once claude next runs with CLAUDE_CONFIG_DIR set. Until then
	// the real config is still the legacy one, and reading the in-dir path
	// would report every usage number as unknown.
	if _, err := os.Stat(inDir); err != nil && claudeHome(p) == filepath.Join(home, ".claude") {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return inDir
}

// envWhitelist is the exact set of variables a child `claude` may inherit.
//
// This is a whitelist, never a filter, because environment leakage is real:
// when ccc is itself started from inside a Claude Code session (or the desktop
// app) the parent exports CLAUDECODE=1, ANTHROPIC_BASE_URL,
// CLAUDE_CODE_OAUTH_SCOPES, CLAUDE_CODE_MESSAGING_*, CLAUDE_CODE_SDK_* and
// friends. A child `claude` inherits them and then authenticates/behaves
// differently — we observed `claude -p` failing with an org-policy error purely
// because of inherited env. No parent CLAUDE*/ANTHROPIC* variable is ever
// passed through; the only one ccc sets is CLAUDE_CONFIG_DIR.
var envWhitelist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "TMPDIR", "TZ", "TERM", "SSH_AUTH_SOCK",
}

// envWhitelistPrefixes are variable name prefixes kept wholesale.
var envWhitelistPrefixes = []string{"LC_", "XDG_"}

// claudeEnv builds the environment for every child `claude` process: the
// whitelist above plus this profile's CLAUDE_CONFIG_DIR. Every exec.Command
// that runs claude MUST use it — see envWhitelist for why.
func claudeEnv(p Profile) []string {
	keep := map[string]bool{}
	for _, k := range envWhitelist {
		keep[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if keep[name] {
			env = append(env, kv)
			continue
		}
		for _, prefix := range envWhitelistPrefixes {
			if strings.HasPrefix(name, prefix) {
				env = append(env, kv)
				break
			}
		}
	}
	// The implicit profile with no inherited dir means "whatever claude does by
	// default" — setting the var would pin a path claude might resolve
	// differently (and would defeat the point of leaving it alone).
	if !p.Implicit && p.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+p.ConfigDir)
	}
	return env
}

// ---------------------------------------------------------------------------
// Profile selection for new sessions
// ---------------------------------------------------------------------------

// unknownUtilization is the assumed utilization when a profile's cache has no
// usable number: pessimistic enough not to be picked over a known-idle account,
// optimistic enough not to be starved by a known-busy one.
const unknownUtilization = 50

// defaultLimitCooldown is how long a profile is skipped after a usage/rate
// limit signal when its cache carries no resets_at to aim at.
const defaultLimitCooldown = 30 * time.Minute

// usageCache mirrors the parts of <config_dir>/.claude.json ccc reads.
// cachedUsageUtilization is written by Claude Code itself; it may be missing,
// stale, or partially null, so every field is treated as best-effort.
type usageCache struct {
	CachedUsageUtilization struct {
		FetchedAtMs int64 `json:"fetchedAtMs"`
		Utilization struct {
			FiveHour usageWindow `json:"five_hour"`
			SevenDay usageWindow `json:"seven_day"`
		} `json:"utilization"`
	} `json:"cachedUsageUtilization"`
}

type usageWindow struct {
	Utilization *int   `json:"utilization"` // 0-100, nil when unknown
	ResetsAt    string `json:"resets_at"`   // RFC3339, "" when unknown
}

// profileUsage is the digested usage snapshot for one profile.
type profileUsage struct {
	FiveHour        int // 0-100, unknownUtilization when unavailable
	SevenDay        int // 0-100, unknownUtilization when unavailable
	FiveHourKnown   bool
	SevenDayKnown   bool
	FiveHourResetAt time.Time // zero when unknown
}

// readProfileUsage reads a profile's cached usage utilization. Missing or
// unparseable data yields unknownUtilization rather than an error: selection
// must never block on a cold cache.
func readProfileUsage(p Profile) profileUsage {
	u := profileUsage{FiveHour: unknownUtilization, SevenDay: unknownUtilization}
	data, err := os.ReadFile(profileClaudeJSON(p))
	if err != nil {
		return u
	}
	var c usageCache
	if json.Unmarshal(data, &c) != nil {
		return u
	}
	util := c.CachedUsageUtilization.Utilization
	if util.FiveHour.Utilization != nil {
		u.FiveHour = clampPercent(*util.FiveHour.Utilization)
		u.FiveHourKnown = true
	}
	if util.SevenDay.Utilization != nil {
		u.SevenDay = clampPercent(*util.SevenDay.Utilization)
		u.SevenDayKnown = true
	}
	if util.FiveHour.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, util.FiveHour.ResetsAt); err == nil {
			u.FiveHourResetAt = t
		}
	}
	return u
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// profileStat is the full input the selection policy needs for one profile.
// Keeping it a plain value makes chooseProfile a pure, unit-testable function.
type profileStat struct {
	Name          string
	FiveHour      int
	SevenDay      int
	WorkingAgents int
	CooledUntil   time.Time // zero = available
}

// chooseProfile picks the profile a new session should run under: the lowest
// five-hour utilization wins, ties break on fewer working agents, then on name
// (so the choice is deterministic). Profiles still inside a rate-limit cooldown
// are excluded — unless every profile is cooling down, in which case the one
// whose cooldown ends soonest is used rather than refusing to dispatch.
func chooseProfile(stats []profileStat, now time.Time) string {
	if len(stats) == 0 {
		return ""
	}
	var open []profileStat
	for _, s := range stats {
		if s.CooledUntil.IsZero() || !s.CooledUntil.After(now) {
			open = append(open, s)
		}
	}
	if len(open) == 0 {
		best := stats[0]
		for _, s := range stats[1:] {
			if s.CooledUntil.Before(best.CooledUntil) ||
				(s.CooledUntil.Equal(best.CooledUntil) && s.Name < best.Name) {
				best = s
			}
		}
		return best.Name
	}
	best := open[0]
	for _, s := range open[1:] {
		if betterProfile(s, best) {
			best = s
		}
	}
	return best.Name
}

func betterProfile(a, b profileStat) bool {
	if a.FiveHour != b.FiveHour {
		return a.FiveHour < b.FiveHour
	}
	if a.WorkingAgents != b.WorkingAgents {
		return a.WorkingAgents < b.WorkingAgents
	}
	return a.Name < b.Name
}

// ---------------------------------------------------------------------------
// Rate-limit cooldowns
// ---------------------------------------------------------------------------

var (
	cooldownMu sync.Mutex
	cooldowns  = map[string]time.Time{} // profile name -> available again at
)

// noteProfileLimit puts a profile on cooldown after a limit signal: until its
// cached five-hour reset time when we have one, else defaultLimitCooldown.
func noteProfileLimit(p Profile, now time.Time) {
	until := now.Add(defaultLimitCooldown)
	if r := readProfileUsage(p).FiveHourResetAt; r.After(now) {
		until = r
	}
	cooldownMu.Lock()
	if cur, ok := cooldowns[p.Name]; !ok || until.After(cur) {
		cooldowns[p.Name] = until
	}
	cooldownMu.Unlock()
	hookLog("profile %s on usage cooldown until %s", p.Name, until.Format(time.RFC3339))
}

// profileCooledUntil returns when a profile becomes available again (zero when
// it is available now).
func profileCooledUntil(name string, now time.Time) time.Time {
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	until, ok := cooldowns[name]
	if !ok {
		return time.Time{}
	}
	if !until.After(now) {
		delete(cooldowns, name)
		return time.Time{}
	}
	return until
}

// collectProfileStats gathers the selection inputs for every profile. working
// is the number of turns currently running on each profile (DESIGN §4's
// "fewer working bots" tie-break); a nil map just means that input is unknown
// this round, which only affects ties.
func collectProfileStats(config *Config, working map[string]int, now time.Time) []profileStat {
	var stats []profileStat
	for _, p := range listProfiles(config) {
		u := readProfileUsage(p)
		stats = append(stats, profileStat{
			Name:          p.Name,
			FiveHour:      u.FiveHour,
			SevenDay:      u.SevenDay,
			WorkingAgents: working[p.Name],
			CooledUntil:   profileCooledUntil(p.Name, now),
		})
	}
	return stats
}

// ---------------------------------------------------------------------------
// Disclaimer / login state
// ---------------------------------------------------------------------------

// bypassAccepted reports whether a profile has accepted the bypass-permissions
// disclaimer, which `claude --bg --dangerously-skip-permissions` requires once
// per config dir (2.1.259 refuses the launch otherwise, see bypassDisclaimerMsg).
//
// Acceptance is recorded as `skipDangerousModePermissionPrompt: true` in the
// profile's settings.json; older installs recorded
// `bypassPermissionsModeAccepted` in .claude.json and Claude Code migrates that
// forward on startup, so both are accepted as proof. Returns ok=false when
// neither file can be read — "unknown", not "not accepted".
func bypassAccepted(p Profile) (accepted bool, ok bool) {
	readable := false
	if data, err := os.ReadFile(profileSettings(p)); err == nil {
		readable = true
		var s struct {
			Skip *bool `json:"skipDangerousModePermissionPrompt"`
		}
		if json.Unmarshal(data, &s) == nil && s.Skip != nil && *s.Skip {
			return true, true
		}
	}
	if data, err := os.ReadFile(profileClaudeJSON(p)); err == nil {
		readable = true
		var c struct {
			Accepted *bool `json:"bypassPermissionsModeAccepted"`
		}
		if json.Unmarshal(data, &c) == nil && c.Accepted != nil && *c.Accepted {
			return true, true
		}
	}
	return false, readable
}

// bypassDisclaimerMsg is the exact refusal Claude Code 2.1.259 prints when a
// config dir has not accepted the disclaimer yet (extracted from the binary).
const bypassDisclaimerMsg = "--bg with bypassPermissions requires accepting the disclaimer first. " +
	"Run `claude --dangerously-skip-permissions` once interactively."

// bypassDisclaimerHint tells the user how to accept the disclaimer for a
// specific profile.
func bypassDisclaimerHint(p Profile) string {
	if p.Implicit {
		return "Run `claude --dangerously-skip-permissions` once interactively."
	}
	return fmt.Sprintf("Run `CLAUDE_CONFIG_DIR=%s claude --dangerously-skip-permissions` once interactively.", p.ConfigDir)
}

// isDisclaimerRefusal reports whether a dispatch error is the disclaimer gate,
// so callers can surface the actionable instruction instead of a raw failure.
func isDisclaimerRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "requires accepting the disclaimer")
}

// staleTokenMsg is the error Claude Code reports when a profile's OAuth token
// has gone stale. It reads like an org policy decision, but we have seen it
// resolve with nothing but a fresh `claude auth login` on the same account —
// so ccc reports it as "re-login needed", not as "your admin blocked you".
const staleTokenMsg = "organization has disabled Claude subscription access"

// isStaleTokenError reports whether a failure is the stale-token symptom above.
func isStaleTokenError(s string) bool {
	return strings.Contains(s, staleTokenMsg)
}

// profileLoggedIn asks the CLI whether a profile is authenticated. It uses the
// scrubbed environment so the answer is about the profile's own credentials and
// not about whatever OAuth state ccc's parent process leaked in.
func profileLoggedIn(p Profile) (loggedIn bool, account string, err error) {
	// `auth status --json` exits 1 for a logged-out config dir while still
	// printing its JSON, so the payload is authoritative and the exit code is
	// only a fallback for "the command did not run at all".
	out, runErr := runClaudeOutput(p, 15*time.Second, "auth", "status", "--json")
	var st struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
		Account  string `json:"account"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		if runErr != nil {
			return false, "", runErr
		}
		return false, "", fmt.Errorf("cannot parse auth status: %w", err)
	}
	acct := st.Email
	if acct == "" {
		acct = st.Account
	}
	return st.LoggedIn, acct, nil
}
