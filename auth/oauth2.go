// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The token exchange is written here rather than taken from
// golang.org/x/oauth2/clientcredentials. That module is not in the core
// module's graph, and a direct dependency becomes a floor for both
// products (AGENTS.md rule #10); the grant is one form POST.

const (
	clientAuthBasic = "basic"
	clientAuthPost  = "post"

	// tokenTimeout bounds one token request. The request runs detached
	// from any one caller's context, since callers waiting on it share
	// its result.
	tokenTimeout = 30 * time.Second
	// maxEarly is the most a token is refreshed ahead of its expiry; a
	// short-lived token refreshes a tenth of its lifetime early instead.
	maxEarly = time.Minute
	// refreshBackoff spaces background refreshes after one fails, so a
	// token endpoint that is down is not asked again on every request
	// while the current token is still good.
	refreshBackoff = 5 * time.Second
	// minTokenAge is how old a token must be before a 401 drops it. A
	// server that refuses a token fetched moments ago is refusing the
	// client, not the token's age; fetching another per request would
	// only load the token endpoint and churn the token for every caller.
	minTokenAge = 10 * time.Second
	// maxLifetime caps expires_in, so a nonsensical value can't overflow
	// time.Duration.
	maxLifetime = 365 * 24 * time.Hour
)

// clientCredentials is an OAuth2ClientCredentials TokenSource: one
// cached token, refreshed by at most one request at a time.
type clientCredentials struct {
	tokenURL     string
	host         string
	idEnv        string
	secretEnv    string
	clientID     string
	clientSecret string
	scopes       []string
	audience     string
	postAuth     bool
	hc           *http.Client
	now          func() time.Time

	mu    sync.Mutex
	token string
	// refresh is when to fetch the next token, a little before expiry,
	// the token's real end. Both are zero when the server gave no
	// expires_in: the token is kept until a server refuses it.
	refresh, expiry time.Time
	// issued is when the cached token was fetched; nextTry holds back a
	// background refresh after one failed.
	issued, nextTry time.Time
	inflight        *tokenCall
}

// tokenCall is one token request that every caller arriving while it
// runs waits on.
type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

func newClientCredentials(c Config, getenv func(string) string, hc *http.Client) (*clientCredentials, error) {
	u, err := url.Parse(c.TokenURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The URL itself is not secret, but say only what is wrong with
		// it, in case a query string carries something that is.
		return nil, errors.New("auth: token_url is not an http or https URL with a host")
	}
	var missing []string
	id, secret := getenv(c.ClientIDEnv), getenv(c.ClientSecretEnv)
	if id == "" {
		missing = append(missing, c.ClientIDEnv)
	}
	if secret == "" {
		missing = append(missing, c.ClientSecretEnv)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("auth: %s is not set", strings.Join(missing, " and "))
	}
	if hc == nil {
		hc = &http.Client{Timeout: tokenTimeout}
	}
	return &clientCredentials{
		tokenURL:     c.TokenURL,
		host:         u.Host,
		idEnv:        c.ClientIDEnv,
		secretEnv:    c.ClientSecretEnv,
		clientID:     id,
		clientSecret: secret,
		scopes:       c.Scopes,
		audience:     c.Audience,
		postAuth:     c.ClientAuth == clientAuthPost,
		hc:           hc,
		now:          time.Now,
	}, nil
}

// String names the variables and the token endpoint's host, never the
// values.
func (s *clientCredentials) String() string {
	return fmt.Sprintf("%s from $%s and $%s via %s", OAuth2ClientCredentials, s.idEnv, s.secretEnv, s.host)
}

// GoString keeps %#v from printing the secret.
func (s *clientCredentials) GoString() string { return s.String() }

// Token returns the cached token, or fetches one when there is none.
// A token due for refresh but not yet expired is returned at once while
// a new one is fetched in the background, so a slow or failing token
// endpoint delays no request until the token really runs out. Callers
// that arrive during a blocking fetch share it.
func (s *clientCredentials) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	now := s.now()
	if s.token != "" && (s.refresh.IsZero() || now.Before(s.refresh)) {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	if s.token != "" && now.Before(s.expiry) {
		tok := s.token
		if s.inflight == nil && !now.Before(s.nextTry) {
			s.inflight = &tokenCall{done: make(chan struct{})}
			go s.fetch(context.WithoutCancel(ctx), s.inflight)
		}
		s.mu.Unlock()
		return tok, nil
	}
	call := s.inflight
	if call == nil {
		call = &tokenCall{done: make(chan struct{})}
		s.inflight = call
		go s.fetch(context.WithoutCancel(ctx), call)
	}
	s.mu.Unlock()
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// invalidate drops tok if it is still the cached token and is old
// enough that a refusal can be about its age (minTokenAge), and reports
// whether it did. A token another caller has already replaced stays.
func (s *clientCredentials) invalidate(tok string) bool { //nolint:unparam // read through the invalidator interface by bearer
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != tok || s.now().Sub(s.issued) < minTokenAge {
		return false
	}
	s.token, s.refresh, s.expiry = "", time.Time{}, time.Time{}
	return true
}

func (s *clientCredentials) fetch(ctx context.Context, call *tokenCall) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	issued := s.now()
	tok, life, err := s.exchange(ctx)
	s.mu.Lock()
	if err == nil {
		s.token, s.issued, s.refresh, s.expiry = tok, issued, time.Time{}, time.Time{}
		if life > 0 {
			s.expiry = issued.Add(life)
			s.refresh = s.expiry.Add(-min(maxEarly, life/10))
		}
	} else {
		s.nextTry = s.now().Add(refreshBackoff)
	}
	s.inflight = nil
	s.mu.Unlock()
	call.token, call.err = tok, err
	close(call.done)
}

// exchange makes one client-credentials token request.
func (s *clientCredentials) exchange(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	if s.audience != "" {
		form.Set("audience", s.audience)
	}
	if s.postAuth {
		form.Set("client_id", s.clientID)
		form.Set("client_secret", s.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, s.fail(errors.New("building the token request failed"))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !s.postAuth {
		// RFC 6749 §2.3.1: form-encode each part before Basic encoding.
		req.SetBasicAuth(url.QueryEscape(s.clientID), url.QueryEscape(s.clientSecret))
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		// A *url.Error repeats the URL; the cause alone is enough.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", 0, s.fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, s.fail(fmt.Errorf("reading the answer: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := s.fail(fmt.Errorf("HTTP %d%s", resp.StatusCode, oauthError(body)))
		// A refusal (a wrong or rotated secret, an unknown client) will
		// be refused again; a timeout, rate limit or server error may not.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return "", 0, permanentError{err}
		}
		return "", 0, err
	}
	tok, typ, life, ok := parseTokenResponse(body, resp.Header.Get("Content-Type"))
	if !ok {
		return "", 0, s.fail(errors.New("the answer is not a token response"))
	}
	if tok == "" {
		return "", 0, s.fail(errors.New("the answer has no access_token"))
	}
	// Servers spell bearer variously (Apigee Edge says "BearerToken"), so
	// only types known to need more than a bearer header are refused.
	switch strings.ToLower(typ) {
	case "mac", "dpop", "pop":
		return "", 0, permanentError{s.fail(fmt.Errorf("token_type %q needs proof of possession, which is not supported", typ))}
	}
	return tok, life, nil
}

func (s *clientCredentials) fail(err error) error {
	return fmt.Errorf("auth: %s: token endpoint: %w", s, err)
}

// permanentError marks a token endpoint answer that retrying will not
// change. Package retry reads Permanent and does not resend the request.
type permanentError struct{ error }

func (e permanentError) Unwrap() error   { return e.error }
func (e permanentError) Permanent() bool { return true }

// parseTokenResponse reads an RFC 6749 §5.1 token response: JSON, or the
// form encoding some older servers send.
func parseTokenResponse(body []byte, contentType string) (token, typ string, life time.Duration, ok bool) {
	var tr struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err == nil {
		return tr.AccessToken, tr.TokenType, expiresIn(strings.Trim(string(tr.ExpiresIn), `"`)), true
	}
	ct, _, _ := mime.ParseMediaType(contentType)
	if ct != "application/x-www-form-urlencoded" && ct != "text/plain" {
		return "", "", 0, false
	}
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return "", "", 0, false
	}
	return v.Get("access_token"), v.Get("token_type"), expiresIn(v.Get("expires_in")), true
}

// expiresIn reads expires_in in seconds, which servers send as an
// integer, a decimal or a string of either. Zero means none was given.
func expiresIn(v string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f > maxLifetime.Seconds() {
		return maxLifetime
	}
	return time.Duration(f * float64(time.Second))
}

// oauthError is the RFC 6749 §5.2 error in a token endpoint's answer,
// for the message: ": invalid_client (Client authentication failed)".
// The fields describe the failure and carry no secret.
func oauthError(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return ""
	}
	out := ": " + e.Error
	if d := e.Description; d != "" {
		if len(d) > 200 {
			d = d[:200] + "…"
		}
		out += " (" + d + ")"
	}
	return out
}
