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

package profile

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// reservedHeaders are the headers the library or a vendor SDK sets
// itself, which Headers may not set. Credentials stay in auth, so a
// profile cannot silently replace the one Resolve checked; the rest are
// framing and identification the dialects own. Canonical form.
var reservedHeaders = []string{
	// Credentials: auth's kinds, Azure's api-key, and Vertex's quota
	// project, which auth.BearerTransport sets beside the token.
	"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key", "X-Goog-User-Project",
	// Framing every dialect sets per request.
	"Content-Type", "Content-Length", "Content-Encoding", "Accept", "Accept-Encoding",
	"Host", "Connection", "Transfer-Encoding",
	// Hop-by-hop and protocol switches that change how the transport
	// behaves: an Expect waits for a 100, an Upgrade tries a new
	// protocol, neither of which a dialect expects.
	"Te", "Trailer", "Upgrade", "Expect", "Keep-Alive", "Proxy-Connection",
	// Identification: gemini joins its own and genai's values (see
	// dialect/gemini's mergeHeaders); the Anthropic SDK pins its API
	// version. anthropic-beta stays settable: it is how a profile opts
	// into a beta, and no dialect sets it.
	"User-Agent", "X-Goog-Api-Client", "Anthropic-Version",
}

// validHeaderName reports whether name is an HTTP token (RFC 9110
// §5.6.2), the only characters a field name may hold.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 0x80 || !strings.ContainsRune("!#$%&'*+-.^_`|~", r) &&
			(r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// validHeaderValue reports whether v can be sent as a field value: no
// control characters except tab, so an expanded variable cannot smuggle
// in a second header.
func validHeaderValue(v string) bool {
	for _, r := range v {
		if r == 0x7f || r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

// checkHeaders reports what is wrong with a profile's Headers, naming
// headers and never their values, which may be secrets.
func checkHeaders(h map[string]string) []string {
	var problems []string
	seen := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(h)) {
		canon := http.CanonicalHeaderKey(name)
		switch {
		case !validHeaderName(name):
			problems = append(problems, fmt.Sprintf("headers: %q is not a valid header name", name))
			continue
		case slices.Contains(reservedHeaders, canon):
			problems = append(problems, fmt.Sprintf("headers: %s is set by the library (credentials belong in auth), not by a profile", canon))
			continue
		case seen[canon] != "":
			problems = append(problems, fmt.Sprintf("headers: %q and %q are the same header", seen[canon], name))
			continue
		}
		seen[canon] = name
		// The literal parts only: a ${VAR} is checked once it is read.
		if !validHeaderValue(stripEnv(h[name])) {
			problems = append(problems, fmt.Sprintf("headers.%s: the value contains a control character", canon))
		}
	}
	return problems
}

// resolvedHeaders holds the read values; see Resolved.headers.
type resolvedHeaders struct{ h http.Header }

// hasHeader reports whether h names the header name, in any case.
func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// headerTransport sets a profile's headers on every request. It sits
// under the retry layer, so each attempt carries them.
type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

// RoundTrip adds the headers to a copy of req. A header the request
// already carries keeps its value, so a per-call setting (a genai
// HTTPOptions header, an anthropic-beta for one request) wins over the
// profile's default. On a redirect the headers go only to the first
// request's scheme and host: the transport sees every hop, and a
// gateway's subscription key must not follow a 307 to another host.
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameOriginAsFirstHop(req) {
		return t.base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	if req.Header == nil {
		req.Header = http.Header{}
	}
	for k, v := range t.headers {
		if len(req.Header.Values(k)) == 0 {
			req.Header[k] = slices.Clone(v)
		}
	}
	return t.base.RoundTrip(req)
}

// sameOriginAsFirstHop reports whether req goes to the scheme and host
// of the request that started its redirect chain (itself, when it is not
// a redirect).
func sameOriginAsFirstHop(req *http.Request) bool {
	first := req
	for first.Response != nil && first.Response.Request != nil {
		first = first.Response.Request
	}
	return first == req || strings.EqualFold(first.URL.Scheme, req.URL.Scheme) && strings.EqualFold(first.URL.Host, req.URL.Host)
}

// HeaderTransport returns base with the profile's headers set on every
// request, or base itself when the profile has none. Nil base means
// http.DefaultTransport.
//
// Open puts it under every dialect's transport, so the headers reach the
// model endpoint and nothing else: not a token endpoint, not another
// host a redirect points to, and a metrics endpoint only when it shares
// the API's origin (package kvmetrics). A host that builds a dialect
// itself rather than through Open (gemini.New, to share one client for
// its context cache) wraps the HTTPClient it passes with this.
func (r *Resolved) HeaderTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if r == nil || r.headers == nil {
		return base
	}
	return &headerTransport{base: base, headers: r.headers.h}
}

// HeaderNames lists the headers the profile sends, canonical and
// sorted, without their values, which may be secrets.
func (r *Resolved) HeaderNames() []string {
	if r == nil || r.headers == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(r.headers.h))
}

// resolveHeaders reads every ${VAR} in h. A header whose value comes out
// empty is not sent, which is also how a profile drops one it inherited
// through extends.
func resolveHeaders(h map[string]string, getenv func(string) string) (*resolvedHeaders, error) {
	if len(h) == 0 {
		return nil, nil
	}
	out := http.Header{}
	for _, name := range slices.Sorted(maps.Keys(h)) {
		canon := http.CanonicalHeaderKey(name)
		v, err := expandEnv(h[name], getenv)
		if err != nil {
			return nil, fmt.Errorf("headers.%s: %w", canon, err)
		}
		if !validHeaderValue(v) {
			return nil, fmt.Errorf("headers.%s: the value contains a control character", canon)
		}
		if v != "" {
			out[canon] = []string{v}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &resolvedHeaders{h: out}, nil
}
