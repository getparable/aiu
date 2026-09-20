package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------- http helpers

type apiResponse struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r *apiResponse) ok() bool { return r.Status >= 200 && r.Status < 300 }

func (r *apiResponse) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// describe summarises an error body for a message, with secrets stripped.
func (r *apiResponse) describe() string {
	m := r.json()
	text := ""
	if m != nil {
		text = firstNonEmpty(str(obj(m["error"])["message"]), str(m["error_description"]), str(m["error"]))
	}
	if text == "" {
		text = string(r.Body)
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return Redact(text)
}

func (c *Config) do(ctx context.Context, method, rawURL string, headers map[string]string, body io.Reader) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aiu/"+Version)
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errors.New(Redact(err.Error()))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &apiResponse{Status: res.StatusCode, Body: data, Header: res.Header}, nil
}

func (c *Config) apiGet(ctx context.Context, rawURL, token string, headers map[string]string) (*apiResponse, error) {
	h := map[string]string{"Authorization": "Bearer " + token}
	for k, v := range headers {
		h[k] = v
	}
	return c.do(ctx, http.MethodGet, rawURL, h, nil)
}

func (c *Config) postJSON(ctx context.Context, rawURL string, payload any) (*apiResponse, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, rawURL, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(data))
}

func host(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return rawURL
}

// ---------------------------------------------------------------- claude tokens

func (c *Config) claudeTokensFromResponse(body map[string]any, previousRefresh string) *Record {
	now := c.now().UnixMilli()
	r := &Record{
		Provider:     Claude,
		AccessToken:  str(body["access_token"]),
		RefreshToken: firstNonEmpty(str(body["refresh_token"]), previousRefresh),
	}
	expiresIn, ok := num(body["expires_in"])
	if !ok || expiresIn <= 0 {
		expiresIn = 3600
	}
	r.ExpiresAt = now + int64(expiresIn*1000)
	if n, ok := num(body["refresh_token_expires_in"]); ok && n > 0 {
		r.RefreshTokenExpiresAt = now + int64(n*1000)
	}
	if s := str(body["scope"]); s != "" {
		r.Scopes = strings.Fields(s)
	}
	return r
}

// refreshClaude trades a refresh token for a new pair. Refresh tokens are single-use:
// the old one is dead the moment this succeeds.
func (c *Config) refreshClaude(ctx context.Context, refreshToken string) (*Record, error) {
	var lastErr error
	for _, u := range c.ClaudeTokenURLs {
		res, err := c.postJSON(ctx, u, map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
			"client_id":     claudeClientID,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if body := res.json(); res.ok() && str(body["access_token"]) != "" {
			return c.claudeTokensFromResponse(body, refreshToken), nil
		}
		lastErr = fmt.Errorf("token refresh failed (%d @ %s): %s", res.Status, host(u), res.describe())
		if res.Status == 400 || res.Status == 401 {
			break
		}
	}
	return nil, lastErr
}

func (c *Config) exchangeClaudeCode(ctx context.Context, pasted, expectedState, verifier, redirectURI string) (map[string]any, error) {
	code, embeddedState, hasState := strings.Cut(strings.TrimSpace(pasted), "#")
	if code == "" {
		return nil, errors.New("no authorization code to exchange")
	}
	// The manual flow pastes `code#state`. Trusting that state would let someone hand
	// over their own code and silently attach their account instead.
	if hasState && !safeEqual(embeddedState, expectedState) {
		return nil, errors.New("the pasted code does not belong to this login attempt (state mismatch) — start the login again")
	}
	var lastErr error
	for _, u := range c.ClaudeTokenURLs {
		res, err := c.postJSON(ctx, u, map[string]string{
			"grant_type":    "authorization_code",
			"code":          code,
			"state":         expectedState,
			"redirect_uri":  redirectURI,
			"client_id":     claudeClientID,
			"code_verifier": verifier,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if body := res.json(); res.ok() && str(body["access_token"]) != "" {
			return body, nil
		}
		lastErr = fmt.Errorf("code exchange failed (%d @ %s): %s", res.Status, host(u), res.describe())
		if res.Status == 400 || res.Status == 401 {
			break
		}
	}
	return nil, lastErr
}

// fetchClaudeProfile resolves a Claude token to its account.
func (c *Config) fetchClaudeProfile(ctx context.Context, token string) (string, map[string]any, error) {
	res, err := c.apiGet(ctx, c.ClaudeProfileURL, token, map[string]string{"anthropic-beta": claudeOAuthBeta})
	if err != nil {
		return "", nil, err
	}
	if !res.ok() {
		return "", nil, fmt.Errorf("profile %d: %s", res.Status, res.describe())
	}
	body := res.json()
	account := obj(body["account"])
	if account == nil {
		account = body
	}
	org := obj(body["organization"])
	email := firstNonEmpty(str(account["email"]), str(account["emailAddress"]), str(account["email_address"]))
	if email == "" {
		return "", nil, errors.New("profile response did not include an email")
	}
	profile := map[string]any{"emailAddress": email}
	for k, v := range map[string]any{
		"accountUuid":               account["uuid"],
		"displayName":               account["display_name"],
		"fullName":                  account["full_name"],
		"organizationUuid":          org["uuid"],
		"organizationName":          org["name"],
		"organizationType":          org["organization_type"],
		"billingType":               org["billing_type"],
		"organizationRateLimitTier": org["rate_limit_tier"],
	} {
		if v != nil {
			profile[k] = v
		}
	}
	return email, profile, nil
}

// ---------------------------------------------------------------- codex tokens

func (c *Config) codexTokensFromResponse(body map[string]any, previous *Record) *Record {
	now := c.now().UnixMilli()
	prev := previous
	if prev == nil {
		prev = &Record{}
	}
	idToken := firstNonEmpty(str(body["id_token"]), prev.IDToken)
	id := codexIdentity(idToken, prev.AccountID)
	r := &Record{
		Provider:     Codex,
		AccessToken:  str(body["access_token"]),
		RefreshToken: firstNonEmpty(str(body["refresh_token"]), prev.RefreshToken),
		IDToken:      idToken,
		LastRefresh:  now,
		Email:        id.Email,
		AccountID:    id.AccountID,
		PlanType:     id.PlanType,
		UserID:       id.UserID,
	}
	r.ExpiresAt = jwtExpiryMs(r.AccessToken)
	if r.ExpiresAt == 0 {
		if n, ok := num(body["expires_in"]); ok && n > 0 {
			r.ExpiresAt = now + int64(n*1000)
		} else {
			r.ExpiresAt = now + codexTokenLifetime.Milliseconds()
		}
	}
	return r
}

// refreshCodex rotates a Codex token set; OpenAI retires the old refresh token too.
func (c *Config) refreshCodex(ctx context.Context, previous *Record) (*Record, error) {
	res, err := c.postJSON(ctx, c.CodexTokenURL, map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": previous.RefreshToken,
	})
	if err != nil {
		return nil, err
	}
	if body := res.json(); res.ok() && str(body["access_token"]) != "" {
		return c.codexTokensFromResponse(body, previous), nil
	}
	return nil, fmt.Errorf("token refresh failed (%d @ %s): %s", res.Status, host(c.CodexTokenURL), res.describe())
}

// The Codex token endpoint takes the authorization-code grant as a form, not JSON.
func (c *Config) exchangeCodexCode(ctx context.Context, code, verifier, redirectURI string) (map[string]any, error) {
	if code == "" {
		return nil, errors.New("no authorization code to exchange")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {codexClientID},
		"code_verifier": {verifier},
	}
	res, err := c.do(ctx, http.MethodPost, c.CodexTokenURL,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if body := res.json(); res.ok() && str(body["access_token"]) != "" {
		return body, nil
	}
	return nil, fmt.Errorf("code exchange failed (%d @ %s): %s", res.Status, host(c.CodexTokenURL), res.describe())
}

// ---------------------------------------------------------------- login flow

// LoginOptions selects which login to start.
type LoginOptions struct {
	Provider   Provider
	ReadOnly   bool // Claude only: a user:profile token that cannot run inference
	Manual     bool // Claude only: paste code#state instead of a loopback callback
	UseConsole bool
}

// LoginSession is a sign-in in progress: open AuthorizeURL, then Complete.
type LoginSession struct {
	Provider     Provider
	AuthorizeURL string
	Port         int
	Manual       bool

	redirectURI string
	verifier    string
	state       string
	callback    *callbackServer
}

// Cancel stops waiting for the browser.
func (s *LoginSession) Cancel() {
	if s.callback != nil {
		s.callback.close(errors.New("login cancelled"))
	}
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pkce() (verifier, challenge, state string) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	verifier = base64url(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64url(sum[:])
	_, _ = rand.Read(buf)
	state = base64url(buf)
	return
}

// BeginLogin binds the callback listener first, so the authorize URL it returns
// always points at a port this session owns.
func (c *Config) BeginLogin(opts LoginOptions) (*LoginSession, error) {
	verifier, challenge, state := pkce()
	s := &LoginSession{Provider: opts.Provider, verifier: verifier, state: state, Manual: opts.Manual}

	if opts.Provider == Codex {
		if opts.Manual {
			return nil, errors.New("the Codex login has no paste-the-code flow — OpenAI only redirects to localhost")
		}
		for _, port := range codexCallbackPorts {
			cb, err := listenCallback(port, state, codexCallbackPath, Codex)
			if err == nil {
				s.callback, s.Port = cb, cb.port
				break
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				return nil, err
			}
		}
		if s.callback == nil {
			return nil, errors.New("ports 1455 and 1457 are both in use (a `codex login` in progress?) — OpenAI accepts only those callback ports")
		}
		s.redirectURI = fmt.Sprintf("http://localhost:%d%s", s.Port, codexCallbackPath)
		q := url.Values{
			"response_type":              {"code"},
			"client_id":                  {codexClientID},
			"redirect_uri":               {s.redirectURI},
			"scope":                      {codexLoginScopes},
			"code_challenge":             {challenge},
			"code_challenge_method":      {"S256"},
			"id_token_add_organizations": {"true"},
			"codex_cli_simplified_flow":  {"true"},
			"state":                      {state},
			"originator":                 {codexOriginator},
		}
		s.AuthorizeURL = c.CodexAuthorizeURL + "?" + q.Encode()
		return s, nil
	}

	scopes := LoginScopesFull
	if opts.ReadOnly {
		scopes = LoginScopesRead
	}
	if opts.Manual {
		s.redirectURI = c.ClaudeManualRedirectURL
	} else {
		cb, err := listenCallback(claudeCallbackPort, state, callbackPath, Claude)
		if errors.Is(err, syscall.EADDRINUSE) {
			// A leftover listener from an abandoned login owns the default port.
			cb, err = listenCallback(0, state, callbackPath, Claude)
		}
		if err != nil {
			return nil, err
		}
		s.callback, s.Port = cb, cb.port
		s.redirectURI = fmt.Sprintf("http://localhost:%d%s", s.Port, callbackPath)
	}
	base := c.ClaudeAuthorizeURL
	if opts.UseConsole {
		base = c.ClaudeAuthorizeConsoleURL
	}
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeClientID},
		"response_type":         {"code"},
		"redirect_uri":          {s.redirectURI},
		"scope":                 {scopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	s.AuthorizeURL = base + "?" + q.Encode()
	return s, nil
}

// WaitForCode blocks until the browser hits the callback, it fails, or ctx ends.
func (s *LoginSession) WaitForCode(ctx context.Context) (string, error) {
	if s.callback == nil {
		return "", errors.New("this login has no callback — paste the code instead")
	}
	select {
	case res := <-s.callback.result:
		return res.code, res.err
	case <-ctx.Done():
		s.Cancel()
		return "", ctx.Err()
	}
}

// CompleteLogin exchanges the code and stores the account. Cancellation before
// the exchange begins stops login. Once begun, give the exchange and persistence
// a bounded opportunity to finish: Ctrl+C must not discard credentials already
// issued by the provider. Abrupt process termination cannot offer this guarantee.
func (c *Config) CompleteLogin(ctx context.Context, s *LoginSession, code, label string) (*SavedAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if s.Provider == Codex {
		body, err := c.exchangeCodexCode(ctx, strings.TrimSpace(code), s.verifier, s.redirectURI)
		if err != nil {
			return nil, err
		}
		return c.persistAccount(ctx, c.codexTokensFromResponse(body, nil), label, "oauth-login", false)
	}
	body, err := c.exchangeClaudeCode(ctx, code, s.state, s.verifier, s.redirectURI)
	if err != nil {
		return nil, err
	}
	return c.persistAccount(ctx, c.claudeTokensFromResponse(body, ""), label, "oauth-login", false)
}

func safeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// OpenBrowser hands the browser only https URLs on the hosts we build logins for.
func (c *Config) OpenBrowser(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(rawURL, "\"") || strings.IndexFunc(rawURL, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	allowed := false
	for _, a := range []string{c.ClaudeAuthorizeURL, c.ClaudeAuthorizeConsoleURL, c.CodexAuthorizeURL} {
		if host(a) == u.Host {
			allowed = true
		}
	}
	if !allowed {
		return false
	}
	return openBrowser(rawURL)
}

// ---------------------------------------------------------------- callback server

type callbackResult struct {
	code string
	err  error
}

type callbackServer struct {
	port   int
	server *http.Server
	result chan callbackResult
	once   sync.Once
}

// The callback page is served to a browser: no scripts, no embedding, and no caching
// or referrer that could carry the authorization code anywhere.
var callbackHeaders = map[string]string{
	"Content-Type":            "text/html; charset=utf-8",
	"Cache-Control":           "no-store, no-cache, must-revalidate",
	"Pragma":                  "no-cache",
	"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
	"Referrer-Policy":         "no-referrer",
	"X-Content-Type-Options":  "nosniff",
	"X-Frame-Options":         "DENY",
	"Connection":              "close",
}

func listenCallback(port int, expectedState, path string, provider Provider) (*callbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	cb := &callbackServer{port: ln.Addr().(*net.TCPAddr).Port, result: make(chan callbackResult, 1)}
	page := func(w http.ResponseWriter, status int, kind pageKind, title, detail string) {
		for k, v := range callbackHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		io.WriteString(w, callbackPage(provider, kind, title, detail))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.String()) > 8192 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			page(w, http.StatusBadRequest, pageNotice, "Unsupported request", "This address only accepts the sign-in redirect.")
			return
		}
		q := r.URL.Query()
		if r.URL.Path != path || !safeEqual(q.Get("state"), expectedState) {
			page(w, http.StatusNotFound, pageNotice, "Not this sign-in", "This link belongs to a different or expired aiu sign-in. Start again from aiu.")
			return
		}
		code, authErr := q.Get("code"), q.Get("error")
		switch {
		case authErr != "":
			page(w, http.StatusBadRequest, pageFailure, "Sign-in didn't finish", provider.Name()+" reported: "+authErr+". Start again from aiu.")
			go cb.close(errors.New("authorization failed: " + authErr))
		case code == "":
			page(w, http.StatusBadRequest, pageFailure, "Sign-in didn't finish", "The redirect carried no authorization code. Start again from aiu.")
			go cb.close(errors.New("no authorization code in callback"))
		default:
			page(w, http.StatusOK, pageSuccess, provider.Name()+" account connected", "aiu is saving the login. The account shows up in the menu bar and in aiu within a few seconds.")
			go cb.finish(code)
		}
	})
	cb.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = cb.server.Serve(ln) }()
	time.AfterFunc(loginTimeout, func() {
		cb.close(errors.New("timed out waiting for the browser callback (5 min)"))
	})
	return cb, nil
}

func (cb *callbackServer) finish(code string) { cb.settle(callbackResult{code: code}) }
func (cb *callbackServer) close(err error)    { cb.settle(callbackResult{err: err}) }

func (cb *callbackServer) settle(res callbackResult) {
	cb.once.Do(func() {
		cb.result <- res
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = cb.server.Shutdown(ctx)
	})
}
