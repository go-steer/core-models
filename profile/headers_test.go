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

package profile_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
)

func withHeaders(h map[string]string) profile.Profile {
	return profile.Profile{Name: "gw", Dialect: profile.OpenAIChat, BaseURL: "https://gw.example/v1",
		Auth: auth.Config{Kind: auth.None}, OpenModels: new(true), Headers: h}
}

// TestHeadersValidate: credentials and the headers dialects own are
// refused whatever their case, as are names that are not HTTP tokens,
// two spellings of one header, and a literal control character. No
// message carries a value.
func TestHeadersValidate(t *testing.T) {
	for name, c := range map[string]struct {
		h    map[string]string
		want string
	}{
		"authorization":         {map[string]string{"authorization": "Bearer s3cret"}, "Authorization is set by the library"},
		"x-api-key":             {map[string]string{"X-API-KEY": "s3cret"}, "X-Api-Key is set by the library"},
		"azure api-key":         {map[string]string{"api-key": "s3cret"}, "Api-Key is set by the library"},
		"goog key":              {map[string]string{"x-goog-api-key": "s3cret"}, "X-Goog-Api-Key is set by the library"},
		"quota project":         {map[string]string{"x-goog-user-project": "p"}, "X-Goog-User-Project is set by the library"},
		"content-type":          {map[string]string{"Content-Type": "text/plain"}, "Content-Type is set by the library"},
		"accept":                {map[string]string{"accept": "*/*"}, "Accept is set by the library"},
		"user-agent":            {map[string]string{"user-agent": "me"}, "User-Agent is set by the library"},
		"anthropic-version":     {map[string]string{"anthropic-version": "2023-06-01"}, "Anthropic-Version is set by the library"},
		"bad name":              {map[string]string{"X Tenant": "a"}, `"X Tenant" is not a valid header name`},
		"empty name":            {map[string]string{"": "a"}, `"" is not a valid header name`},
		"two spellings":         {map[string]string{"X-Tenant": "a", "x-tenant": "b"}, "are the same header"},
		"literal CRLF in value": {map[string]string{"X-Tenant": "a\r\nX-Evil: s3cret"}, "headers.X-Tenant: the value contains a control character"},
	} {
		t.Run(name, func(t *testing.T) {
			err := withHeaders(c.h).Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, c.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error carries a value: %v", err)
			}
		})
	}
	if err := withHeaders(map[string]string{"AI-Resource-Group": "${G:-default}", "anthropic-beta": "x", "Ocp-Apim-Subscription-Key": "${K}"}).Validate(); err != nil {
		t.Errorf("ordinary headers refused: %v", err)
	}
}

// TestHeadersResolve: values are read from the environment, with or
// without a default; a missing variable fails naming the header and the
// variable; an empty result is not sent; HeaderNames lists names only;
// and a value never shows in a printed Resolved or an error.
func TestHeadersResolve(t *testing.T) {
	env := map[string]string{"TENANT": "acme", "SUB": "s3cret", "EVIL": "a\nb"}
	getenv := func(k string) string { return env[k] }
	resolve := func(h map[string]string) (*profile.Resolved, error) {
		return profile.Resolve(context.Background(), withHeaders(h), profile.Options{Getenv: getenv})
	}

	r, err := resolve(map[string]string{"x-tenant": "${TENANT}", "X-Sub": "${SUB}", "X-Region": "${REGION:-eu}", "X-Off": "${NOPE:-}"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.HeaderNames(); !slices.Equal(got, []string{"X-Region", "X-Sub", "X-Tenant"}) {
		t.Errorf("HeaderNames = %v", got)
	}
	for _, s := range []string{fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", *r)} {
		if strings.Contains(s, "s3cret") || strings.Contains(s, "acme") {
			t.Errorf("a printed Resolved carries a header value: %s", s)
		}
	}

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) { got = req.Header.Clone() }))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept", "text/plain")
	resp, err := (&http.Client{Transport: r.HeaderTransport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("X-Tenant") != "acme" || got.Get("X-Sub") != "s3cret" || got.Get("X-Region") != "eu" || got.Get("Accept") != "text/plain" {
		t.Errorf("sent headers %v", got)
	}
	if _, ok := got["X-Off"]; ok {
		t.Error("X-Off sent though it resolved empty")
	}
	if req.Header.Get("X-Tenant") != "" {
		t.Error("HeaderTransport modified the caller's request")
	}

	_, err = resolve(map[string]string{"X-Sub": "${MISSING}"})
	if err == nil || !strings.Contains(err.Error(), "headers.X-Sub") || !strings.Contains(err.Error(), "MISSING is not set") {
		t.Errorf("missing variable: err = %v", err)
	}
	_, err = resolve(map[string]string{"X-Evil": "${EVIL}"})
	if err == nil || !strings.Contains(err.Error(), "headers.X-Evil: the value contains a control character") {
		t.Errorf("control character after expansion: err = %v", err)
	}

	none, err := resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := http.DefaultTransport; none.HeaderTransport(base) != base || none.HeaderNames() != nil {
		t.Error("a profile without headers should leave the transport alone")
	}
}

// TestHeaderTransportRules: a header the request already carries keeps
// its value; a redirect to another host gets no profile headers while one
// back to the same host does; a request with a nil Header does not panic;
// and the transport can be compared like any other.
func TestHeaderTransportRules(t *testing.T) {
	r, err := profile.Resolve(context.Background(), withHeaders(map[string]string{"X-Sub": "s3cret", "X-Tenant": "acme"}), profile.Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	type hit struct{ path, sub, tenant string }
	var hits []hit
	record := func(w http.ResponseWriter, req *http.Request) {
		hits = append(hits, hit{req.URL.Path, req.Header.Get("X-Sub"), req.Header.Get("X-Tenant")})
	}
	other := httptest.NewServer(http.HandlerFunc(record))
	defer other.Close()
	var self *httptest.Server
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		record(w, req)
		switch req.URL.Path {
		case "/away":
			http.Redirect(w, req, other.URL+"/landed", http.StatusTemporaryRedirect)
		case "/home":
			http.Redirect(w, req, self.URL+"/landed", http.StatusTemporaryRedirect)
		}
	}))
	defer self.Close()
	client := &http.Client{Transport: r.HeaderTransport(nil)}
	get := func(path string, set map[string]string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, self.URL+path, nil)
		for k, v := range set {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	get("/away", nil)
	if len(hits) != 2 || hits[0] != (hit{"/away", "s3cret", "acme"}) || hits[1] != (hit{"/landed", "", ""}) {
		t.Errorf("cross-host redirect: %+v, want the headers on the first hop only", hits)
	}
	hits = nil
	get("/home", nil)
	if len(hits) != 2 || hits[1] != (hit{"/landed", "s3cret", "acme"}) {
		t.Errorf("same-host redirect: %+v, want the headers on both hops", hits)
	}
	hits = nil
	get("/", map[string]string{"X-Tenant": "per-call"})
	if len(hits) != 1 || hits[0] != (hit{"/", "s3cret", "per-call"}) {
		t.Errorf("per-call header: %+v, want the request's own X-Tenant kept", hits)
	}

	hits = nil
	u, _ := url.Parse(self.URL + "/raw")
	resp, err := r.HeaderTransport(nil).RoundTrip(&http.Request{Method: http.MethodGet, URL: u})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(hits) != 1 || hits[0].sub != "s3cret" {
		t.Errorf("nil-Header request: %+v", hits)
	}

	if a, b := r.HeaderTransport(nil), r.HeaderTransport(nil); a == b {
		t.Error("two transports compared equal") // the point is that == does not panic
	}
}
