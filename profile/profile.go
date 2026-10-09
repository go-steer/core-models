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

// Package profile declares how to reach a model: endpoint, credential,
// wire dialect, capabilities, and the models served.
//
// A model is named by (profile, model id), not by a model id alone. The
// same id is served by several backends at several prices with several
// credentials — gpt-oss-20b by Vertex AI and by an operator's vLLM — so
// the id cannot pick the transport, the credential or the price, and a
// profile does (docs/design.md §5).
//
// This package owns the schema and the built-in profiles. Where an
// operator's profiles are read from is each product's decision; the
// struct carries json and yaml tags so either format decodes into it,
// and DecodeJSON is the strict JSON reader.
//
// A profile is checked at two moments, and both fail before any
// request is sent (R1): Validate checks its shape, and Resolve checks
// it against the environment — variables set, credentials found.
package profile

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-steer/core-models/auth"
)

// Dialect is the wire format a profile's server speaks.
type Dialect string

const (
	// OpenAIChat is POST /v1/chat/completions: Vertex AI partner models,
	// xAI, vLLM, SGLang, Ollama, llama.cpp, NIM and the managed long
	// tail.
	OpenAIChat Dialect = "openai-chat"
	// OpenAIResponses is POST /v1/responses: OpenAI first-party, xAI.
	OpenAIResponses Dialect = "openai-responses"
	// Anthropic is the Messages API: first-party, Vertex, Bedrock.
	Anthropic Dialect = "anthropic"
	// Gemini is genai's generateContent: the Developer API and Vertex.
	Gemini Dialect = "gemini"
)

func (d Dialect) known() bool {
	switch d {
	case OpenAIChat, OpenAIResponses, Anthropic, Gemini:
		return true
	}
	return false
}

// Tier is the three-rung vocabulary products route on.
type Tier string

const (
	Small    Tier = "small"
	Mid      Tier = "mid"
	Frontier Tier = "frontier"
)

func (t Tier) known() bool { return t == Small || t == Mid || t == Frontier }

// Reliability says whether a usage field a server reports can be
// trusted.
type Reliability string

const (
	// Reported (the default) records the field when the server sends it.
	Reported Reliability = ""
	// Unreliable records the field as not reported even when the server
	// sends it — for a server build known to send nonsense, such as the
	// vLLM V1 builds that null or zero prompt_tokens_details.
	Unreliable Reliability = "unreliable"
)

// Profile is one declared way to reach models.
type Profile struct {
	// Name identifies the profile: what --provider names and what a
	// product's config refers to. Lowercase letters, digits and '-'.
	Name string `json:"name" yaml:"name"`
	// Extends names a built-in profile this one starts from; every field
	// set here overrides the built-in's.
	Extends string `json:"extends,omitempty" yaml:"extends,omitempty"`
	// Dialect is the server's wire format.
	Dialect Dialect `json:"dialect,omitempty" yaml:"dialect,omitempty"`
	// BaseURL is the API root. It may contain {param} placeholders,
	// filled from Params.
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	// Params fill BaseURL's placeholders. A value may be ${VAR} or
	// ${VAR:-default}, read from the environment at Resolve.
	Params map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
	// Auth is the credential.
	Auth auth.Config `json:"auth,omitzero" yaml:"auth,omitempty"`
	// Backend is the identity prices are keyed on, beside the model id.
	// Defaults to Name.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	// OpenModels admits any model id, not only those in Models: a
	// self-hosted server serves whatever weights the operator loaded.
	OpenModels *bool `json:"open_models,omitempty" yaml:"open_models,omitempty"`
	// Capabilities are what the server honors. Products refuse at
	// startup a workload that needs one the profile lacks.
	Capabilities Capabilities `json:"capabilities,omitzero" yaml:"capabilities,omitempty"`
	// Usage overrides how reported usage fields are trusted.
	Usage UsageOverrides `json:"usage,omitzero" yaml:"usage,omitempty"`
	// ReasoningFormat says where the server puts reasoning.
	ReasoningFormat ReasoningFormat `json:"reasoning_format,omitempty" yaml:"reasoning_format,omitempty"`
	// MetricsURL is the server's Prometheus endpoint, for the opt-in
	// KV-cache sampler (docs/design.md §8).
	MetricsURL string `json:"metrics_url,omitempty" yaml:"metrics_url,omitempty"`
	// Models lists the models the profile serves.
	Models []Model `json:"models,omitempty" yaml:"models,omitempty"`
	// Tiers names the model for each tier this profile can fill.
	Tiers map[Tier]string `json:"tiers,omitempty" yaml:"tiers,omitempty"`

	// derive computes extra BaseURL params from the resolved ones. Only
	// built-ins set it; see vertexMaaS.
	derive func(params map[string]string)
}

// Model is one model a profile serves.
type Model struct {
	ID            string `json:"id" yaml:"id"`
	Tier          Tier   `json:"tier,omitempty" yaml:"tier,omitempty"`
	ContextWindow int    `json:"context_window,omitempty" yaml:"context_window,omitempty"`
}

// Capabilities are declared, never probed. Nil means not declared,
// which a product reads as absent.
type Capabilities struct {
	// ResponseSchema: the server enforces a JSON-schema response format.
	// Many self-hosted servers accept the field and ignore it unless a
	// guided-decoding backend is enabled — declare it only when it is.
	ResponseSchema *bool `json:"response_schema,omitempty" yaml:"response_schema,omitempty"`
	// ReasoningEcho: reasoning from one turn is carried into the next.
	ReasoningEcho *bool `json:"reasoning_echo,omitempty" yaml:"reasoning_echo,omitempty"`
	// ParallelToolCalls: the model may call several tools in one turn.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	// ServerTools: the provider runs built-in tools (search, code).
	ServerTools *bool `json:"server_tools,omitempty" yaml:"server_tools,omitempty"`
	// Streaming: the server streams responses.
	Streaming *bool `json:"streaming,omitempty" yaml:"streaming,omitempty"`
}

// ReasoningFormat says where a server puts a model's reasoning.
type ReasoningFormat string

const (
	// ReasoningField (the default): in reasoning_content or reasoning,
	// apart from the answer, or not at all.
	ReasoningField ReasoningFormat = ""
	// ThinkTags: inline at the start of the answer as <think>…</think>
	// (Ollama; vLLM and SGLang without a reasoning parser). The adapter
	// splits the block out, so it never reaches the product as answer
	// text.
	ThinkTags ReasoningFormat = "think_tags"
)

// UsageOverrides adjust how reported usage is trusted.
type UsageOverrides struct {
	CachedTokens Reliability `json:"cached_tokens,omitempty" yaml:"cached_tokens,omitempty"`
}

// Has reports whether a declared capability is true.
func Has(c *bool) bool { return c != nil && *c }

// Open reports whether the profile admits model ids it does not list.
func (p Profile) Open() bool { return Has(p.OpenModels) }

// BackendName is Backend, defaulting to Name.
func (p Profile) BackendName() string {
	if p.Backend != "" {
		return p.Backend
	}
	return p.Name
}

// Serves reports whether the profile can serve model id.
func (p Profile) Serves(id string) bool {
	if p.Open() {
		return id != ""
	}
	for _, m := range p.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// DefaultSmallModel is the profile's small-tier model, or "".
func (p Profile) DefaultSmallModel() string { return p.Tiers[Small] }

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// placeholderRE matches a {param} in BaseURL.
var placeholderRE = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)\}`)

// Validate checks the profile's shape without touching the
// environment. Every problem is reported, joined, each naming the
// profile, so one run shows them all.
func (p Profile) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("profile %q: "+format, append([]any{p.Name}, args...)...))
	}
	if !nameRE.MatchString(p.Name) {
		bad("name must be lowercase letters, digits and '-'")
	}
	if p.Extends != "" {
		bad("extends %q has not been applied; build the profile with Expand", p.Extends)
	}
	if !p.Dialect.known() {
		bad("dialect %q is not one of openai-chat, openai-responses, anthropic, gemini", p.Dialect)
	}
	switch {
	case p.BaseURL == "" && (p.Dialect == OpenAIChat || p.Dialect == OpenAIResponses):
		bad("base_url is required for dialect %s", p.Dialect)
	case p.BaseURL != "":
		// Placeholders and ${VAR}s are filled at Resolve; stand a "0" in
		// for each so the shape check holds wherever they sit, the port
		// included.
		shape := placeholderRE.ReplaceAllString(envRE.ReplaceAllString(p.BaseURL, "0"), "0")
		if err := checkURL(shape); err != nil {
			bad("base_url: %v", err)
		}
		for _, m := range placeholderRE.FindAllStringSubmatch(p.BaseURL, -1) {
			if _, ok := p.Params[m[1]]; !ok && !p.derives(m[1]) {
				bad("base_url uses {%s}, which params does not set", m[1])
			}
		}
	}
	if err := p.Auth.Validate(); err != nil {
		bad("%v", err)
	}
	if p.MetricsURL != "" {
		if err := checkURL(envRE.ReplaceAllString(p.MetricsURL, "0")); err != nil {
			bad("metrics_url: %v", err)
		}
	}
	if p.Usage.CachedTokens != Reported && p.Usage.CachedTokens != Unreliable {
		bad("usage.cached_tokens %q is not \"unreliable\" or unset", p.Usage.CachedTokens)
	}
	if p.ReasoningFormat != ReasoningField && p.ReasoningFormat != ThinkTags {
		bad("reasoning_format %q is not \"think_tags\" or unset", p.ReasoningFormat)
	}
	if !p.Open() && len(p.Models) == 0 {
		bad("lists no models; list them, or set open_models for a server that serves what it was given")
	}
	seen := map[string]bool{}
	for _, m := range p.Models {
		switch {
		case m.ID == "":
			bad("a model has no id")
		case seen[m.ID]:
			bad("model %q is listed twice", m.ID)
		}
		seen[m.ID] = true
		if m.Tier != "" && !m.Tier.known() {
			bad("model %q: tier %q is not small, mid or frontier", m.ID, m.Tier)
		}
		if m.ContextWindow < 0 {
			bad("model %q: context_window is negative", m.ID)
		}
	}
	for tier, id := range p.Tiers {
		if !tier.known() {
			bad("tiers: %q is not small, mid or frontier", tier)
		}
		if !p.Serves(id) {
			bad("tiers.%s names %q, which the profile does not serve", tier, id)
		}
	}
	return errors.Join(errs...)
}

func (p Profile) derives(param string) bool {
	if p.derive == nil {
		return false
	}
	probe := map[string]string{}
	for k, v := range p.Params {
		probe[k] = v
	}
	p.derive(probe)
	_, ok := probe[param]
	return ok
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q is not an http or https URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// expandEnv replaces ${VAR} and ${VAR:-default} in s. A ${VAR} that is
// unset, with no default, is an error naming VAR.
func expandEnv(s string, getenv func(string) string) (string, error) {
	var missing []string
	out := envRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-1]
		name, def, hasDef := strings.Cut(inner, ":-")
		if v := getenv(name); v != "" {
			return v
		}
		if hasDef {
			return def
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}

var envRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}`)
