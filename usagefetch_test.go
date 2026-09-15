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

func farExpiryMS() string {
	return jsonNumber(time.Now().Add(8 * time.Hour).UnixMilli())
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
