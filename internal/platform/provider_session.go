package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const sessionEndpoint = "https://chatgpt.com/api/auth/session?refresh=true&reason=token_expired"
const accountEndpoint = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=0"

var sessionCookieName = regexp.MustCompile(`^__Secure-(?:next-auth|platform)\.session-token(?:\.[0-9]{1,2})?$`)

type refreshCookieJar struct{ http.CookieJar }

func (j refreshCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if u.Scheme != "https" || u.Hostname() != "chatgpt.com" {
		return
	}
	// Clear obsolete aliases/chunks when the server selects a new cookie family.
	for _, incoming := range cookies {
		if !sessionCookieName.MatchString(incoming.Name) || incoming.Value == "" || incoming.MaxAge < 0 {
			continue
		}
		family := strings.Split(incoming.Name, "session-token")[0] + "session-token"
		chunked := incoming.Name != family
		for _, old := range j.Cookies(u) {
			if !sessionCookieName.MatchString(old.Name) {
				continue
			}
			if !strings.HasPrefix(old.Name, family) || (!chunked && old.Name != family) || (chunked && old.Name == family) {
				j.CookieJar.SetCookies(u, []*http.Cookie{{Name: old.Name, Value: "", Path: "/", MaxAge: -1, Secure: true}})
			}
		}
		break
	}
	j.CookieJar.SetCookies(u, cookies)
}

// Persist only the minimum first-party authorization, never arbitrary Session JSON.
func sessionCredentials(raw json.RawMessage, info SessionInfo) (checkoutInput, time.Time, error) {
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		raw = []byte(quoted)
	}
	var s struct {
		AccessToken  string `json:"accessToken"`
		SessionToken string `json:"sessionToken"`
		Cookies      string `json:"cookies"`
		Expires      string `json:"expires"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return checkoutInput{}, time.Time{}, bad("Session 格式不正确")
	}
	in := checkoutInput{AccessToken: s.AccessToken, AccountID: info.AccountID, Email: info.Email, TokenExpiresAt: info.ExpiresAt}
	deadline := info.ExpiresAt
	if len(s.SessionToken) > 32768 || len(s.Cookies) > 65536 || strings.ContainsAny(s.Cookies, "\r\n") {
		return in, deadline, bad("Session Cookie 格式不正确")
	}
	if s.SessionToken != "" {
		cookie := &http.Cookie{Name: "__Secure-next-auth.session-token", Value: s.SessionToken}
		if cookie.Valid() != nil {
			return in, deadline, bad("Session Token 格式不正确")
		}
		in.SessionToken = s.SessionToken
	}
	var selected []*http.Cookie
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {s.Cookies}}}).Cookies() {
		if sessionCookieName.MatchString(c.Name) && c.Valid() == nil {
			selected = append(selected, c)
		}
	}
	if len(selected) > 0 {
		in.Cookies = joinSessionCookies(selected)
	}
	if in.SessionToken != "" || in.Cookies != "" {
		deadline = time.Now().Add(24 * time.Hour)
		if expiry, e := time.Parse(time.RFC3339Nano, s.Expires); e == nil && expiry.Before(deadline) {
			deadline = expiry
		}
	}
	return in, deadline, nil
}

func joinSessionCookies(cookies []*http.Cookie) string {
	var parts []string
	for _, c := range cookies {
		if sessionCookieName.MatchString(c.Name) && c.Valid() == nil {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func providerHeaders(in checkoutInput) http.Header {
	h := http.Header{"Authorization": {"Bearer " + in.AccessToken}, "Chatgpt-Account-Id": {in.AccountID}, "Accept": {"application/json"}, "User-Agent": {webCheckoutUA}, "Origin": {"https://chatgpt.com"}, "Referer": {"https://chatgpt.com/"}}
	if in.Cookies != "" {
		h.Set("Cookie", in.Cookies)
	}
	return h
}

func refreshProviderSession(ctx context.Context, client *http.Client, in *checkoutInput, purpose string) error {
	if in.SessionToken == "" && in.Cookies == "" {
		return checkoutFailure("session_refresh_unavailable", false, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	u, _ := url.Parse("https://chatgpt.com/")
	cookies := (&http.Request{Header: http.Header{"Cookie": {in.Cookies}}}).Cookies()
	if len(cookies) == 0 {
		for _, name := range []string{"__Secure-next-auth.session-token", "__Secure-platform.session-token"} {
			cookies = append(cookies, &http.Cookie{Name: name, Value: in.SessionToken, Path: "/", Secure: true, HttpOnly: true})
		}
	}
	jar.SetCookies(u, cookies)
	local := *client
	local.Jar = refreshCookieJar{jar}
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h := providerHeaders(*in)
	h.Del("Authorization")
	h.Del("Cookie")
	h.Set("X-Openai-Target-Path", "/api/auth/session")
	h.Set("X-Openai-Target-Route", "/api/auth/session")
	v, err := providerJSON(ctx, &local, "GET", sessionEndpoint, h, nil, false)
	if err != nil {
		return err
	}
	if str(v, "accessToken") == "" || (v["error"] != nil && v["error"] != "") {
		return checkoutFailure("invalid_session", false, false)
	}
	account, _ := v["account"].(map[string]any)
	user, _ := v["user"].(map[string]any)
	if str(account, "id") != in.AccountID || !strings.EqualFold(str(user, "email"), in.Email) {
		return checkoutFailure("account_mismatch", false, false)
	}
	raw, _ := json.Marshal(v)
	info, err := inspectSession(raw)
	if err != nil {
		return checkoutFailure("invalid_session", false, false)
	}
	in.AccessToken = str(v, "accessToken")
	in.TokenExpiresAt = info.ExpiresAt
	in.Cookies = joinSessionCookies(jar.Cookies(u))
	// The jar is authoritative after rotation or deletion; never resurrect an old cookie.
	in.SessionToken = ""
	in.RefreshedAt = time.Now().UTC()
	in.RefreshPurpose = purpose
	return nil
}

type providerSession struct {
	ctx       context.Context
	client    *http.Client
	in        *checkoutInput
	persist   func(checkoutInput) error
	attempted bool
	purpose   string
	trace     func(string, string)
}

func (s *providerSession) refresh() error {
	s.attempted = true
	if s.trace != nil {
		s.trace("running", "")
	}
	err := refreshProviderSession(s.ctx, s.client, s.in, s.purpose)
	if err == nil {
		err = s.persist(*s.in)
	}
	if s.trace != nil {
		if err == nil {
			s.trace("completed", "")
		} else {
			s.trace("failed", providerCode(err))
		}
	}
	return err
}

func (s *providerSession) prepare() error {
	// Activation first queries the supplied AT; only a rejected read refreshes it.
	if s.purpose == "activation" {
		return nil
	}
	// Checkout uses the supplied AT. Refresh belongs to the post-payment renewal
	// stage; an expired checkout credential must not add a pre-payment refresh.
	if s.purpose == "checkout" {
		if !s.in.TokenExpiresAt.IsZero() && !s.in.TokenExpiresAt.After(time.Now()) {
			return checkoutFailure("invalid_session", false, false)
		}
		return nil
	}
	available := s.in.SessionToken != "" || s.in.Cookies != ""
	if !available && s.trace != nil {
		s.trace("waiting", "session_refresh_unavailable")
	}
	if available && (s.in.RefreshPurpose != s.purpose || time.Since(s.in.RefreshedAt) > time.Minute || !s.in.TokenExpiresAt.After(time.Now().Add(time.Minute))) {
		// Once renewal requires a refresh, a failed refresh cannot authorize
		// cancellation with the original, potentially stale AT.
		return s.refresh()
	}
	if !s.in.TokenExpiresAt.IsZero() && !s.in.TokenExpiresAt.After(time.Now()) {
		return checkoutFailure("invalid_session", false, false)
	}
	return nil
}

func (s *providerSession) read(ctx context.Context, target string) (M, error) {
	v, err := providerJSON(ctx, s.client, "GET", target, providerHeaders(*s.in), nil, false)
	if s.purpose != "checkout" && providerCode(err) == "invalid_session" && !s.attempted && (s.in.SessionToken != "" || s.in.Cookies != "") {
		if err = s.refresh(); err == nil {
			return providerJSON(ctx, s.client, "GET", target, providerHeaders(*s.in), nil, false)
		}
	}
	return v, err
}

func providerCode(err error) string {
	if err == nil {
		return ""
	}
	var e checkoutError
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal_error"
}

func checkAccountForCheckout(ctx context.Context, s *providerSession) error {
	v, err := s.read(ctx, accountEndpoint)
	if err != nil {
		return err
	}
	accounts, _ := v["accounts"].(map[string]any)
	for key, value := range accounts {
		entry, _ := value.(map[string]any)
		account, _ := entry["account"].(map[string]any)
		id := str(account, "id")
		if id == "" {
			id = str(account, "account_id")
		}
		if id == "" {
			id = key
		}
		if id != s.in.AccountID {
			continue
		}
		if kind := str(account, "account_type"); kind == "team" || kind == "business" || kind == "enterprise" {
			return checkoutFailure("personal_account_required", false, false)
		}
		ent, _ := entry["entitlement"].(map[string]any)
		active, ok := ent["has_active_subscription"].(bool)
		if !ok {
			return checkoutFailure("account_status_unknown", true, false)
		}
		if active {
			return checkoutFailure("existing_subscription", false, false)
		}
		return nil
	}
	return checkoutFailure("account_mismatch", false, false)
}

func (a *App) saveCheckoutCredentials(ctx context.Context, j *Job, in checkoutInput) error {
	// Keep only authorization in the encrypted record, no callbacks/checkout parameters.
	in.BeforeCreate = nil
	in.ObserveIP = nil
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	enc, err := a.encrypt(string(raw))
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, "SELECT id FROM jobs WHERE id=$1 AND lease_token=$2 AND state='running' AND lease_until>now() FOR UPDATE", j.ID, j.Lease).Scan(&id); err != nil {
		return ErrLeaseLost
	}
	if _, err = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc=$2 WHERE order_id=$1", j.OrderID, enc); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2 WHERE order_id=$1 AND renewal_state='not_started'", j.OrderID, enc); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) saveRenewalCredentials(ctx context.Context, id, lease string, in checkoutInput, standalone bool) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	enc, err := a.encrypt(string(raw))
	if err != nil {
		return err
	}
	query := "UPDATE subscription_runs SET credentials_enc=$3 WHERE order_id=$1 AND lease_token=$2 AND lease_until>now()"
	if standalone {
		query = "UPDATE renewal_tasks SET credentials_enc=$3 WHERE id=$1 AND lease_token=$2 AND lease_until>now()"
	}
	tag, err := a.DB.Exec(ctx, query, id, lease, enc)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return err
}
