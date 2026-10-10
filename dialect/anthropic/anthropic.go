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

// Originally derived from go-steer/mast@6568618f531368b8fa4894645df292b1e2aca09f:internal/providers/anthropic/anthropic.go
// Originally derived from go-steer/core-agent@869cc645952b49b81afda60c0b43e14a2f7c60b4:pkg/models/anthropic/anthropic.go

// Package anthropic is the anthropic dialect: Claude through the
// Messages API, first-party and on Vertex AI (docs/design.md §7.3,
// phase L4).
//
// It adapts the official SDK (github.com/anthropics/anthropic-sdk-go):
// a genai-shaped llm.Request becomes a MessageNewParams, and the SSE
// stream is accumulated back into genai-shaped Responses. The shape is
// mast's — option structs, no registry, usage in usage.Detail — and the
// behavior core-agent built after the two copies split is ported onto
// it: rolling and one-hour prompt caching (cache.go), per-call opt-outs
// through callctx, and the positional schema normalizer. The
// thinking-request shape is mast's, chosen per model generation.
//
// # Retries
//
// The SDK's own retries are off. Package retry's Transport does it in
// the HTTP client instead, as for every other dialect, so a 429 is
// handled the same way whichever model a workload names and each
// attempt carries a fresh credential.
//
// # Vertex AI
//
// Claude on Vertex is the same Messages body sent to a different URL:
// the model moves from the body into the path, and the body carries an
// anthropic_version. Options.Vertex turns that rewrite on; see
// vertex.go. It is done here rather than through the SDK's vertex
// package so that the credential is core-models' own (package auth)
// and the core module takes no golang.org/x/oauth2 or
// google.golang.org/api dependency.
package anthropic

import (
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
)

// DefaultBaseURL is the first-party API root.
const DefaultBaseURL = "https://api.anthropic.com"

// DefaultMaxTokens caps a single response when the request sets no
// MaxOutputTokens. 16K is plenty for most turns and well under the
// streaming HTTP timeouts.
const DefaultMaxTokens = 16_384

// Options configures a Client. A coremodels.Open caller never builds
// one by hand; it comes from a resolved profile.
type Options struct {
	// BaseURL is the API root. Empty means DefaultBaseURL. With Vertex
	// set it is the publisher prefix
	// .../projects/{p}/locations/{r}/publishers/anthropic/models, and a
	// request goes to BaseURL/{model}:streamRawPredict.
	BaseURL string
	// Credential authenticates every request: an api_key credential as
	// x-api-key, as the first-party API expects, and any other kind as
	// a bearer token (Google ADC on Vertex, or a gateway's token). Nil
	// sends none.
	Credential *auth.Credential
	// Vertex routes requests the way Vertex AI serves Claude.
	Vertex bool
	// HTTPClient supplies the base transport and timeout. Nil means a
	// client with http.DefaultTransport.
	HTTPClient *http.Client
	// Retry is the HTTP-layer retry policy. Nil means retry.Default().
	Retry *retry.Policy

	// Backend and Region are stamped into every usage.Detail.
	Backend string
	Region  string

	// Cache is the prompt-caching policy. The zero value places no
	// breakpoints; coremodels.Open passes DefaultCacheOptions unless
	// told otherwise.
	Cache CacheOptions
	// BuiltinTools are Anthropic's server-side tools, all off unless
	// set (mast #340).
	BuiltinTools BuiltinTools
}

// Client holds one configured SDK client. It is safe for concurrent
// use, and every model it returns shares its connection pool.
type Client struct {
	opts Options
	sdk  anthropic.Client
}

// New returns a Client for opts.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		if opts.Vertex {
			return nil, errors.New("anthropic: Vertex needs a BaseURL (the publishers/anthropic/models prefix)")
		}
		base = DefaultBaseURL
	}
	hc := &http.Client{}
	if opts.HTTPClient != nil {
		c := *opts.HTTPClient
		hc = &c
	}
	rt := hc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	sdkOpts := []option.RequestOption{
		// The profile is the whole configuration: no ANTHROPIC_API_KEY,
		// ANTHROPIC_BASE_URL or SDK config profile may reach in from the
		// environment behind it.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(base),
		option.WithMaxRetries(0),
	}
	if c := opts.Credential; c != nil {
		if c.Kind() == auth.APIKey {
			sdkOpts = append(sdkOpts, option.WithAPIKey(c.Secret()))
		} else {
			rt = c.BearerTransport(rt)
		}
	}
	policy := retry.Default()
	if opts.Retry != nil {
		policy = *opts.Retry
	}
	hc.Transport = policy.Transport(rt)
	sdkOpts = append(sdkOpts, option.WithHTTPClient(hc))
	if opts.Vertex {
		sdkOpts = append(sdkOpts, option.WithMiddleware(vertexMiddleware(base)))
	}
	return &Client{opts: opts, sdk: anthropic.NewClient(sdkOpts...)}, nil
}

// Model returns the model id as an llm.LLM. Vertex AI sometimes serves
// Claude under a date-suffixed id ("claude-opus-4-5@20251101"); pass the
// id the backend expects, since it goes into the URL verbatim.
func (c *Client) Model(id string) llm.LLM {
	return &model{
		client:   c.sdk,
		modelID:  id,
		cache:    c.opts.Cache,
		builtins: c.opts.BuiltinTools,
		backend:  c.opts.Backend,
		region:   c.opts.Region,
	}
}
