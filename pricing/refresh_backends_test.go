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

package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRefreshKeepsAnthropicVertexLongContext is the case core-agent's
// price A/B found: claude-sonnet-4-5 on anthropic-vertex at a 300K-token
// prompt read $0.96 after a Refresh — the refreshed first-party bare
// row, which has no long-context tier — instead of Vertex's $1.89.
func TestRefreshKeepsAnthropicVertexLongContext(t *testing.T) {
	const body = `{
  "claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "litellm_provider": "anthropic", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true},
  "vertex_ai/claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "input_cost_per_token_above_200k_tokens": 6e-06, "output_cost_per_token_above_200k_tokens": 2.25e-05,
    "litellm_provider": "vertex_ai-anthropic_models", "mode": "chat", "max_input_tokens": 200000, "supports_function_calling": true}
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	home := t.TempDir()
	if _, err := Refresh(context.Background(), home, RefreshOptions{
		Source: srv.URL, Client: srv.Client(),
	}); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	cat, err := NewCatalog(Options{UserHome: home})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	const prompt, out = 300_000, 4_000
	for _, c := range []struct {
		backend string
		want    float64
	}{
		{"anthropic-vertex", 1.89},
		{"anthropic", 0.96},
	} {
		r, src, ok := cat.LookupForWithSource(c.backend, "claude-sonnet-4-5")
		if !ok {
			t.Fatalf("%s: claude-sonnet-4-5 unpriced", c.backend)
		}
		got := r.At(prompt).CostUSD(prompt, out)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s at %d tokens: $%.4f (source %s), want $%.2f", c.backend, prompt, got, src, c.want)
		}
	}
}
