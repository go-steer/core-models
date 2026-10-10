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

// Package coremodels opens a provider profile: it resolves the profile
// against the environment, picks the dialect its server speaks, and
// hands back a Provider whose models are llm.LLMs.
//
//	p, err := coremodels.Open(ctx, prof, coremodels.Options{})
//	m, err := p.Model(ctx, "Qwen/Qwen3-Coder-Next")
//
// Everything that can fail before a request — an unknown dialect, a
// missing variable, absent credentials, a model the profile does not
// serve — fails here, naming the profile (requirement R1).
package coremodels

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-steer/core-models/dialect/anthropic"
	"github.com/go-steer/core-models/dialect/openaichat"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
	"github.com/go-steer/core-models/retry"
)

// Provider is an opened profile.
type Provider interface {
	// Name is the profile's name.
	Name() string
	// Backend is the identity prices are keyed on, beside the model id.
	Backend() string
	// Model returns a model the profile serves, and refuses one it
	// does not.
	Model(ctx context.Context, id string) (llm.LLM, error)
	// DefaultSmallModel is the profile's small-tier model, or "".
	DefaultSmallModel() string
	// Capabilities are what the profile declares its server honors.
	Capabilities() profile.Capabilities
	// Profile is the resolved profile, extends applied.
	Profile() profile.Profile
}

// Options adjusts Open.
type Options struct {
	// Resolve is passed to profile.Resolve.
	Resolve profile.Options
	// HTTPClient supplies the base transport and timeout. Nil means a
	// client with http.DefaultTransport.
	HTTPClient *http.Client
	// Retry overrides the HTTP-layer retry policy.
	Retry *retry.Policy
	// BuiltinTools turns on server-side tools by their provider-neutral
	// names (web_search, url_context, code_execution), for a dialect
	// that has them. Empty is none. Opening a profile whose dialect has
	// none with any set is an error, not a silently missing tool.
	BuiltinTools []string
	// Logf receives an adapter's operator-facing notices. Nil discards
	// them.
	Logf func(format string, args ...any)

	// PromptCache is the Anthropic prompt-caching policy. Nil means
	// anthropic.DefaultCacheOptions: the system block and the
	// conversation tail, at the five-minute TTL. Pass a zero value to
	// place no breakpoints. Ignored by other dialects.
	PromptCache *anthropic.CacheOptions
}

// Open resolves p and returns a Provider for it.
func Open(ctx context.Context, p profile.Profile, opts Options) (Provider, error) {
	r, err := profile.Resolve(ctx, p, opts.Resolve)
	if err != nil {
		return nil, err
	}
	rp := r.Profile
	switch rp.Dialect {
	case profile.OpenAIChat:
		if len(opts.BuiltinTools) > 0 {
			return nil, fmt.Errorf("profile %q: dialect %s has no server-side tools, but %v were asked for", rp.Name, rp.Dialect, opts.BuiltinTools)
		}
		c, err := openaichat.New(openaichat.Options{
			BaseURL:                r.BaseURL,
			Credential:             r.Credential,
			HTTPClient:             opts.HTTPClient,
			Retry:                  opts.Retry,
			Backend:                rp.BackendName(),
			Region:                 r.Params["region"],
			ResponseSchema:         profile.Has(rp.Capabilities.ResponseSchema),
			ReasoningEcho:          profile.Has(rp.Capabilities.ReasoningEcho),
			CachedTokensUnreliable: rp.Usage.CachedTokens == profile.Unreliable,
			ThinkTags:              rp.ReasoningFormat == profile.ThinkTags,
		})
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", rp.Name, err)
		}
		return &provider{p: rp, model: func(id string) llm.LLM {
			caps := rp.CapabilitiesFor(id)
			return c.ModelWith(id, openaichat.ModelOptions{
				ResponseSchema:     new(profile.Has(caps.ResponseSchema)),
				ReasoningEcho:      new(profile.Has(caps.ReasoningEcho)),
				NoForcedToolChoice: new(caps.ForcedToolChoice != nil && !*caps.ForcedToolChoice),
				ExtraBody:          rp.ExtraBodyFor(id),
			})
		}}, nil
	case profile.Gemini:
		return openGemini(ctx, r, opts)
	case profile.Anthropic:
		return openAnthropic(r, opts)
	case profile.OpenAIResponses:
		return nil, fmt.Errorf("profile %q: dialect %s is not built yet (docs/design.md §11: openai-responses is L2)", rp.Name, rp.Dialect)
	}
	return nil, fmt.Errorf("profile %q: unknown dialect %q", rp.Name, rp.Dialect)
}

type provider struct {
	p     profile.Profile
	model func(id string) llm.LLM
}

func (p *provider) Name() string                       { return p.p.Name }
func (p *provider) Backend() string                    { return p.p.BackendName() }
func (p *provider) DefaultSmallModel() string          { return p.p.DefaultSmallModel() }
func (p *provider) Capabilities() profile.Capabilities { return p.p.Capabilities }
func (p *provider) Profile() profile.Profile           { return p.p }

func (p *provider) Model(_ context.Context, id string) (llm.LLM, error) {
	if !p.p.Serves(id) {
		return nil, fmt.Errorf("profile %q does not serve model %q", p.p.Name, id)
	}
	return p.model(id), nil
}

// openAnthropic opens an anthropic-dialect profile: Claude on the
// first-party API, or on Vertex AI when the profile says platform:
// vertex.
func openAnthropic(r *profile.Resolved, opts Options) (Provider, error) {
	rp := r.Profile
	builtins, err := anthropic.BuiltinToolsFromNames(opts.BuiltinTools)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", rp.Name, err)
	}
	cache := anthropic.DefaultCacheOptions()
	if opts.PromptCache != nil {
		cache = *opts.PromptCache
	}
	c, err := anthropic.New(anthropic.Options{
		BaseURL:      r.BaseURL,
		Credential:   r.Credential,
		Vertex:       rp.Platform == profile.PlatformVertex,
		HTTPClient:   opts.HTTPClient,
		Retry:        opts.Retry,
		Backend:      rp.BackendName(),
		Region:       r.Params["region"],
		Cache:        cache,
		BuiltinTools: builtins,
	})
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", rp.Name, err)
	}
	return &provider{p: rp, model: c.Model}, nil
}
