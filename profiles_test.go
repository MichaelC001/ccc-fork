package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFixtureProfile writes a profile config dir with the given .claude.json and
// settings.json contents ("" = do not create the file).
func newFixtureProfile(t *testing.T, name, claudeJSON, settingsJSON string) Profile {
	t.Helper()
	dir := t.TempDir()
	if claudeJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(claudeJSON), 0600); err != nil {
			t.Fatalf("write .claude.json: %v", err)
		}
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settingsJSON), 0600); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}
	return Profile{Name: name, ConfigDir: dir}
}

// usageFixture is the shape Claude Code writes; only the fields ccc reads are
// filled in, plus noise to prove unknown fields are ignored.
const usageFixture = `{
  "hasCompletedOnboarding": true,
  "cachedUsageUtilization": {
    "fetchedAtMs": 1788010567241,
    "utilization": {
      "five_hour": {"utilization": 7, "resets_at": "2026-08-29T17:10:00.160901+00:00"},
      "seven_day": {"utilization": 42, "resets_at": "2026-09-03T15:00:00.160920+00:00"},
      "seven_day_opus": null
    }
  }
}`

func TestReadProfileUsage(t *testing.T) {
	t.Run("full cache", func(t *testing.T) {
		p := newFixtureProfile(t, "a", usageFixture, "")
		u := readProfileUsage(p)
		if u.FiveHour != 7 || !u.FiveHourKnown {
			t.Errorf("five_hour = %d (known=%v), want 7 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 42 || !u.SevenDayKnown {
			t.Errorf("seven_day = %d (known=%v), want 42 known", u.SevenDay, u.SevenDayKnown)
		}
		if u.FiveHourResetAt.IsZero() {
			t.Error("five_hour resets_at not parsed")
		}
	})

	t.Run("null utilization is unknown", func(t *testing.T) {
		p := newFixtureProfile(t, "b", `{"cachedUsageUtilization":{"utilization":{"five_hour":null,"seven_day":{"utilization":null}}}}`, "")
		u := readProfileUsage(p)
		if u.FiveHour != unknownUtilization || u.FiveHourKnown {
			t.Errorf("five_hour = %d (known=%v), want %d unknown", u.FiveHour, u.FiveHourKnown, unknownUtilization)
		}
	})

	t.Run("missing file is unknown", func(t *testing.T) {
		u := readProfileUsage(Profile{Name: "c", ConfigDir: filepath.Join(t.TempDir(), "nope")})
		if u.FiveHour != unknownUtilization || u.SevenDay != unknownUtilization {
			t.Errorf("got %+v, want both %d", u, unknownUtilization)
		}
	})

	t.Run("malformed json is unknown", func(t *testing.T) {
		p := newFixtureProfile(t, "d", `{not json`, "")
		if u := readProfileUsage(p); u.FiveHourKnown || u.FiveHour != unknownUtilization {
			t.Errorf("got %+v, want unknown", u)
		}
	})

	t.Run("out-of-range percent is clamped", func(t *testing.T) {
		p := newFixtureProfile(t, "e", `{"cachedUsageUtilization":{"utilization":{"five_hour":{"utilization":250},"seven_day":{"utilization":-3}}}}`, "")
		u := readProfileUsage(p)
		if u.FiveHour != 100 || u.SevenDay != 0 {
			t.Errorf("got 5h=%d 7d=%d, want 100 and 0", u.FiveHour, u.SevenDay)
		}
	})
}

func TestChooseProfile(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		stats []profileStat
		want  string
	}{
		{
			name:  "no profiles",
			stats: nil,
			want:  "",
		},
		{
			name: "lowest five-hour utilization wins",
			stats: []profileStat{
				{Name: "work", FiveHour: 80, SevenDay: 10},
				{Name: "work2", FiveHour: 12, SevenDay: 90},
			},
			want: "work2",
		},
		{
			name: "tie on utilization breaks on fewer working agents",
			stats: []profileStat{
				{Name: "a", FiveHour: 30, WorkingAgents: 4},
				{Name: "b", FiveHour: 30, WorkingAgents: 1},
			},
			want: "b",
		},
		{
			name: "full tie breaks on name",
			stats: []profileStat{
				{Name: "zeta", FiveHour: 30, WorkingAgents: 2},
				{Name: "alpha", FiveHour: 30, WorkingAgents: 2},
			},
			want: "alpha",
		},
		{
			name: "unknown utilization loses to a known-idle profile",
			stats: []profileStat{
				{Name: "known", FiveHour: 5},
				{Name: "unknown", FiveHour: unknownUtilization},
			},
			want: "known",
		},
		{
			name: "unknown utilization beats a known-busy profile",
			stats: []profileStat{
				{Name: "busy", FiveHour: 95},
				{Name: "unknown", FiveHour: unknownUtilization},
			},
			want: "unknown",
		},
		{
			name: "a cooling-down profile is skipped even when idler",
			stats: []profileStat{
				{Name: "limited", FiveHour: 1, CooledUntil: now.Add(10 * time.Minute)},
				{Name: "open", FiveHour: 70},
			},
			want: "open",
		},
		{
			name: "an expired cooldown does not exclude",
			stats: []profileStat{
				{Name: "expired", FiveHour: 1, CooledUntil: now.Add(-time.Minute)},
				{Name: "open", FiveHour: 70},
			},
			want: "expired",
		},
		{
			name: "when every profile is cooling down, take the one freeing up first",
			stats: []profileStat{
				{Name: "late", FiveHour: 1, CooledUntil: now.Add(2 * time.Hour)},
				{Name: "soon", FiveHour: 99, CooledUntil: now.Add(5 * time.Minute)},
			},
			want: "soon",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseProfile(tt.stats, now); got != tt.want {
				t.Errorf("chooseProfile = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChooseProfileIsDeterministic(t *testing.T) {
	now := time.Now()
	stats := []profileStat{
		{Name: "c", FiveHour: 10},
		{Name: "a", FiveHour: 10},
		{Name: "b", FiveHour: 10},
	}
	for i := 0; i < 20; i++ {
		if got := chooseProfile(stats, now); got != "a" {
			t.Fatalf("iteration %d: chooseProfile = %q, want %q", i, got, "a")
		}
	}
}

func TestCountWorking(t *testing.T) {
	snap := profileSnapshot{OK: true, Agents: []AgentInfo{
		{ID: "1", State: "working"},
		{ID: "2", State: "Working"}, // state comparison is case-insensitive
		{ID: "3", State: "done"},
		{ID: "4", State: ""},
	}}
	if got := countWorking(snap); got != 2 {
		t.Errorf("countWorking = %d, want 2", got)
	}
	// A failed snapshot claims nothing rather than reporting a bogus zero-truth.
	if got := countWorking(profileSnapshot{OK: false, Agents: snap.Agents}); got != 0 {
		t.Errorf("countWorking(!OK) = %d, want 0", got)
	}
}

func TestIsUsageLimitSignal(t *testing.T) {
	tests := []struct {
		name string
		js   *jobState
		want bool
	}{
		{"nil", nil, false},
		{"not blocked", &jobState{State: "working", Detail: "usage limit reached"}, false},
		{"blocked with usage limit in detail", &jobState{State: "blocked", Detail: "Usage limit reached"}, true},
		{"blocked with rate limit in needs", &jobState{State: "blocked", Needs: "rate limit — try later"}, true},
		{"blocked with resets in needs", &jobState{State: "blocked", Needs: "limit resets at 5pm"}, true},
		{"blocked for another reason", &jobState{State: "blocked", Detail: "waiting for your answer", Needs: "a decision"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUsageLimitSignal(tt.js); got != tt.want {
				t.Errorf("isUsageLimitSignal = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProfileCooldown(t *testing.T) {
	cooldownMu.Lock()
	cooldowns = map[string]time.Time{}
	cooldownMu.Unlock()

	now := time.Now()
	// A profile with no cached resets_at falls back to the fixed cooldown.
	p := newFixtureProfile(t, "cool", `{}`, "")
	noteProfileLimit(p, now)
	until := profileCooledUntil("cool", now)
	if until.IsZero() {
		t.Fatal("profile should be on cooldown")
	}
	if d := until.Sub(now); d < defaultLimitCooldown-time.Second || d > defaultLimitCooldown+time.Second {
		t.Errorf("cooldown = %v, want ~%v", d, defaultLimitCooldown)
	}
	// Once it has elapsed the entry is forgotten.
	if got := profileCooledUntil("cool", now.Add(defaultLimitCooldown+time.Minute)); !got.IsZero() {
		t.Errorf("expired cooldown = %v, want zero", got)
	}
	if got := profileCooledUntil("never-limited", now); !got.IsZero() {
		t.Errorf("unknown profile cooldown = %v, want zero", got)
	}
}

func TestBypassAccepted(t *testing.T) {
	t.Run("settings.json flag", func(t *testing.T) {
		p := newFixtureProfile(t, "a", "", `{"skipDangerousModePermissionPrompt":true}`)
		if accepted, known := bypassAccepted(p); !accepted || !known {
			t.Errorf("got accepted=%v known=%v, want true true", accepted, known)
		}
	})
	t.Run("legacy .claude.json flag", func(t *testing.T) {
		p := newFixtureProfile(t, "b", `{"bypassPermissionsModeAccepted":true}`, `{}`)
		if accepted, known := bypassAccepted(p); !accepted || !known {
			t.Errorf("got accepted=%v known=%v, want true true", accepted, known)
		}
	})
	t.Run("present but false", func(t *testing.T) {
		p := newFixtureProfile(t, "c", `{}`, `{"skipDangerousModePermissionPrompt":false}`)
		accepted, known := bypassAccepted(p)
		if accepted || !known {
			t.Errorf("got accepted=%v known=%v, want false true", accepted, known)
		}
	})
	t.Run("fresh dir is unknown, not refused", func(t *testing.T) {
		p := Profile{Name: "d", ConfigDir: filepath.Join(t.TempDir(), "fresh")}
		accepted, known := bypassAccepted(p)
		if accepted || known {
			t.Errorf("got accepted=%v known=%v, want false false", accepted, known)
		}
	})
}

func TestListProfilesAndDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Run("no profiles yields the implicit one", func(t *testing.T) {
		all := listProfiles(&Config{})
		if len(all) != 1 || all[0].Name != defaultProfileName || !all[0].Implicit {
			t.Fatalf("got %+v, want a single implicit profile", all)
		}
	})
	t.Run("nil config yields the implicit one", func(t *testing.T) {
		if all := listProfiles(nil); len(all) != 1 || all[0].Name != defaultProfileName {
			t.Fatalf("got %+v, want the implicit profile", all)
		}
	})
	t.Run("configured profiles are sorted", func(t *testing.T) {
		cfg := &Config{Profiles: map[string]*Profile{
			"work2": {ConfigDir: "/tmp/b"},
			"work":  {ConfigDir: "/tmp/a"},
		}}
		all := listProfiles(cfg)
		if len(all) != 2 || all[0].Name != "work" || all[1].Name != "work2" {
			t.Fatalf("got %+v, want [work work2]", all)
		}
	})
	t.Run("default_profile selects, else first by name", func(t *testing.T) {
		cfg := &Config{Profiles: map[string]*Profile{
			"work":  {ConfigDir: "/tmp/a"},
			"work2": {ConfigDir: "/tmp/b"},
		}}
		if got := defaultProfile(cfg).Name; got != "work" {
			t.Errorf("default without default_profile = %q, want work", got)
		}
		cfg.DefaultProfile = "work2"
		if got := defaultProfile(cfg).Name; got != "work2" {
			t.Errorf("default_profile = %q, want work2", got)
		}
		// A dangling default_profile must not break dispatch.
		cfg.DefaultProfile = "gone"
		if got := defaultProfile(cfg).Name; got != "work" {
			t.Errorf("dangling default_profile = %q, want fallback work", got)
		}
	})
}

func TestProfileForSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Profiles:       map[string]*Profile{"work": {ConfigDir: "/tmp/a"}, "work2": {ConfigDir: "/tmp/b"}},
		DefaultProfile: "work",
	}
	// Empty Profile means "the default" — this is what keeps configs written
	// before multi-profile support valid with no migration.
	if got := profileFor(cfg, &SessionInfo{}).Name; got != "work" {
		t.Errorf("empty profile = %q, want work", got)
	}
	if got := profileFor(cfg, &SessionInfo{Profile: "work2"}).Name; got != "work2" {
		t.Errorf("explicit profile = %q, want work2", got)
	}
	// A profile deleted behind a session's back must not lose the session.
	if got := profileFor(cfg, &SessionInfo{Profile: "removed"}).Name; got != "work" {
		t.Errorf("dangling profile = %q, want work", got)
	}
	if got := profileFor(cfg, nil).Name; got != "work" {
		t.Errorf("nil session = %q, want work", got)
	}
}

func TestSessionProfileName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Profiles:       map[string]*Profile{"work": {ConfigDir: "/tmp/a"}, "work2": {ConfigDir: "/tmp/b"}},
		DefaultProfile: "work",
	}
	// The default profile is stored as "" so single-account configs stay
	// byte-identical to what ccc wrote before profiles existed.
	if got := sessionProfileName(cfg, Profile{Name: "work"}); got != "" {
		t.Errorf("default profile stored as %q, want empty", got)
	}
	if got := sessionProfileName(cfg, Profile{Name: "work2"}); got != "work2" {
		t.Errorf("non-default profile stored as %q, want work2", got)
	}
}

func TestClaudeEnvScrubsInheritedState(t *testing.T) {
	// The leak this guards against: ccc started from inside a Claude Code
	// session inherits these, and a child `claude` then authenticates and
	// behaves as that parent session instead of as the profile.
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leaked.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_SCOPES", "leaked")
	t.Setenv("CLAUDE_CODE_MESSAGING_URL", "leaked")
	t.Setenv("CLAUDE_CODE_SDK_VERSION", "leaked")
	t.Setenv("SOME_RANDOM_VAR", "leaked")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/test")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/501")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")

	env := claudeEnv(Profile{Name: "work2", ConfigDir: "/tmp/claude-b"})
	got := map[string]string{}
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				got[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for _, banned := range []string{
		"CLAUDECODE", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_SCOPES", "CLAUDE_CODE_MESSAGING_URL",
		"CLAUDE_CODE_SDK_VERSION", "SOME_RANDOM_VAR",
	} {
		if v, ok := got[banned]; ok {
			t.Errorf("%s leaked into the child env as %q", banned, v)
		}
	}
	for k, want := range map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/test",
		"LC_ALL": "en_US.UTF-8", "XDG_RUNTIME_DIR": "/run/user/501",
		"SSH_AUTH_SOCK": "/tmp/agent.sock",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["CLAUDE_CONFIG_DIR"] != "/tmp/claude-b" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want /tmp/claude-b", got["CLAUDE_CONFIG_DIR"])
	}
}

func TestClaudeEnvOmitsConfigDirForImplicitProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/test")
	initProfiles()
	p := implicitProfile()
	if !p.Implicit {
		t.Fatalf("expected an implicit profile, got %+v", p)
	}
	for _, kv := range claudeEnv(p) {
		if len(kv) >= 18 && kv[:18] == "CLAUDE_CONFIG_DIR=" {
			t.Errorf("implicit profile must not pin CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}

	// When ccc itself was started with a dir, children must inherit it.
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/dir")
	initProfiles()
	p = implicitProfile()
	if p.Implicit || p.ConfigDir != "/custom/dir" {
		t.Fatalf("got %+v, want an explicit /custom/dir profile", p)
	}
	found := false
	for _, kv := range claudeEnv(p) {
		if kv == "CLAUDE_CONFIG_DIR=/custom/dir" {
			found = true
		}
	}
	if !found {
		t.Error("inherited CLAUDE_CONFIG_DIR was not passed through")
	}
	// Leave the package-level capture as the test process found it.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
}

func TestProfilePathsAreScopedToTheConfigDir(t *testing.T) {
	p := Profile{Name: "work2", ConfigDir: "/tmp/claude-b"}
	cases := map[string]string{
		profileJobsDir(p):      "/tmp/claude-b/jobs",
		profileProjectsDir(p):  "/tmp/claude-b/projects",
		profileSessionNames(p): "/tmp/claude-b/session-names",
		profileClaudeJSON(p):   "/tmp/claude-b/.claude.json",
		profileSettings(p):     "/tmp/claude-b/settings.json",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestProfileClaudeJSONForImplicitProfile pins the one path that is NOT inside
// the config dir: with CLAUDE_CONFIG_DIR unset, .claude.json sits beside
// ~/.claude rather than in it.
func TestProfileClaudeJSONForImplicitProfile(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
	defer initProfiles()

	imp := implicitProfile()
	if got, want := profileClaudeJSON(imp), "/home/test/.claude.json"; got != want {
		t.Errorf("implicit .claude.json = %q, want %q", got, want)
	}
	if got, want := profileSettings(imp), "/home/test/.claude/settings.json"; got != want {
		t.Errorf("implicit settings.json = %q, want %q", got, want)
	}
	// An explicitly configured dir keeps its own copy, even when it is ~/.claude.
	explicit := Profile{Name: "same", ConfigDir: "/home/test/.claude"}
	if got, want := profileClaudeJSON(explicit), "/home/test/.claude/.claude.json"; got != want {
		t.Errorf("explicit .claude.json = %q, want %q", got, want)
	}
}

// TestEmptyConfigDirMeansClaudeDefault pins the distinction between an empty
// config_dir ("leave CLAUDE_CONFIG_DIR unset") and one spelled out as
// ~/.claude: setting the variable makes claude start a fresh <dir>/.claude.json
// instead of using ~/.claude.json, so `ccc profile add` must never pin it.
func TestEmptyConfigDirMeansClaudeDefault(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
	defer initProfiles()

	cfg := &Config{Profiles: map[string]*Profile{
		"default": {Label: "claude's default config dir"},
		"work2":   {ConfigDir: "/tmp/claude-b"},
	}}
	all := listProfiles(cfg)
	if len(all) != 2 {
		t.Fatalf("got %d profiles, want 2", len(all))
	}
	def, other := all[0], all[1]
	if def.Name != "default" || !def.Implicit {
		t.Fatalf("default = %+v, want an implicit profile", def)
	}
	if def.ConfigDir != "/home/test/.claude" {
		t.Errorf("default dir = %q, want /home/test/.claude", def.ConfigDir)
	}
	if profileClaudeJSON(def) != "/home/test/.claude.json" {
		t.Errorf("default .claude.json = %q, want the legacy location", profileClaudeJSON(def))
	}
	for _, kv := range claudeEnv(def) {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("empty config_dir must not pin CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}
	if other.Implicit {
		t.Errorf("work2 = %+v, want an explicit profile", other)
	}
}

// TestIsStaleTokenError pins the mapping from Claude Code's org-flavoured
// wording to the action that actually fixes it (a fresh login).
func TestIsStaleTokenError(t *testing.T) {
	blocked := "org disabled OAuth — use API key or ask admin · Your organization has " +
		"disabled Claude subscription access for Claude Code · Use an Anthropic API key instead"
	if !isStaleTokenError(blocked) {
		t.Error("the observed blocked-job text should be recognised as a stale token")
	}
	if isStaleTokenError("waiting for your answer") {
		t.Error("an ordinary block must not be reported as a stale token")
	}
	if isStaleTokenError("") {
		t.Error("empty reason must not be reported as a stale token")
	}
}
