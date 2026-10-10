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

// Package gemini is core-models' Gemini adapter: the Gemini Developer
// API (API key) and Gemini on Vertex AI (Application Default
// Credentials), over genai, as llm.LLM.
//
// It is the extraction both products' copies were waiting on
// (docs/design.md §7.3, phase L5). The shape is mast's — an option
// struct, no registry, the usage.Detail sidecar — and the behaviour
// merges both:
//
//   - from mast: built-ins off unless asked for (#324, #340); the one
//     eviction verdict both the wrapper and the cache manager ask
//     (#325, vertexcache.Gone); skipping built-ins on a pre-3.0 model
//     carrying function tools;
//   - from core-agent: IncludeServerSideToolInvocations set from the
//     backend rather than by the caller, so the Developer API path can
//     no longer forget it (mast #505); per-request opt-outs through
//     callctx; the retry on a bare 400 after the session was served
//     (#898, #1247); the below-minimum cache verdict (#1067);
//   - from neither: a genai-native base model in place of ADK's
//     model/gemini, which the core module cannot import (model.go).
//
// Retries happen at the HTTP layer through package retry, like every
// core-models dialect: 408, 429 and 5xx, honoring Retry-After, plus
// the bare-400 rule as Policy.AfterSuccess. That replaces core-agent's
// model-level 429/503 predicate; a product's own outer policy, if it
// keeps one, now sees only what this layer gave up on.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
)

// Options configures a Client. Backend picks the service; the fields
// after it apply to one backend or the other.
type Options struct {
	// Backend is genai.BackendGeminiAPI or genai.BackendVertexAI.
	Backend genai.Backend

	// APIKey authenticates the Gemini Developer API.
	APIKey string

	// Project and Location address Vertex AI. "global" is a valid
	// location for current Gemini models.
	Project  string
	Location string
	// Credential authenticates Vertex AI requests as a bearer token. Nil
	// means Application Default Credentials, detected here.
	Credential *auth.Credential

	// BaseURL overrides the service endpoint, for a proxy, a private
	// endpoint or a test server. Empty means Google's.
	BaseURL string

	// HTTPClient supplies the base transport, for tracing or a proxy;
	// the retry and auth layers wrap it. Nil means
	// http.DefaultTransport.
	HTTPClient *http.Client
	// Retry is the HTTP-layer retry policy. Nil means retry.Default().
	// Either way, a nil AfterSuccess gets the bare-400 rule
	// (IsBareInvalidArgumentBody); to turn it off, supply an
	// AfterSuccess that returns false.
	Retry *retry.Policy

	// BackendName labels usage.Detail.Backend, the key products price
	// by. Empty means "gemini" for the Developer API and "vertex" for
	// Vertex AI, matching mast's and core-agent's pricing catalogs.
	BackendName string

	// BuiltinTools turns on Gemini's server-side tools. The zero value
	// is none, which is what both products start from: an unattended
	// workload's internet reachability should not depend on which
	// vendor it resolved to (mast #324).
	BuiltinTools BuiltinTools

	// ContextCacheModel, ContextCacheInit, ContextCacheName and
	// ContextCacheInvalidate wire Vertex explicit context caching — see
	// vertexcache.Manager, whose Model, Init, Name and MarkEvictedName
	// they usually are. A cache serves the one model it was created
	// for, so the hooks apply only to requests for ContextCacheModel,
	// which is required when any hook is set. They are skipped, too,
	// for a side call, a request that opted out of built-ins or prompt
	// caching, and one carrying a tool config (see cacheable). Ignored
	// on the Developer API, which rejects the cache reference on some
	// model families.
	ContextCacheModel      string
	ContextCacheInit       ContextCacheInitFn
	ContextCacheName       ContextCacheNameFn
	ContextCacheInvalidate ContextCacheInvalidateFn

	// Logf receives the adapter's operator-facing notices: an empty
	// response retried, a cache found evicted, built-ins skipped for an
	// old model. Nil discards them.
	Logf func(format string, args ...any)
}

// Client is a configured Gemini endpoint. Model returns an llm.LLM for
// one model id on it. Safe for concurrent use.
type Client struct {
	opts   Options
	client *genai.Client
}

// New builds a Client. On Vertex AI with no Credential it detects
// Application Default Credentials, and fails if there are none.
func New(ctx context.Context, opts Options) (*Client, error) {
	switch opts.Backend {
	case genai.BackendGeminiAPI:
		if opts.APIKey == "" {
			return nil, errors.New("gemini: an API key is required for the Gemini Developer API")
		}
		if opts.BackendName == "" {
			opts.BackendName = "gemini"
		}
	case genai.BackendVertexAI:
		if opts.Project == "" || opts.Location == "" {
			return nil, errors.New("gemini: Vertex AI needs a project and a location")
		}
		if opts.Credential == nil {
			cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(ctx, auth.Options{})
			if err != nil {
				return nil, fmt.Errorf("gemini: %w", err)
			}
			opts.Credential = cred
		}
		if opts.BackendName == "" {
			opts.BackendName = "vertex"
		}
	default:
		return nil, fmt.Errorf("gemini: backend %v is not the Developer API or Vertex AI", opts.Backend)
	}

	if (opts.ContextCacheInit != nil || opts.ContextCacheName != nil || opts.ContextCacheInvalidate != nil) && opts.ContextCacheModel == "" {
		return nil, errors.New("gemini: context-cache hooks need ContextCacheModel, the model the cache was created for")
	}

	base := opts.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	rt = mergeHeaders{base: rt}
	// Vertex authenticates per request with a bearer token. The
	// Developer API key is a header genai sets itself.
	if opts.Backend == genai.BackendVertexAI {
		rt = opts.Credential.BearerTransport(rt)
	}
	policy := retry.Default()
	if opts.Retry != nil {
		policy = *opts.Retry
	}
	if policy.AfterSuccess == nil {
		policy.AfterSuccess = IsBareInvalidArgumentBody
	}
	hc := *base
	hc.Transport = policy.Transport(rt)

	cc := &genai.ClientConfig{Backend: opts.Backend, HTTPClient: &hc}
	if opts.BaseURL != "" {
		cc.HTTPOptions.BaseURL = opts.BaseURL
	}
	if opts.Backend == genai.BackendGeminiAPI {
		cc.APIKey = opts.APIKey
	} else {
		// With an HTTPClient supplied and no Credentials or key, genai
		// skips its own ADC detection; the bearer transport above is
		// the authentication.
		cc.Project, cc.Location = opts.Project, opts.Location
	}
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	return &Client{opts: opts, client: client}, nil
}

// Model returns the model id as an llm.LLM carrying the Client's
// behaviour (see wrap.go).
func (c *Client) Model(id string) llm.LLM {
	isVertex := c.opts.Backend == genai.BackendVertexAI
	w := &model{
		inner: &baseModel{
			client:      c.client,
			name:        id,
			backendName: c.opts.BackendName,
			region:      c.opts.Location,
		},
		builtins: c.opts.BuiltinTools.asTools(),
		// The Developer API requires the flag whenever built-ins ride
		// with function tools; Vertex AI rejects the parameter and
		// allows the mix unconditionally (mast #505).
		directAPI: !isVertex,
		logf:      c.opts.Logf,
	}
	if isVertex {
		w.cacheModel = c.opts.ContextCacheModel
		w.cacheInit = c.opts.ContextCacheInit
		w.cacheName = c.opts.ContextCacheName
		w.cacheInvalidate = c.opts.ContextCacheInvalidate
	}
	return w
}

// Caches returns the genai caches service on the Client's endpoint and
// credentials, for building a vertexcache.Manager without a second
// client configuration.
func (c *Client) Caches() *genai.Caches { return c.client.Caches }

// BuiltinTools toggles Gemini's server-side tools. Each enabled flag
// becomes its own genai.Tool appended to a request's tools, alongside
// its function declarations.
//
// Other genai built-ins (FileSearch, GoogleMaps, ComputerUse,
// EnterpriseWebSearch, Retrieval) are not surfaced: they need an
// upstream resource configured first, so turning one on without it
// yields an API error rather than a tool.
type BuiltinTools struct {
	GoogleSearch  bool // web search grounding
	URLContext    bool // fetch and ground on URLs the model picks
	CodeExecution bool // sandboxed Python on Google's servers
}

// Built-in names shared across providers, so a product's config reads
// the same whichever vendor it resolves to.
const (
	WebSearch     = "web_search"
	URLContext    = "url_context"
	CodeExecution = "code_execution"
)

// BuiltinToolsFromNames maps provider-neutral names onto the toggles.
// An unknown name is an error, not a silently absent tool.
func BuiltinToolsFromNames(names []string) (BuiltinTools, error) {
	var b BuiltinTools
	for _, n := range names {
		switch n {
		case WebSearch:
			b.GoogleSearch = true
		case URLContext:
			b.URLContext = true
		case CodeExecution:
			b.CodeExecution = true
		default:
			return BuiltinTools{}, fmt.Errorf("gemini: unknown built-in tool %q (have %s, %s, %s)", n, WebSearch, URLContext, CodeExecution)
		}
	}
	return b, nil
}

// Names reports the enabled built-ins under the neutral names, in a
// fixed order.
func (b BuiltinTools) Names() []string { return builtinNames(b.asTools()) }

// asTools projects the toggles in field order, so the request shape is
// stable across calls — it matters for prompt caching.
func (b BuiltinTools) asTools() []*genai.Tool {
	var out []*genai.Tool
	if b.GoogleSearch {
		out = append(out, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	if b.URLContext {
		out = append(out, &genai.Tool{URLContext: &genai.URLContext{}})
	}
	if b.CodeExecution {
		out = append(out, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
	}
	return out
}

// builtinNames reads the names back off the projected tools, so a
// report and a request cannot disagree about what is on.
func builtinNames(tools []*genai.Tool) []string {
	var out []string
	for _, t := range tools {
		switch {
		case t.GoogleSearch != nil:
			out = append(out, WebSearch)
		case t.URLContext != nil:
			out = append(out, URLContext)
		case t.CodeExecution != nil:
			out = append(out, CodeExecution)
		}
	}
	return slices.Clip(out)
}
