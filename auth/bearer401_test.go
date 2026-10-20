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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-models/retry"
)

// gateway is a fake model endpoint that accepts only the tokens in ok.
type gateway struct {
	mu     sync.Mutex
	ok     map[string]bool
	auths  []string
	bodies []string
	srv    *httptest.Server
}

func newGateway(t *testing.T, ok ...string) *gateway {
	t.Helper()
	g := &gateway{ok: map[string]bool{}}
	for _, k := range ok {
		g.ok[k] = true
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		g.auths = append(g.auths, tok)
		g.bodies = append(g.bodies, string(b))
		accepted := g.ok[tok]
		g.mu.Unlock()
		if !accepted {
			http.Error(w, `{"error":"token refused"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auths...)
}

// client is cred's BearerTransport inside the retry layer, as every
// dialect builds it.
func client(cred *Credential) *http.Client {
	p := retry.Default()
	p.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return &http.Client{Transport: p.Transport(cred.BearerTransport(nil))}
}

func post(t *testing.T, hc *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	return hc.Do(req)
}

func credFor(t *testing.T, ts *tokenServer) (*Credential, *clientCredentials, *fakeClock) {
	t.Helper()
	s, clk := source(t, ts, nil)
	token(t, s) // tok-1, as Resolve fetches it
	return &Credential{kind: OAuth2ClientCredentials, tokens: s, describe: s.String()}, s, clk
}

func TestA401OnAnAgedTokenFetchesANewOneAndResendsOnce(t *testing.T) {
	ts := newTokenServer(t, "")
	cred, _, clk := credFor(t, ts)
	clk.add(time.Hour) // revoked early by the gateway, which accepts only tok-2
	g := newGateway(t, "tok-2")
	resp, err := post(t, client(cred), g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := g.seen(); len(got) != 2 || got[0] != "tok-1" || got[1] != "tok-2" {
		t.Fatalf("tokens sent = %q, want tok-1 then tok-2", got)
	}
	if g.bodies[1] != `{"model":"m"}` {
		t.Fatalf("the resent request lost its body: %q", g.bodies[1])
	}
}

func TestA401ThatPersistsIsReturnedAfterOneResend(t *testing.T) {
	ts := newTokenServer(t, "")
	cred, _, clk := credFor(t, ts)
	clk.add(time.Hour)
	g := newGateway(t) // refuses everything
	resp, err := post(t, client(cred), g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(g.seen()) != 2 || ts.requests() != 2 {
		t.Fatalf("status %d after %d requests and %d token requests: want the 401 after one re-fetch and one resend",
			resp.StatusCode, len(g.seen()), ts.requests())
	}
}

// A token fetched moments ago that the server refuses is a refusal of
// the client, not of the token's age: no new token per request.
func TestA401OnAFreshTokenIsNotRefetched(t *testing.T) {
	ts := newTokenServer(t, "")
	cred, _, _ := credFor(t, ts)
	g := newGateway(t)
	hc := client(cred)
	for range 5 {
		resp, err := post(t, hc, g.srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(g.seen()) != 5 || ts.requests() != 1 {
		t.Fatalf("%d requests, %d token requests: want 5 and 1", len(g.seen()), ts.requests())
	}
}

func TestA401WithABodyThatCannotBeResentStillDropsTheToken(t *testing.T) {
	ts := newTokenServer(t, "")
	cred, s, clk := credFor(t, ts)
	clk.add(time.Hour)
	g := newGateway(t, "tok-2")
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL, io.NopCloser(strings.NewReader("x")))
	req.GetBody = nil
	resp, err := cred.BearerTransport(nil).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(g.seen()) != 1 {
		t.Fatalf("status %d after %d requests: want the 401, not resent", resp.StatusCode, len(g.seen()))
	}
	if got := token(t, s); got != "tok-2" {
		t.Fatalf("next token = %q, want tok-2: the refused one must be dropped", got)
	}
}

func TestARefusedSecretSurfacesAndIsNotRetried(t *testing.T) {
	ts := newTokenServer(t, "")
	cred, _, clk := credFor(t, ts)
	clk.add(time.Hour)
	ts.fail.Store(true) // the secret was rotated: invalid_client
	g := newGateway(t)
	_, err := post(t, client(cred), g.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "refused the token (401)") || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("err = %v, want the token endpoint's reason", err)
	}
	if n := ts.requests(); n != 2 {
		t.Fatalf("token requests = %d, want 2: the retry layer must not resend a refused secret", n)
	}
	if n := len(g.seen()); n != 1 {
		t.Fatalf("gateway requests = %d, want 1", n)
	}
}
