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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testID     = "client-7"
	testSecret = "s3cret/with+chars"
)

// tokenServer is a fake token endpoint. Each request gets the next token
// ("tok-1", "tok-2", …) with expires_in, unless fail is set.
type tokenServer struct {
	t         *testing.T
	srv       *httptest.Server
	n         atomic.Int32
	expiresIn string // raw JSON value; "" omits the field
	delay     time.Duration
	fail      atomic.Bool
	mu        sync.Mutex
	forms     []url.Values
	auths     []string
}

func newTokenServer(t *testing.T, expiresIn string) *tokenServer {
	t.Helper()
	ts := &tokenServer{t: t, expiresIn: expiresIn}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		ts.mu.Lock()
		ts.forms = append(ts.forms, form)
		ts.auths = append(ts.auths, r.Header.Get("Authorization"))
		ts.mu.Unlock()
		if ts.delay > 0 {
			time.Sleep(ts.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		if ts.fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"Client authentication failed"}`)
			return
		}
		n := ts.n.Add(1)
		exp := ""
		if ts.expiresIn != "" {
			exp = `,"expires_in":` + ts.expiresIn
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer"%s}`, n, exp)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) requests() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.forms)
}

func (ts *tokenServer) config() Config {
	return Config{
		Kind:            OAuth2ClientCredentials,
		TokenURL:        ts.srv.URL + "/oauth/token",
		ClientIDEnv:     "GW_ID",
		ClientSecretEnv: "GW_SECRET",
	}
}

func testEnv(k string) string {
	return map[string]string{"GW_ID": testID, "GW_SECRET": testSecret}[k]
}

// fakeClock is a settable now().
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func source(t *testing.T, ts *tokenServer, mod func(*Config)) (*clientCredentials, *fakeClock) {
	t.Helper()
	c := ts.config()
	if mod != nil {
		mod(&c)
	}
	s, err := newClientCredentials(c, testEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s.now = clk.now
	return s, clk
}

func token(t *testing.T, s *clientCredentials) string {
	t.Helper()
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestClientCredentialsRefreshesBeforeExpiry(t *testing.T) {
	ts := newTokenServer(t, "3600")
	s, clk := source(t, ts, nil)
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("first token = %q", got)
	}
	// A one-hour token refreshes a minute early (min(1m, 6m)).
	clk.add(58 * time.Minute)
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("at 58m token = %q, want the cached tok-1", got)
	}
	// Due for refresh: the still-valid token comes back at once while a
	// new one is fetched in the background.
	clk.add(90 * time.Second)
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("at 59m30s token = %q, want tok-1 returned without waiting", got)
	}
	waitFor(t, s, "tok-2")
	if n := ts.n.Load(); n != 2 {
		t.Fatalf("token requests = %d, want 2", n)
	}
}

// waitFor polls until the background refresh has installed want.
func waitFor(t *testing.T, s *clientCredentials, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := token(t, s); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("token never became %q", want)
}

func TestClientCredentialsShortLivedTokenRefreshesATenthEarly(t *testing.T) {
	ts := newTokenServer(t, `"300"`) // a string, as some servers send it
	s, clk := source(t, ts, nil)
	token(t, s)
	clk.add(269 * time.Second)
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("at 269s token = %q, want tok-1", got)
	}
	clk.add(2 * time.Second)
	token(t, s) // starts the refresh 30s before a 300s expiry
	waitFor(t, s, "tok-2")
}

func TestClientCredentialsWithoutExpiryKeepsTheTokenUntilRefused(t *testing.T) {
	ts := newTokenServer(t, "")
	s, clk := source(t, ts, nil)
	token(t, s)
	clk.add(48 * time.Hour)
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("token = %q, want tok-1 kept", got)
	}
	s.invalidate("tok-1")
	if got := token(t, s); got != "tok-2" {
		t.Fatalf("after invalidate token = %q, want tok-2", got)
	}
	// A stale invalidate (another caller already replaced it) is a no-op.
	s.invalidate("tok-1")
	if got := token(t, s); got != "tok-2" {
		t.Fatalf("after a stale invalidate token = %q, want tok-2 kept", got)
	}
}

func TestClientCredentialsConcurrentCallersShareOneRefresh(t *testing.T) {
	ts := newTokenServer(t, "3600")
	ts.delay = 50 * time.Millisecond
	s, _ := source(t, ts, nil)
	var wg sync.WaitGroup
	got := make([]string, 50)
	for i := range got {
		wg.Go(func() {
			tok, err := s.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			got[i] = tok
		})
	}
	wg.Wait()
	if n := ts.n.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1 shared by 50 callers", n)
	}
	for i, tok := range got {
		if tok != "tok-1" {
			t.Fatalf("caller %d got %q", i, tok)
		}
	}
}

func TestClientCredentialsFailedEarlyRefreshKeepsTheLiveToken(t *testing.T) {
	ts := newTokenServer(t, "3600")
	ts.delay = 20 * time.Millisecond
	s, clk := source(t, ts, nil)
	token(t, s)
	ts.fail.Store(true)
	clk.add(59*time.Minute + 30*time.Second) // due for refresh, not expired
	if got := token(t, s); got != "tok-1" {
		t.Fatalf("token = %q, want tok-1 while it is still valid", got)
	}
	// Let the background refresh fail, then show the backoff holds:
	// a burst of requests asks the token endpoint nothing more.
	deadline := time.Now().Add(5 * time.Second)
	for ts.requests() < 2 || s.inflightNow() {
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for range 20 {
		if got := token(t, s); got != "tok-1" {
			t.Fatalf("token = %q during backoff", got)
		}
	}
	if n := ts.requests(); n != 2 {
		t.Fatalf("token requests = %d, want 2: no retry inside the backoff", n)
	}
	clk.add(time.Minute) // now expired
	if _, err := s.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("Token after expiry = %v, want the token endpoint's error", err)
	}
}

func TestClientCredentialsRequestShape(t *testing.T) {
	ts := newTokenServer(t, "3600")
	s, _ := source(t, ts, func(c *Config) {
		c.Scopes = []string{"models.invoke", "models.read"}
		c.Audience = "https://api.example.com"
	})
	token(t, s)
	f, a := ts.forms[0], ts.auths[0]
	if f.Get("grant_type") != "client_credentials" || f.Get("scope") != "models.invoke models.read" || f.Get("audience") != "https://api.example.com" {
		t.Fatalf("form = %v", f)
	}
	if f.Has("client_id") || f.Has("client_secret") {
		t.Fatalf("basic auth put credentials in the form: %v", f)
	}
	req := &http.Request{Header: http.Header{"Authorization": {a}}}
	id, secret, ok := req.BasicAuth()
	if !ok || id != testID || secret != url.QueryEscape(testSecret) {
		t.Fatalf("basic auth = %q %q %v, want the form-encoded id and secret", id, secret, ok)
	}

	ts2 := newTokenServer(t, "3600")
	s2, _ := source(t, ts2, func(c *Config) { c.ClientAuth = clientAuthPost })
	token(t, s2)
	if f := ts2.forms[0]; f.Get("client_id") != testID || f.Get("client_secret") != testSecret || ts2.auths[0] != "" {
		t.Fatalf("post auth: form = %v, Authorization = %q", f, ts2.auths[0])
	}
}

func TestClientCredentialsNeverShowsTheSecret(t *testing.T) {
	ts := newTokenServer(t, "3600")
	c := ts.config()
	cred, err := c.Resolve(context.Background(), Options{Getenv: testEnv})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := cred.Token(context.Background())
	for _, s := range []string{
		cred.String(), fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred), fmt.Sprintf("%#v", cred.tokens),
		fmt.Sprintf("%v", cred.tokens), fmt.Sprintf("%+v", cred.tokens),
	} {
		if strings.Contains(s, testSecret) || strings.Contains(s, tok) || strings.Contains(s, testID) {
			t.Fatalf("%q shows the id, secret or token", s)
		}
	}
	if got := cred.String(); got != "oauth2_client_credentials from $GW_ID and $GW_SECRET via "+ts.srv.Listener.Addr().String() {
		t.Fatalf("String = %q", got)
	}
	if cred.Secret() != "" {
		t.Fatal("Secret() exposes the client secret")
	}

	ts.fail.Store(true)
	_, err = ts.config().Resolve(context.Background(), Options{Getenv: testEnv})
	if err == nil {
		t.Fatal("Resolve with a refused secret succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, testSecret) || !strings.Contains(msg, "invalid_client") || !strings.Contains(msg, "$GW_SECRET") {
		t.Fatalf("error = %q: want the endpoint's reason and the variable names, never the secret", msg)
	}
}

func TestClientCredentialsFailsAtResolve(t *testing.T) {
	ts := newTokenServer(t, "3600")
	for name, tc := range map[string]struct {
		c      Config
		getenv func(string) string
		want   string
	}{
		"missing secret": {ts.config(), func(k string) string { return map[string]string{"GW_ID": "x"}[k] }, "GW_SECRET is not set"},
		"missing both":   {ts.config(), func(string) string { return "" }, "GW_ID and GW_SECRET is not set"},
		"not a URL":      {Config{Kind: OAuth2ClientCredentials, TokenURL: "ftp://x", ClientIDEnv: "GW_ID", ClientSecretEnv: "GW_SECRET"}, testEnv, "not an http or https URL"},
		"unreachable":    {Config{Kind: OAuth2ClientCredentials, TokenURL: "http://127.0.0.1:1/token", ClientIDEnv: "GW_ID", ClientSecretEnv: "GW_SECRET"}, testEnv, "via 127.0.0.1:1: token endpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.c.Resolve(context.Background(), Options{Getenv: tc.getenv})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if n := ts.n.Load(); n != 0 {
		t.Fatalf("token requests = %d for configurations that should fail first", n)
	}
}

func (s *clientCredentials) inflightNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight != nil
}

func TestTokenResponseShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body, ct string
		tok      string
		life     time.Duration
		ok       bool
	}{
		"json":          {`{"access_token":"a","token_type":"bearer","expires_in":3600}`, "application/json", "a", time.Hour, true},
		"apigee":        {`{"access_token":"a","token_type":"BearerToken","expires_in":"3599"}`, "application/json;charset=UTF-8", "a", 3599 * time.Second, true},
		"decimal":       {`{"access_token":"a","expires_in":3600.0}`, "text/plain", "a", time.Hour, true},
		"json as text":  {`{"access_token":"a"}`, "text/plain", "a", 0, true},
		"form":          {`access_token=a&token_type=bearer&expires_in=60`, "application/x-www-form-urlencoded", "a", time.Minute, true},
		"huge expiry":   {`{"access_token":"a","expires_in":1e30}`, "application/json", "a", maxLifetime, true},
		"html":          {`<html>login</html>`, "text/html", "", 0, false},
		"negative":      {`{"access_token":"a","expires_in":-5}`, "application/json", "a", 0, true},
		"nan as string": {`{"access_token":"a","expires_in":"NaN"}`, "application/json", "a", 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			tok, _, life, ok := parseTokenResponse([]byte(tc.body), tc.ct)
			if ok != tc.ok || tok != tc.tok || life != tc.life {
				t.Fatalf("parse = %q, %v, %v; want %q, %v, %v", tok, life, ok, tc.tok, tc.life, tc.ok)
			}
		})
	}
}

func TestProofOfPossessionTokensAreRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a","token_type":"DPoP"}`)
	}))
	t.Cleanup(srv.Close)
	c := Config{Kind: OAuth2ClientCredentials, TokenURL: srv.URL, ClientIDEnv: "GW_ID", ClientSecretEnv: "GW_SECRET"}
	if _, err := c.Resolve(context.Background(), Options{Getenv: testEnv}); err == nil || !strings.Contains(err.Error(), "proof of possession") {
		t.Fatalf("Resolve = %v", err)
	}
}
