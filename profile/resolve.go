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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/go-steer/core-models/auth"
)

func yes() *bool { return new(true) }
func no() *bool  { return new(false) }

// builtins are the profiles core-models ships. Each is data plus, for
// vertex-maas, a derive rule. Templates (vllm, sglang,
// openai-compatible) have no base_url: an operator extends them and
// supplies one, and using one directly fails validation saying so.
//
// Capabilities are declared conservatively: a capability a server only
// sometimes honors is declared absent, because a product refuses a
// workload that needs an absent capability at startup, and the
// alternative is a server that accepts the field and ignores it at run
// time. An operator who has enabled it (vLLM with guided decoding, say)
// declares it in their own profile.
var builtins = map[string]Profile{
	"openai-compatible": {
		Name:       "openai-compatible",
		Dialect:    OpenAIChat,
		Auth:       auth.Config{Kind: auth.None},
		OpenModels: yes(),
		Capabilities: Capabilities{
			ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: no(),
			ServerTools: no(), Streaming: yes(),
		},
	},
	"vllm": {
		Name:       "vllm",
		Dialect:    OpenAIChat,
		Auth:       auth.Config{Kind: auth.None},
		OpenModels: yes(),
		Capabilities: Capabilities{
			ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: yes(),
			ServerTools: no(), Streaming: yes(),
		},
	},
	"sglang": {
		Name:       "sglang",
		Dialect:    OpenAIChat,
		Auth:       auth.Config{Kind: auth.None},
		OpenModels: yes(),
		Capabilities: Capabilities{
			ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: yes(),
			ServerTools: no(), Streaming: yes(),
		},
	},
	// Ollama's OpenAI-compatible endpoint puts a reasoning model's
	// thinking inline as <think>…</think> (observed on 0.9.6 with
	// qwen3); the splitter only acts on a leading block, so a model that
	// does not think is unaffected.
	"ollama": {
		Name:            "ollama",
		Dialect:         OpenAIChat,
		BaseURL:         "http://localhost:11434/v1",
		ReasoningFormat: ThinkTags,
		Auth:            auth.Config{Kind: auth.None},
		OpenModels:      yes(),
		Capabilities: Capabilities{
			ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: no(),
			ServerTools: no(), Streaming: yes(),
		},
	},
	// Vertex AI serves its partner models (Grok, DeepSeek, Qwen, gpt-oss,
	// Llama and others) through an OpenAI-compatible Chat Completions
	// endpoint under ADC, with publisher-qualified ids such as
	// "openai/gpt-oss-20b-maas". The global location has its own host.
	"vertex-maas": {
		Name:    "vertex-maas",
		Dialect: OpenAIChat,
		BaseURL: "https://{host}/v1/projects/{project}/locations/{region}/endpoints/openapi",
		Params: map[string]string{
			"project": "${GOOGLE_CLOUD_PROJECT}",
			"region":  "${GOOGLE_CLOUD_LOCATION:-global}",
		},
		Auth:       auth.Config{Kind: auth.GoogleADC},
		Backend:    "vertex-maas",
		OpenModels: yes(),
		// Known per-model quirks, from Google's function-calling notes
		// for open models. Listing a model does not close the profile:
		// open_models still admits any id.
		Models: []Model{
			{ID: "openai/gpt-oss-20b-maas", Capabilities: Capabilities{ForcedToolChoice: no()}},
			{ID: "openai/gpt-oss-120b-maas", Capabilities: Capabilities{ForcedToolChoice: no()}},
		},
		Capabilities: Capabilities{
			ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: no(),
			ServerTools: no(), Streaming: yes(),
		},
		derive: vertexHost,
	},
}

// vertexHost sets {host} from {region}: the global location is served
// from aiplatform.googleapis.com, every other from its regional host.
func vertexHost(params map[string]string) {
	if r := params["region"]; r == "" || r == "global" {
		params["host"] = "aiplatform.googleapis.com"
	} else {
		params["host"] = r + "-aiplatform.googleapis.com"
	}
}

// Builtin returns the built-in profile named name.
func Builtin(name string) (Profile, bool) {
	p, ok := builtins[name]
	if !ok {
		return Profile{}, false
	}
	return p.clone(), true
}

// BuiltinNames lists the built-in profiles, sorted.
func BuiltinNames() []string {
	return slices.Sorted(maps.Keys(builtins))
}

func (p Profile) clone() Profile {
	c := p
	c.Params = maps.Clone(p.Params)
	c.Models = slices.Clone(p.Models)
	c.Tiers = maps.Clone(p.Tiers)
	c.ExtraBody = maps.Clone(p.ExtraBody)
	if p.Auth.Scopes != nil {
		c.Auth.Scopes = slices.Clone(p.Auth.Scopes)
	}
	return c
}

// Expand applies Extends: the result is the named built-in with every
// field p sets laid over it. Params and Tiers merge key by key; Models
// and Auth replace as a whole; each capability overrides on its own.
// A profile without Extends is returned unchanged.
func Expand(p Profile) (Profile, error) {
	if p.Extends == "" {
		return p, nil
	}
	base, ok := Builtin(p.Extends)
	if !ok {
		return Profile{}, fmt.Errorf("profile %q: extends %q, which is not a built-in (have %s)",
			p.Name, p.Extends, strings.Join(BuiltinNames(), ", "))
	}
	out := base
	out.Name = p.Name
	out.Extends = ""
	// A derived profile is its own backend unless the built-in names one
	// (vertex-maas prices as vertex-maas whatever the operator calls it).
	if p.Backend != "" {
		out.Backend = p.Backend
	}
	if p.Dialect != "" {
		out.Dialect = p.Dialect
	}
	if p.BaseURL != "" {
		out.BaseURL = p.BaseURL
	}
	for k, v := range p.Params {
		if out.Params == nil {
			out.Params = map[string]string{}
		}
		out.Params[k] = v
	}
	if p.Auth.Kind != "" {
		out.Auth = p.Auth
	}
	if p.OpenModels != nil {
		out.OpenModels = p.OpenModels
	}
	overlay(&out.Capabilities.ResponseSchema, p.Capabilities.ResponseSchema)
	overlay(&out.Capabilities.ReasoningEcho, p.Capabilities.ReasoningEcho)
	overlay(&out.Capabilities.ParallelToolCalls, p.Capabilities.ParallelToolCalls)
	overlay(&out.Capabilities.ServerTools, p.Capabilities.ServerTools)
	overlay(&out.Capabilities.Streaming, p.Capabilities.Streaming)
	overlay(&out.Capabilities.ForcedToolChoice, p.Capabilities.ForcedToolChoice)
	if p.Usage.CachedTokens != "" {
		out.Usage.CachedTokens = p.Usage.CachedTokens
	}
	if p.ReasoningFormat != "" {
		out.ReasoningFormat = p.ReasoningFormat
	}
	for k, v := range p.ExtraBody {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody[k] = v
	}
	if p.MetricsURL != "" {
		out.MetricsURL = p.MetricsURL
	}
	if len(p.Models) > 0 {
		out.Models = slices.Clone(p.Models)
	}
	for k, v := range p.Tiers {
		if out.Tiers == nil {
			out.Tiers = map[Tier]string{}
		}
		out.Tiers[k] = v
	}
	return out, nil
}

func overlay(dst **bool, src *bool) {
	if src != nil {
		*dst = src
	}
}

// Find returns the profile named name: one of declared if any has that
// name, else a built-in. A name declared twice is an error, since the
// second would silently shadow the first.
func Find(name string, declared []Profile) (Profile, error) {
	var hit *Profile
	for i := range declared {
		if declared[i].Name != name {
			continue
		}
		if hit != nil {
			return Profile{}, fmt.Errorf("profile %q is declared twice", name)
		}
		hit = &declared[i]
	}
	if hit != nil {
		return *hit, nil
	}
	if p, ok := Builtin(name); ok {
		return p, nil
	}
	names := BuiltinNames()
	for _, d := range declared {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	return Profile{}, fmt.Errorf("no profile named %q (have %s)", name, strings.Join(slices.Compact(names), ", "))
}

// Options adjusts Resolve.
type Options struct {
	// Getenv replaces os.Getenv, for ${VAR} expansion and credentials.
	Getenv func(string) string
	// Auth is passed to auth.Config.Resolve.
	Auth auth.Options
}

// Resolved is a profile checked against the environment: every
// placeholder filled, every variable read, the credential found.
type Resolved struct {
	Profile Profile
	// Params are the profile's params with every ${VAR} read and the
	// derived ones (vertex-maas's {host}) added.
	Params     map[string]string
	BaseURL    string
	MetricsURL string
	Credential *auth.Credential
}

// Resolve expands, validates and resolves p. Any failure names the
// profile and what is missing; nothing here sends a request.
func Resolve(ctx context.Context, p Profile, opts Options) (*Resolved, error) {
	p, err := Expand(p)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("profile %q: "+format, append([]any{p.Name}, args...)...)
	}

	params := map[string]string{}
	for k, v := range p.Params {
		ev, err := expandEnv(v, getenv)
		if err != nil {
			return nil, fail("params.%s: %v", k, err)
		}
		params[k] = ev
	}
	if p.derive != nil {
		p.derive(params)
	}
	base, err := expandEnv(p.BaseURL, getenv)
	if err != nil {
		return nil, fail("base_url: %v", err)
	}
	base = placeholderRE.ReplaceAllStringFunc(base, func(m string) string {
		return params[m[1:len(m)-1]]
	})
	if base != "" {
		if err := checkURL(base); err != nil {
			return nil, fail("base_url resolves to %v", err)
		}
	}
	metrics, err := expandEnv(p.MetricsURL, getenv)
	if err != nil {
		return nil, fail("metrics_url: %v", err)
	}

	authOpts := opts.Auth
	if authOpts.Getenv == nil {
		authOpts.Getenv = getenv
	}
	cred, err := p.Auth.Resolve(ctx, authOpts)
	if err != nil {
		return nil, fail("%v", err)
	}
	return &Resolved{Profile: p, Params: params, BaseURL: strings.TrimRight(base, "/"), MetricsURL: metrics, Credential: cred}, nil
}

// DecodeJSON reads a JSON array of profiles, refusing unknown fields:
// a misspelled key ("base_ulr") is an error, not a silently missing
// value.
func DecodeJSON(data []byte) ([]Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var out []Profile
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("profiles: trailing data after the array")
	}
	return out, nil
}
