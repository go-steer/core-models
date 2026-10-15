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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-models/pricing"
)

// agreementFixture holds one LiteLLM entry per key rule the generator
// and Refresh share: a family model on each of its backends (Vertex's
// dated "@" spelling and its long-context tier included), a Gemini model
// listed only under prefixes, off-convention models, the three profile
// backends, and entries the generator turns down.
const agreementFixture = `{
  "sample_spec": {"input_cost_per_token": 999, "output_cost_per_token": 999, "mode": "chat"},
  "claude-quill-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "cache_read_input_token_cost": 3e-07, "cache_creation_input_token_cost": 3.75e-06,
    "cache_creation_input_token_cost_above_1hr": 6e-06,
    "litellm_provider": "anthropic", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "vertex_ai/claude-quill-5@20300101": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "cache_read_input_token_cost": 3e-07, "cache_creation_input_token_cost": 3.75e-06,
    "input_cost_per_token_above_200k_tokens": 6e-06, "output_cost_per_token_above_200k_tokens": 2.25e-05,
    "cache_read_input_token_cost_above_200k_tokens": 6e-07,
    "litellm_provider": "vertex_ai-anthropic_models", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "claude-quill-5-20300101": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "litellm_provider": "anthropic", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "vertex_ai/claude-vertexonly-6": {"input_cost_per_token": 5e-06, "output_cost_per_token": 2.5e-05,
    "litellm_provider": "vertex_ai-anthropic_models", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "gemini-9.1-pro": {"input_cost_per_token": 1.25e-06, "output_cost_per_token": 1e-05,
    "input_cost_per_token_above_200k_tokens": 2.5e-06, "output_cost_per_token_above_200k_tokens": 1.5e-05,
    "litellm_provider": "vertex_ai-language-models", "mode": "chat", "max_input_tokens": 1048576, "supports_function_calling": true},
  "gemini/gemini-9.1-pro": {"input_cost_per_token": 1e-06, "output_cost_per_token": 8e-06,
    "litellm_provider": "gemini", "mode": "chat", "max_input_tokens": 1048576, "supports_function_calling": true},
  "gemini/gemini-9-argon": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05,
    "litellm_provider": "gemini", "mode": "chat", "max_input_tokens": 1048576, "supports_function_calling": true},
  "vertex_ai/gemini-9-argon": {"input_cost_per_token": 2.5e-06, "output_cost_per_token": 1.2e-05,
    "cache_read_input_token_cost": 2.5e-07,
    "litellm_provider": "vertex_ai-language-models", "mode": "chat", "max_input_tokens": 1048576, "supports_function_calling": true},
  "gemini/gemini-9-devonly": {"input_cost_per_token": 1e-06, "output_cost_per_token": 4e-06,
    "litellm_provider": "gemini", "mode": "chat", "max_input_tokens": 1048576, "supports_function_calling": true},
  "vertex_ai/zai-org/glm-9-maas": {"input_cost_per_token": 6e-07, "output_cost_per_token": 2.2e-06,
    "litellm_provider": "vertex_ai-zai_models", "mode": "chat", "max_input_tokens": 200000},
  "gpt-9-mini": {"input_cost_per_token": 2.5e-07, "output_cost_per_token": 2e-06,
    "cache_read_input_token_cost": 2.5e-08,
    "litellm_provider": "openai", "mode": "responses", "max_input_tokens": 400000, "supports_function_calling": true},
  "xai/grok-9": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "litellm_provider": "xai", "mode": "chat", "max_input_tokens": 256000, "supports_function_calling": true},
  "gemini-9-tts": {"input_cost_per_token": 1e-06, "output_cost_per_token": 2e-05,
    "litellm_provider": "vertex_ai-language-models", "mode": "audio_speech", "max_input_tokens": 8192},
  "claude-legacy-1": {"input_cost_per_token": 8e-06, "output_cost_per_token": 2.4e-05,
    "litellm_provider": "anthropic", "mode": "chat", "max_input_tokens": 100000, "supports_function_calling": false}
}`

// TestRefreshAgreesWithTheGeneratedTable is the invariant that keeps the
// built-in table and a refreshed one from disagreeing: given the same
// LiteLLM catalog, every row the generator would emit gets the same rate
// from pricing.Refresh's layer, under the same (backend, model) lookup.
// The refreshed layer outranks builtin, so a row Refresh keyed
// differently — LiteLLM's "vertex_ai/claude-…" rather than
// "anthropic-vertex/claude-…" — would fall through to Refresh's bare
// row, which is another backend's rate.
//
// CORE_MODELS_LITELLM_SNAPSHOT names a saved copy of LiteLLM's real
// catalog to run the same check over every row it yields.
func TestRefreshAgreesWithTheGeneratedTable(t *testing.T) {
	body := []byte(agreementFixture)
	if path := os.Getenv("CORE_MODELS_LITELLM_SNAPSHOT"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		body = b
	}
	all, err := parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	kept, qualified, _ := generate(all, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	if len(kept) == 0 || len(qualified) == 0 {
		t.Fatalf("generate kept %d bare and %d qualified rows; the fixture should yield both", len(kept), len(qualified))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	home := t.TempDir()
	if _, err := pricing.Refresh(context.Background(), home, pricing.RefreshOptions{
		Source: srv.URL, Client: srv.Client(),
	}); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	cat, err := pricing.NewCatalog(pricing.Options{UserHome: home})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	check := func(backend, model string, want generatedEntry) {
		t.Helper()
		got, src, ok := cat.LookupForWithSource(backend, model)
		if !ok || src != pricing.SourceUserExternal {
			t.Errorf("LookupFor(%q, %q): found=%v source=%q, want the refreshed layer", backend, model, ok, src)
			return
		}
		if msg := sameRates(want, got); msg != "" {
			t.Errorf("LookupFor(%q, %q): %s", backend, model, msg)
		}
	}
	for _, k := range kept {
		check("", k.Name, k)
	}
	for _, q := range qualified {
		backend, model, _ := strings.Cut(q.Name, "/")
		check(backend, model, q)
	}

	// The fixture's own expectations, so a generator change that drops
	// a rule cannot pass this test by emitting nothing for it.
	if os.Getenv("CORE_MODELS_LITELLM_SNAPSHOT") == "" {
		var names []string
		for _, r := range append(kept, qualified...) {
			names = append(names, r.Name)
		}
		for _, want := range []string{
			"claude-quill-5", "anthropic/claude-quill-5",
			"anthropic-vertex/claude-quill-5-20300101", "anthropic-vertex/claude-vertexonly-6",
			"gemini-9.1-pro", "vertex/gemini-9.1-pro", "gemini/gemini-9.1-pro",
			"gemini-9-argon", "vertex/gemini-9-argon", "gemini/gemini-9-argon",
			"gemini/gemini-9-devonly",
			"vertex-maas/zai-org/glm-9-maas", "openai/gpt-9-mini", "xai/grok-9",
		} {
			if !strings.Contains(" "+strings.Join(names, " ")+" ", " "+want+" ") {
				t.Errorf("generator emitted no %q row; rows: %v", want, names)
			}
		}
	}
}

// sameRates reports how got differs from the generator's row, or "".
func sameRates(want generatedEntry, got pricing.Rates) string {
	var diffs []string
	field := func(name string, w, g float64) {
		if w != g {
			diffs = append(diffs, name+": generated "+fmtRate(w)+", refreshed "+fmtRate(g))
		}
	}
	field("input", want.InputPerMTok, got.InputPerMTok)
	field("cached input", want.CachedInputPerMTok, got.CachedInputPerMTok)
	field("cache write", want.CacheCreationInputPerMTok, got.CacheCreationInputPerMTok)
	field("cache write 1h", want.CacheCreation1hInputPerMTok, got.CacheCreation1hInputPerMTok)
	field("output", want.OutputPerMTok, got.OutputPerMTok)
	switch {
	case want.LongContext == nil && got.LongContext != nil:
		diffs = append(diffs, "refreshed row has a long-context tier the generated one lacks")
	case want.LongContext != nil && got.LongContext == nil:
		diffs = append(diffs, "refreshed row lost the long-context tier")
	case want.LongContext != nil:
		w, g := want.LongContext, got.LongContext
		if w.ThresholdTokens != g.ThresholdTokens {
			diffs = append(diffs, "long-context threshold differs")
		}
		field("long input", round6(w.Input*1e6), g.InputPerMTok)
		field("long cached input", round6(w.CacheRead*1e6), g.CachedInputPerMTok)
		field("long cache write", round6(w.CacheCreation*1e6), g.CacheCreationInputPerMTok)
		field("long cache write 1h", round6(w.CacheCreation1hr*1e6), g.CacheCreation1hInputPerMTok)
		field("long output", round6(w.Output*1e6), g.OutputPerMTok)
	}
	return strings.Join(diffs, "; ")
}

func fmtRate(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
