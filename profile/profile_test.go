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
	"strings"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// noADC stands in for Google credentials so tests never look for real
// ones.
var noADC = auth.Options{DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) {
	return fakeTokens{}, nil
}}

type fakeTokens struct{}

func (fakeTokens) Token(context.Context) (string, error) { return "tok", nil }

func resolve(t *testing.T, p profile.Profile, kv map[string]string) (*profile.Resolved, error) {
	t.Helper()
	return profile.Resolve(context.Background(), p, profile.Options{Getenv: env(kv), Auth: noADC})
}

// Templates exist to be extended, so using one directly must fail and
// say what is missing; everything else must be usable as shipped.
func TestBuiltins(t *testing.T) {
	templates := map[string]bool{"vllm": true, "sglang": true, "openai-compatible": true}
	for _, name := range profile.BuiltinNames() {
		p, _ := profile.Builtin(name)
		err := p.Validate()
		if templates[name] {
			if err == nil || !strings.Contains(err.Error(), "base_url is required") {
				t.Errorf("%s: Validate = %v, want the missing base_url named", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

func TestOllamaResolvesWithNothingSet(t *testing.T) {
	p, _ := profile.Builtin("ollama")
	r, err := resolve(t, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseURL != "http://localhost:11434/v1" || r.Credential.Kind() != auth.None || r.Profile.ReasoningFormat != profile.ThinkTags {
		t.Errorf("resolved = %+v", r)
	}
}

func TestVertexMaaSEndpoint(t *testing.T) {
	p, _ := profile.Builtin("vertex-maas")
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"global by default": {
			map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"},
			"https://aiplatform.googleapis.com/v1/projects/acme/locations/global/endpoints/openapi",
		},
		"regional": {
			map[string]string{"GOOGLE_CLOUD_PROJECT": "acme", "GOOGLE_CLOUD_LOCATION": "us-central1"},
			"https://us-central1-aiplatform.googleapis.com/v1/projects/acme/locations/us-central1/endpoints/openapi",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := resolve(t, p, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if r.BaseURL != tc.want {
				t.Errorf("BaseURL = %s\nwant      %s", r.BaseURL, tc.want)
			}
			if strings.Contains(r.Params["region"], "$") || r.Params["host"] == "" {
				t.Errorf("Params = %v, want expanded and derived values", r.Params)
			}
			if r.Profile.BackendName() != "vertex-maas" || r.Credential.Kind() != auth.GoogleADC {
				t.Errorf("backend %q, auth %s", r.Profile.BackendName(), r.Credential)
			}
		})
	}
	_, err := resolve(t, p, nil)
	if err == nil || !strings.Contains(err.Error(), `profile "vertex-maas"`) || !strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT is not set") {
		t.Errorf("Resolve with no project = %v, want the profile and the variable named", err)
	}
}

func TestExtendingATemplate(t *testing.T) {
	house := profile.Profile{
		Name:         "house-vllm",
		Extends:      "vllm",
		BaseURL:      "http://vllm.infra.svc:8000/v1/",
		Capabilities: profile.Capabilities{ResponseSchema: new(true)},
		Usage:        profile.UsageOverrides{CachedTokens: profile.Unreliable},
		Models:       []profile.Model{{ID: "Qwen/Qwen3-Coder-Next", Tier: profile.Mid, ContextWindow: 262144}},
		Tiers:        map[profile.Tier]string{profile.Mid: "Qwen/Qwen3-Coder-Next"},
	}
	r, err := resolve(t, house, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Profile
	if r.BaseURL != "http://vllm.infra.svc:8000/v1" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", r.BaseURL)
	}
	if got.Dialect != profile.OpenAIChat || !got.Open() || got.BackendName() != "house-vllm" {
		t.Errorf("inherited dialect %q, open %v, backend %q", got.Dialect, got.Open(), got.BackendName())
	}
	if !profile.Has(got.Capabilities.ResponseSchema) {
		t.Error("the operator's response_schema did not override the template's")
	}
	if !profile.Has(got.Capabilities.ParallelToolCalls) || profile.Has(got.Capabilities.ServerTools) {
		t.Error("capabilities the operator did not set were not inherited")
	}
	if got.Usage.CachedTokens != profile.Unreliable || !got.Serves("anything/else") {
		t.Errorf("usage %q, serves open ids %v", got.Usage.CachedTokens, got.Serves("anything/else"))
	}

	// The built-in must be untouched by the derivation.
	again, _ := profile.Builtin("vllm")
	if profile.Has(again.Capabilities.ResponseSchema) || again.BaseURL != "" {
		t.Error("Expand mutated the built-in")
	}
}

func TestAProfileExtendingVertexMaaSStillPricesAsVertexMaaS(t *testing.T) {
	p := profile.Profile{Name: "maas-eu", Extends: "vertex-maas", Params: map[string]string{"region": "europe-west4"}}
	r, err := resolve(t, p, map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile.BackendName() != "vertex-maas" {
		t.Errorf("backend = %q, want vertex-maas: the price belongs to the backend, not the operator's name", r.Profile.BackendName())
	}
	if !strings.HasPrefix(r.BaseURL, "https://europe-west4-aiplatform.googleapis.com/") {
		t.Errorf("BaseURL = %s", r.BaseURL)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	p := profile.Profile{
		Name:            "Bad_Name",
		Dialect:         "grpc",
		BaseURL:         "ftp://x/{zone}",
		Auth:            auth.Config{Kind: auth.APIKey},
		MetricsURL:      "not a url",
		Usage:           profile.UsageOverrides{CachedTokens: "sometimes"},
		ReasoningFormat: "xml",
		Models:          []profile.Model{{ID: "m", Tier: "huge"}, {ID: "m"}, {ContextWindow: -1}},
		Tiers:           map[profile.Tier]string{profile.Small: "unlisted", "tiny": "m"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate passed a profile with every field wrong")
	}
	for _, want := range []string{
		"name must be lowercase",
		`dialect "grpc"`,
		"not an http or https URL",
		"uses {zone}",
		"needs env",
		"metrics_url",
		"usage.cached_tokens",
		`reasoning_format "xml"`,
		`tier "huge"`,
		`model "m" is listed twice`,
		"a model has no id",
		"context_window is negative",
		`tiers.small names "unlisted"`,
		`"tiny" is not small, mid or frontier`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if n := strings.Count(err.Error(), `profile "Bad_Name"`); n < 14 {
		t.Errorf("%d errors name the profile, want every one", n)
	}
}

func TestAClosedProfileMustListItsModels(t *testing.T) {
	p := profile.Profile{Name: "x", Dialect: profile.OpenAIChat, BaseURL: "https://x", Auth: auth.Config{Kind: auth.None}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "lists no models") {
		t.Errorf("Validate = %v", err)
	}
	p.Models = []profile.Model{{ID: "a"}, {ID: "b"}}
	p.Tiers = map[profile.Tier]string{profile.Small: "b"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if !p.Serves("a") || p.Serves("c") || p.DefaultSmallModel() != "b" {
		t.Errorf("Serves(a)=%v Serves(c)=%v small=%q", p.Serves("a"), p.Serves("c"), p.DefaultSmallModel())
	}
}

func TestResolveNamesTheProfileWhenTheCredentialIsMissing(t *testing.T) {
	p := profile.Profile{
		Name: "groq", Extends: "openai-compatible", BaseURL: "https://api.groq.com/openai/v1",
		Auth: auth.Config{Kind: auth.APIKey, Env: "GROQ_API_KEY"},
	}
	_, err := resolve(t, p, nil)
	if err == nil || !strings.Contains(err.Error(), `profile "groq"`) || !strings.Contains(err.Error(), "GROQ_API_KEY is not set") {
		t.Errorf("Resolve = %v", err)
	}
	r, err := resolve(t, p, map[string]string{"GROQ_API_KEY": "gsk"})
	if err != nil || r.Credential.Secret() != "gsk" {
		t.Errorf("Resolve = %+v, %v", r, err)
	}
}

func TestEnvExpansionInBaseURL(t *testing.T) {
	p := profile.Profile{Name: "lab", Extends: "vllm", BaseURL: "http://${VLLM_HOST:-localhost}:${VLLM_PORT}/v1"}
	if _, err := resolve(t, p, nil); err == nil || !strings.Contains(err.Error(), "VLLM_PORT is not set") {
		t.Errorf("Resolve = %v, want VLLM_PORT named", err)
	}
	r, err := resolve(t, p, map[string]string{"VLLM_PORT": "8000"})
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("BaseURL = %s", r.BaseURL)
	}
}

func TestExpandAnUnknownBuiltin(t *testing.T) {
	_, err := profile.Expand(profile.Profile{Name: "x", Extends: "vlm"})
	if err == nil || !strings.Contains(err.Error(), `extends "vlm"`) || !strings.Contains(err.Error(), "vllm") {
		t.Errorf("Expand = %v, want the typo named and the built-ins listed", err)
	}
	if err := (profile.Profile{Name: "x", Extends: "vllm"}).Validate(); err == nil || !strings.Contains(err.Error(), "Expand") {
		t.Errorf("Validate on an unexpanded profile = %v", err)
	}
}

func TestFind(t *testing.T) {
	mine := profile.Profile{Name: "ollama", Extends: "ollama", BaseURL: "http://gpu-box:11434/v1"}
	got, err := profile.Find("ollama", []profile.Profile{mine})
	if err != nil || got.BaseURL != "http://gpu-box:11434/v1" {
		t.Errorf("a declared profile did not shadow the built-in: %+v, %v", got, err)
	}
	if got, err := profile.Find("vertex-maas", nil); err != nil || got.Name != "vertex-maas" {
		t.Errorf("built-in lookup: %+v, %v", got, err)
	}
	if _, err := profile.Find("ollama", []profile.Profile{mine, mine}); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := profile.Find("olama", []profile.Profile{{Name: "house-vllm"}}); err == nil ||
		!strings.Contains(err.Error(), "house-vllm") || !strings.Contains(err.Error(), "ollama") {
		t.Errorf("unknown = %v, want declared and built-in names listed", err)
	}
}

func TestDecodeJSON(t *testing.T) {
	ps, err := profile.DecodeJSON([]byte(`[{
		"name": "house-vllm",
		"extends": "vllm",
		"base_url": "http://vllm.infra.svc:8000/v1",
		"metrics_url": "http://vllm.infra.svc:8000/metrics",
		"usage": {"cached_tokens": "unreliable"},
		"models": [{"id": "Qwen/Qwen3-Coder-Next", "tier": "mid", "context_window": 262144}],
		"tiers": {"mid": "Qwen/Qwen3-Coder-Next"}
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := resolve(t, ps[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.MetricsURL != "http://vllm.infra.svc:8000/metrics" || r.Profile.Tiers[profile.Mid] != "Qwen/Qwen3-Coder-Next" {
		t.Errorf("resolved = %+v", r)
	}

	if _, err := profile.DecodeJSON([]byte(`[{"name": "x", "base_ulr": "http://x"}]`)); err == nil || !strings.Contains(err.Error(), "base_ulr") {
		t.Errorf("a misspelled key decoded silently: %v", err)
	}
	if _, err := profile.DecodeJSON([]byte(`[] []`)); err == nil {
		t.Error("trailing data decoded silently")
	}
}

func TestCapabilitiesArePerModel(t *testing.T) {
	maas, _ := profile.Builtin("vertex-maas")
	if c := maas.CapabilitiesFor("openai/gpt-oss-20b-maas"); c.ForcedToolChoice == nil || *c.ForcedToolChoice {
		t.Errorf("gpt-oss on vertex-maas: ForcedToolChoice = %v, want declared false (Google's function-calling notes)", c.ForcedToolChoice)
	}
	if c := maas.CapabilitiesFor("zai-org/glm-5.2-maas"); c.ForcedToolChoice != nil {
		t.Errorf("an unlisted model inherited a per-model override: %v", *c.ForcedToolChoice)
	}
	if !maas.Serves("zai-org/glm-5.2-maas") {
		t.Error("listing quirky models closed an open profile")
	}

	p := profile.Profile{
		Name: "x", Extends: "vllm", BaseURL: "http://x/v1",
		Capabilities: profile.Capabilities{ResponseSchema: new(true)},
		Models:       []profile.Model{{ID: "kimi", Capabilities: profile.Capabilities{ReasoningEcho: new(true), ResponseSchema: new(false)}}},
	}
	e, err := profile.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	k := e.CapabilitiesFor("kimi")
	if !profile.Has(k.ReasoningEcho) || profile.Has(k.ResponseSchema) || !profile.Has(k.ParallelToolCalls) {
		t.Errorf("kimi = %+v: want its own echo and schema over the profile's, and the template's parallel calls", k)
	}
	if o := e.CapabilitiesFor("other"); !profile.Has(o.ResponseSchema) || profile.Has(o.ReasoningEcho) {
		t.Errorf("other = %+v: want the profile's", o)
	}
}

func TestExtraBody(t *testing.T) {
	p := profile.Profile{
		Name: "lab", Extends: "vllm", BaseURL: "http://x/v1",
		ExtraBody: map[string]any{"top_k": 20, "chat_template_kwargs": map[string]any{"enable_thinking": false}},
		Models:    []profile.Model{{ID: "gemma", ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}}},
	}
	e, err := profile.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	g := e.ExtraBodyFor("gemma")
	if g["top_k"] != 20 || g["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Errorf("gemma extra = %v: want the profile's top_k and its own thinking switch", g)
	}
	if o := e.ExtraBodyFor("other"); o["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Errorf("other extra = %v: want the profile's", o)
	}
	if (profile.Profile{Name: "x"}).ExtraBodyFor("m") != nil {
		t.Error("no extras should be nil")
	}

	bad := profile.Profile{Name: "lab", Extends: "vllm", BaseURL: "http://x/v1",
		ExtraBody: map[string]any{"temperature": 0.1},
		Models:    []profile.Model{{ID: "m", ExtraBody: map[string]any{"messages": []any{}}}}}
	e, _ = profile.Expand(bad)
	err = e.Validate()
	if err == nil || !strings.Contains(err.Error(), `profile extra_body sets "temperature"`) || !strings.Contains(err.Error(), `model "m" extra_body sets "messages"`) {
		t.Errorf("Validate = %v, want both reserved keys named", err)
	}
}
