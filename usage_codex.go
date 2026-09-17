package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Codex in ccc is a ChatGPT-subscription login (isolated CODEX_HOME), not a
// platform API key. The CLI itself reads quota from
//
//	GET https://chatgpt.com/backend-api/wham/usage
//
// (Authorization: Bearer <access_token>, ChatGPT-Account-Id: <account_id>).
// App-server's account/rateLimits/read is the same data over JSON-RPC; we
// call the HTTP route Claude-style so /status does not have to spawn
// `codex app-server`. Verified against Codex CLI 0.154.0 and live Pro Lite.

const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

var (
	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	codexTokenURL = "https://auth.openai.com/oauth/token"
)

type codexAuthFile struct {
	AuthMode     string       `json:"auth_mode"`
	OpenAIAPIKey *string      `json:"OPENAI_API_KEY"`
	Tokens       *codexTokens `json:"tokens"`
	LastRefresh  string       `json:"last_refresh"`
}

type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type codexUsagePayload struct {
	PlanType  string          `json:"plan_type"`
	RateLimit *codexRateLimit `json:"rate_limit"`
	Credits   *codexCredits   `json:"credits"`
}

type codexRateLimit struct {
	PrimaryWindow   *codexWindow `json:"primary_window"`
	SecondaryWindow *codexWindow `json:"secondary_window"`
}

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAfterSeconds  int     `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type codexCredits struct {
	HasCredits bool     `json:"has_credits"`
	Unlimited  bool     `json:"unlimited"`
	Balance    *float64 `json:"balance"`
}

func fetchCodexUsage(p Profile) (profileUsage, error) {
	auth, raw, err := loadCodexAuth(p)
	if err != nil {
		return unknownProfileUsage(), err
	}
	if reason := codexUsageNA(auth); reason != "" {
		return naProfileUsage(reason), nil
	}
	tok, err := codexAccessToken(p, auth, raw)
	if err != nil {
		return unknownProfileUsage(), err
	}
	acct := ""
	if auth.Tokens != nil {
		acct = auth.Tokens.AccountID
	}
	u, status, err := codexUsageOnce(tok, acct)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if fresh, rerr := codexRefreshAndSave(p); rerr == nil {
			tok = fresh.AccessToken
			acct = fresh.AccountID
			u, status, err = codexUsageOnce(tok, acct)
		}
	}
	if err != nil {
		return unknownProfileUsage(), err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), fmt.Errorf("codex usage: HTTP %d", status)
	}
	return u, nil
}

func codexUsageNA(auth codexAuthFile) string {
	mode := strings.ToLower(strings.TrimSpace(auth.AuthMode))
	if auth.Tokens != nil && strings.TrimSpace(auth.Tokens.AccessToken) != "" {
		return ""
	}
	if mode == "apikey" || mode == "api_key" || (auth.OpenAIAPIKey != nil && strings.TrimSpace(*auth.OpenAIAPIKey) != "") {
		return "API key, not ChatGPT quota"
	}
	return ""
}

func codexUsageOnce(tok, accountID string) (profileUsage, int, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if accountID != "" {
		h.Set("ChatGPT-Account-Id", accountID)
	}
	raw, status, err := httpJSON(http.MethodGet, codexUsageURL, "Bearer "+tok, h, nil)
	if err != nil {
		return unknownProfileUsage(), status, err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), status, nil
	}
	var payload codexUsagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return unknownProfileUsage(), status, err
	}
	return profileUsageFromCodex(payload), status, nil
}

func profileUsageFromCodex(p codexUsagePayload) profileUsage {
	u := unknownProfileUsage()
	if p.RateLimit == nil {
		return u
	}
	applyCodexWindow(&u, p.RateLimit.PrimaryWindow)
	applyCodexWindow(&u, p.RateLimit.SecondaryWindow)
	return u
}

func applyCodexWindow(u *profileUsage, w *codexWindow) {
	if w == nil {
		return
	}
	name := usageWindowName(w.LimitWindowSeconds)
	n := percentFromFloat(w.UsedPercent)
	reset := time.Time{}
	if w.ResetAt > 0 {
		reset = time.Unix(w.ResetAt, 0)
	}
	u.Windows = append(u.Windows, usageWin{Name: name, Percent: n, ResetAt: reset})
	if !u.FiveHourKnown {
		u.FiveHour = n
		u.FiveHourKnown = true
		u.FiveHourResetAt = reset
	}
	if name == "7d" || w.LimitWindowSeconds >= 2*86400 {
		u.SevenDay = n
		u.SevenDayKnown = true
	}
}

func codexAccessToken(p Profile, auth codexAuthFile, raw []byte) (string, error) {
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return "", fmt.Errorf("no codex chatgpt credentials for %s", accountDisplay(p))
	}
	if !tokenExpired(jwtExpiry(auth.Tokens.AccessToken), time.Now()) {
		return auth.Tokens.AccessToken, nil
	}
	if auth.Tokens.RefreshToken == "" {
		return auth.Tokens.AccessToken, nil
	}
	fresh, err := refreshCodexAuth(*auth.Tokens)
	if err != nil {
		return auth.Tokens.AccessToken, nil
	}
	if err := saveCodexTokens(codexAuthJSON(p), raw, fresh); err != nil {
		hookLog("could not persist refreshed codex token: %v", err)
	}
	return fresh.AccessToken, nil
}

func codexRefreshAndSave(p Profile) (codexTokens, error) {
	auth, raw, err := loadCodexAuth(p)
	if err != nil {
		return codexTokens{}, err
	}
	if auth.Tokens == nil || auth.Tokens.RefreshToken == "" {
		return codexTokens{}, fmt.Errorf("codex: no refresh token")
	}
	fresh, err := refreshCodexAuth(*auth.Tokens)
	if err != nil {
		return codexTokens{}, err
	}
	if err := saveCodexTokens(codexAuthJSON(p), raw, fresh); err != nil {
		hookLog("could not persist refreshed codex token: %v", err)
	}
	return fresh, nil
}

func loadCodexAuth(p Profile) (codexAuthFile, []byte, error) {
	raw, err := os.ReadFile(codexAuthJSON(p))
	if err != nil {
		return codexAuthFile{}, nil, err
	}
	var auth codexAuthFile
	if err := json.Unmarshal(raw, &auth); err != nil {
		return codexAuthFile{}, raw, fmt.Errorf("codex auth.json: %w", err)
	}
	return auth, raw, nil
}

func refreshCodexAuth(toks codexTokens) (codexTokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {toks.RefreshToken},
		"client_id":     {codexOAuthClientID},
	}
	raw, status, err := httpFormPost(codexTokenURL, form)
	if err != nil {
		return codexTokens{}, err
	}
	if status != http.StatusOK {
		return codexTokens{}, fmt.Errorf("codex oauth refresh: HTTP %d", status)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return codexTokens{}, err
	}
	if tok.AccessToken == "" {
		return codexTokens{}, fmt.Errorf("codex oauth refresh: empty access_token")
	}
	fresh := toks
	fresh.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		fresh.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		fresh.IDToken = tok.IDToken
	}
	return fresh, nil
}

func saveCodexTokens(path string, raw []byte, toks codexTokens) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	b, err := json.Marshal(toks)
	if err != nil {
		return err
	}
	m["tokens"] = b
	if ts, err := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano)); err == nil {
		m["last_refresh"] = ts
	}
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
}
