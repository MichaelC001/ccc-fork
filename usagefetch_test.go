package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const liveUsageJSON = `{
  "five_hour": {"utilization": 2.0, "resets_at": "2026-09-15T14:30:00.048430+00:00"},
  "seven_day": {"utilization": 14.0, "resets_at": "2026-09-17T18:00:00.048462+00:00"},
  "seven_day_opus": null,
  "limits": [
    {"kind": "session", "group": "session", "percent": 2, "resets_at": "2026-09-15T14:30:00.048430+00:00"},
    {"kind": "weekly_all", "group": "weekly", "percent": 14, "resets_at": "2026-09-17T18:00:00.048462+00:00"},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 15, "resets_at": "2026-09-17T18:00:00.048797+00:00",
     "scope": {"model": {"display_name": "Fable"}}}
  ]
}`

func TestProfileUsageFromPayload(t *testing.T) {
	t.Run("float windows from the live API", func(t *testing.T) {
		var p oauthUsagePayload
		if err := json.Unmarshal([]byte(liveUsageJSON), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		u := profileUsageFromPayload(p)
		if u.FiveHour != 2 || !u.FiveHourKnown {
			t.Errorf("5h = %d known=%v, want 2 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 14 || !u.SevenDayKnown {
			t.Errorf("7d = %d known=%v, want 14 known", u.SevenDay, u.SevenDayKnown)
		}
		if u.FiveHourResetAt.IsZero() {
			t.Error("5h resets_at not parsed")
		}
	})

	t.Run("limits array fills in when five_hour is null", func(t *testing.T) {
		raw := `{
		  "five_hour": null,
		  "seven_day": null,
		  "limits": [
		    {"kind": "session", "percent": 41, "resets_at": "2026-09-15T14:30:00Z"},
		    {"kind": "weekly_all", "percent": 9, "resets_at": "2026-09-17T18:00:00Z"}
		  ]
		}`
		var p oauthUsagePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		u := profileUsageFromPayload(p)
		if u.FiveHour != 41 || !u.FiveHourKnown {
			t.Errorf("5h = %d known=%v, want 41 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 9 || !u.SevenDayKnown {
			t.Errorf("7d = %d known=%v, want 9 known", u.SevenDay, u.SevenDayKnown)
		}
	})
}

func TestRefreshProfileUsageFetchesAPI(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != oauthUsageBeta {
			t.Errorf("beta = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveUsageJSON))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient, oldSec := oauthUsageURL, usageHTTP, securityOutput
	oauthUsageURL = srv.URL + "/api/oauth/usage"
	usageHTTP = srv.Client()
	securityOutput = func(args ...string) ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() {
		oauthUsageURL = oldURL
		usageHTTP = oldClient
		securityOutput = oldSec
	})

	dir := t.TempDir()
	creds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test","refreshToken":"sk-ant-ort01-test","expiresAt":` +
		farExpiryMS() + `,"scopes":["user:profile"]}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(creds), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", ConfigDir: dir}

	u := refreshProfileUsage(p)
	if u.FiveHour != 2 || u.SevenDay != 14 {
		t.Fatalf("got 5h=%d 7d=%d, want 2 and 14", u.FiveHour, u.SevenDay)
	}
	// Second call must be served from memory, not the server (which we now close).
	srv.Close()
	u2 := refreshProfileUsage(p)
	if u2.FiveHour != 2 || u2.SevenDay != 14 {
		t.Fatalf("cached 5h=%d 7d=%d", u2.FiveHour, u2.SevenDay)
	}
}

func TestPickOAuthPrefersLiveKeychainOverStaleFile(t *testing.T) {
	file := oauthBlob{oauth: &claudeAiOauth{AccessToken: "stale", ExpiresAt: 1}}
	keychain := oauthBlob{
		oauth:   &claudeAiOauth{AccessToken: "live", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		service: keychainService,
		account: "jairo",
	}
	got, ok := pickOAuth(file, keychain, time.Now())
	if !ok || got.oauth.AccessToken != "live" || !got.fromKeychain() {
		t.Fatalf("got %+v ok=%v, want the live keychain token", got.oauth, ok)
	}
}

func TestPatchOAuthJSONKeepsSiblingKeys(t *testing.T) {
	raw := []byte(`{"mcpOAuth":{"slack":true},"claudeAiOauth":{"accessToken":"old","refreshToken":"r","expiresAt":1}}`)
	fresh := &claudeAiOauth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 9}
	out, err := patchOAuthJSON(raw, fresh)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["mcpOAuth"]) != `{"slack":true}` {
		t.Errorf("mcpOAuth lost: %s", m["mcpOAuth"])
	}
	var oauth claudeAiOauth
	if err := json.Unmarshal(m["claudeAiOauth"], &oauth); err != nil {
		t.Fatal(err)
	}
	if oauth.AccessToken != "new" {
		t.Errorf("access = %s", oauth.AccessToken)
	}
}

func TestUsageWindowName(t *testing.T) {
	cases := map[int]string{0: "win", 900: "15m", 18000: "5h", 604800: "7d"}
	for sec, want := range cases {
		if got := usageWindowName(sec); got != want {
			t.Errorf("usageWindowName(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestUsageLine(t *testing.T) {
	claude := profileUsage{FiveHour: 12, SevenDay: 40, FiveHourKnown: true, SevenDayKnown: true}
	if got := profileUsageLine(engineClaude, claude); got != "5h 12% · 7d 40%" {
		t.Errorf("claude = %q", got)
	}
	if got := profileUsageLine(engineClaude, unknownProfileUsage()); got != "5h ? · 7d ?" {
		t.Errorf("claude unknown = %q", got)
	}
	grok := profileUsage{Windows: []usageWin{{Name: "week", Percent: 3}}, FiveHour: 3, FiveHourKnown: true}
	if got := profileUsageLine(engineGrok, grok); got != "week 3%" {
		t.Errorf("grok = %q", got)
	}
	if got := profileUsageLine(engineAntigravity, naProfileUsage("no public usage endpoint")); got != "n/a (no public usage endpoint)" {
		t.Errorf("agy = %q", got)
	}
	if got := profileUsageLine(engineGrok, unknownProfileUsage()); got != "?" {
		t.Errorf("grok unknown = %q", got)
	}
}

func TestProfileUsageFromGrokWeekly(t *testing.T) {
	pct := 3.0
	u := profileUsageFromGrok(grokBillingConfig{
		CreditUsagePercent: &pct,
		CurrentPeriod:      grokPeriod{Type: "USAGE_PERIOD_TYPE_WEEKLY", End: "2026-09-23T20:26:20Z"},
	})
	if !u.FiveHourKnown || u.FiveHour != 3 {
		t.Fatalf("5h = %d known=%v", u.FiveHour, u.FiveHourKnown)
	}
	if len(u.Windows) != 1 || u.Windows[0].Name != "week" || u.Windows[0].Percent != 3 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	if profileUsageLine(engineGrok, u) != "week 3%" {
		t.Errorf("line = %q", profileUsageLine(engineGrok, u))
	}
}

func TestProfileUsageFromCodexWindows(t *testing.T) {
	u := profileUsageFromCodex(codexUsagePayload{RateLimit: &codexRateLimit{
		PrimaryWindow:   &codexWindow{UsedPercent: 18, LimitWindowSeconds: 18000, ResetAt: 1730947200},
		SecondaryWindow: &codexWindow{UsedPercent: 86, LimitWindowSeconds: 604800, ResetAt: 1730980800},
	}})
	if profileUsageLine(engineCodex, u) != "5h 18% · 7d 86%" {
		t.Errorf("line = %q", profileUsageLine(engineCodex, u))
	}
	if u.FiveHour != 18 || u.SevenDay != 86 {
		t.Errorf("selection 5h=%d 7d=%d", u.FiveHour, u.SevenDay)
	}

	weekly := profileUsageFromCodex(codexUsagePayload{RateLimit: &codexRateLimit{
		PrimaryWindow: &codexWindow{UsedPercent: 0, LimitWindowSeconds: 604800, ResetAt: 1790246266},
	}})
	if profileUsageLine(engineCodex, weekly) != "7d 0%" {
		t.Errorf("weekly-only = %q", profileUsageLine(engineCodex, weekly))
	}
}

func TestRefreshProfileUsageGrokFetchesBilling(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing" || r.URL.RawQuery != "format=credits" {
			t.Errorf("path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer grok-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":3.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-09-23T20:26:20Z"}}}`))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient := grokBillingURL, usageHTTP
	grokBillingURL = srv.URL + "/v1/billing?format=credits"
	usageHTTP = srv.Client()
	t.Cleanup(func() {
		grokBillingURL = oldURL
		usageHTTP = oldClient
	})

	dir := t.TempDir()
	auth := `{"https://auth.x.ai::test":{"key":"grok-token","auth_mode":"oidc","expires_at":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", Engine: engineGrok, ConfigDir: dir}
	u := refreshProfileUsage(p)
	if profileUsageLine(engineGrok, u) != "week 3%" {
		t.Fatalf("got %q (5h=%d known=%v)", profileUsageLine(engineGrok, u), u.FiveHour, u.FiveHourKnown)
	}
	srv.Close()
	u2 := refreshProfileUsage(p)
	if profileUsageLine(engineGrok, u2) != "week 3%" {
		t.Fatalf("cached %q", profileUsageLine(engineGrok, u2))
	}
}

func TestRefreshProfileUsageCodexFetchesWham(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-1" {
			t.Errorf("account-id = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":18,"limit_window_seconds":18000,"reset_at":1730947200},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1730980800}}}`))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient := codexUsageURL, usageHTTP
	codexUsageURL = srv.URL + "/backend-api/wham/usage"
	usageHTTP = srv.Client()
	t.Cleanup(func() {
		codexUsageURL = oldURL
		usageHTTP = oldClient
	})

	dir := t.TempDir()
	auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"codex-token","refresh_token":"rt","account_id":"acct-1"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "openai", Engine: engineCodex, ConfigDir: dir}
	u := refreshProfileUsage(p)
	if profileUsageLine(engineCodex, u) != "5h 18% · 7d 40%" {
		t.Fatalf("got %q", profileUsageLine(engineCodex, u))
	}
}

func TestRefreshProfileUsageAntigravityIsNA(t *testing.T) {
	p := Profile{Name: "lab", Engine: engineAntigravity, ConfigDir: t.TempDir()}
	u := refreshProfileUsage(p)
	if u.Unavailable != "no public usage endpoint" {
		t.Fatalf("unavailable = %q", u.Unavailable)
	}
	if profileUsageLine(engineAntigravity, u) != "n/a (no public usage endpoint)" {
		t.Fatalf("line = %q", profileUsageLine(engineAntigravity, u))
	}
}

func TestCodexAPIKeyHasNoQuota(t *testing.T) {
	key := "sk-test"
	auth := codexAuthFile{AuthMode: "apikey", OpenAIAPIKey: &key}
	if got := codexUsageNA(auth); got != "API key, not ChatGPT quota" {
		t.Fatalf("got %q", got)
	}
}

func farExpiryMS() string {
	return jsonNumber(time.Now().Add(8 * time.Hour).UnixMilli())
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
