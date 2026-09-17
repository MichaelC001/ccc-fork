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

// Grok Build (the ccc grok engine) is a SuperGrok OAuth session, not an xAI
// API key. The CLI's /usage card reads:
//
//	GET https://cli-chat-proxy.grok.com/v1/billing?format=credits
//
// with the access token in $GROK_HOME/auth.json. That is the same weekly pool
// grok.com Settings → Usage shows. The xAI Management API prepaid-balance
// route is API-key billing and does not apply to this login.

var (
	grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokTokenURL   = "https://auth.x.ai/oauth2/token"
)

type grokAuth struct {
	Key          string `json:"key"`
	AuthMode     string `json:"auth_mode"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
}

type grokBillingPayload struct {
	Config *grokBillingConfig `json:"config"`
	grokBillingConfig
}

type grokBillingConfig struct {
	CreditUsagePercent *float64      `json:"creditUsagePercent"`
	CurrentPeriod      grokPeriod    `json:"currentPeriod"`
	BillingPeriodEnd   string        `json:"billingPeriodEnd"`
	ProductUsage       []grokProduct `json:"productUsage"`
	PrepaidBalance     *grokMoneyVal `json:"prepaidBalance"`
}

type grokPeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type grokProduct struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usagePercent"`
}

type grokMoneyVal struct {
	Val json.RawMessage `json:"val"`
}

func fetchGrokUsage(p Profile) (profileUsage, error) {
	tok, err := grokAccessToken(p)
	if err != nil {
		return unknownProfileUsage(), err
	}
	u, status, err := grokBillingOnce(tok)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if fresh, rerr := grokRefreshAndSave(p); rerr == nil {
			tok = fresh
			u, status, err = grokBillingOnce(tok)
		}
	}
	if err != nil {
		return unknownProfileUsage(), err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), fmt.Errorf("grok billing: HTTP %d", status)
	}
	return u, nil
}

func grokBillingOnce(tok string) (profileUsage, int, error) {
	raw, status, err := httpJSON(http.MethodGet, grokBillingURL, "Bearer "+tok, nil, nil)
	if err != nil {
		return unknownProfileUsage(), status, err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), status, nil
	}
	var payload grokBillingPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return unknownProfileUsage(), status, err
	}
	cfg := payload.grokBillingConfig
	if payload.Config != nil {
		cfg = *payload.Config
	}
	return profileUsageFromGrok(cfg), status, nil
}

func profileUsageFromGrok(cfg grokBillingConfig) profileUsage {
	u := unknownProfileUsage()
	pct := grokPercent(cfg)
	if pct == nil {
		return u
	}
	name := grokPeriodName(cfg.CurrentPeriod.Type)
	reset := parseResetAt(cfg.CurrentPeriod.End)
	if reset.IsZero() {
		reset = parseResetAt(cfg.BillingPeriodEnd)
	}
	n := percentFromFloat(*pct)
	u.Windows = []usageWin{{Name: name, Percent: n, ResetAt: reset}}
	u.FiveHour = n
	u.FiveHourKnown = true
	u.FiveHourResetAt = reset
	if name == "week" || name == "7d" || name == "month" {
		u.SevenDay = n
		u.SevenDayKnown = true
	}
	return u
}

func grokPercent(cfg grokBillingConfig) *float64 {
	if cfg.CreditUsagePercent != nil {
		return cfg.CreditUsagePercent
	}
	for _, p := range cfg.ProductUsage {
		if strings.EqualFold(p.Product, "GrokBuild") && p.UsagePercent != nil {
			return p.UsagePercent
		}
	}
	for _, p := range cfg.ProductUsage {
		if p.UsagePercent != nil {
			return p.UsagePercent
		}
	}
	return nil
}

func grokPeriodName(t string) string {
	t = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(t)), "USAGE_PERIOD_TYPE_")
	switch t {
	case "WEEKLY":
		return "week"
	case "DAILY":
		return "day"
	case "MONTHLY":
		return "month"
	case "HOURLY":
		return "hour"
	case "":
		return "week"
	default:
		return strings.ToLower(t)
	}
}

func grokAccessToken(p Profile) (string, error) {
	key, auth, raw, err := loadGrokAuth(p)
	if err != nil {
		return "", err
	}
	if !tokenExpired(grokExpiry(auth), time.Now()) {
		return auth.Key, nil
	}
	if auth.RefreshToken == "" {
		if auth.Key != "" {
			return auth.Key, nil
		}
		return "", fmt.Errorf("grok token expired and no refresh token")
	}
	fresh, err := refreshGrokAuth(auth)
	if err != nil {
		if auth.Key != "" {
			return auth.Key, nil
		}
		return "", err
	}
	if err := saveGrokAuth(grokAuthJSON(p), raw, key, fresh); err != nil {
		hookLog("could not persist refreshed grok token: %v", err)
	}
	return fresh.Key, nil
}

func grokRefreshAndSave(p Profile) (string, error) {
	key, auth, raw, err := loadGrokAuth(p)
	if err != nil {
		return "", err
	}
	if auth.RefreshToken == "" {
		return "", fmt.Errorf("grok: no refresh token")
	}
	fresh, err := refreshGrokAuth(auth)
	if err != nil {
		return "", err
	}
	if err := saveGrokAuth(grokAuthJSON(p), raw, key, fresh); err != nil {
		hookLog("could not persist refreshed grok token: %v", err)
	}
	return fresh.Key, nil
}

func grokExpiry(a grokAuth) time.Time {
	if exp := jwtExpiry(a.Key); !exp.IsZero() {
		return exp
	}
	return parseResetAt(a.ExpiresAt)
}

func loadGrokAuth(p Profile) (entryKey string, auth grokAuth, raw []byte, err error) {
	path := grokAuthJSON(p)
	raw, err = os.ReadFile(path)
	if err != nil {
		return "", grokAuth{}, nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", grokAuth{}, raw, fmt.Errorf("grok auth.json: %w", err)
	}
	var bestKey string
	var best grokAuth
	now := time.Now()
	for k, blob := range m {
		var a grokAuth
		if json.Unmarshal(blob, &a) != nil || strings.TrimSpace(a.Key) == "" {
			continue
		}
		if best.Key == "" {
			bestKey, best = k, a
		}
		if !tokenExpired(grokExpiry(a), now) {
			return k, a, raw, nil
		}
	}
	if best.Key == "" {
		return "", grokAuth{}, raw, fmt.Errorf("no grok oauth credentials for %s", accountDisplay(p))
	}
	return bestKey, best, raw, nil
}

func refreshGrokAuth(a grokAuth) (grokAuth, error) {
	cid := strings.TrimSpace(a.OIDCClientID)
	if cid == "" {
		return grokAuth{}, fmt.Errorf("grok refresh: missing oidc_client_id")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.RefreshToken},
		"client_id":     {cid},
	}
	raw, status, err := httpFormPost(grokTokenURL, form)
	if err != nil {
		return grokAuth{}, err
	}
	if status != http.StatusOK {
		return grokAuth{}, fmt.Errorf("grok oauth refresh: HTTP %d", status)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return grokAuth{}, err
	}
	if tok.AccessToken == "" {
		return grokAuth{}, fmt.Errorf("grok oauth refresh: empty access_token")
	}
	fresh := a
	fresh.Key = tok.AccessToken
	if tok.RefreshToken != "" {
		fresh.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		fresh.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	return fresh, nil
}

func saveGrokAuth(path string, raw []byte, entryKey string, auth grokAuth) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(m[entryKey], &obj); err != nil {
		return err
	}
	keyJSON, err := json.Marshal(auth.Key)
	if err != nil {
		return err
	}
	obj["key"] = keyJSON
	if auth.RefreshToken != "" {
		rt, err := json.Marshal(auth.RefreshToken)
		if err != nil {
			return err
		}
		obj["refresh_token"] = rt
	}
	if auth.ExpiresAt != "" {
		ex, err := json.Marshal(auth.ExpiresAt)
		if err != nil {
			return err
		}
		obj["expires_at"] = ex
	}
	patched, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	m[entryKey] = patched
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
}
