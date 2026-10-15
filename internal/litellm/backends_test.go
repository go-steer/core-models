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

package litellm

import (
	"maps"
	"testing"
)

func TestQualify(t *testing.T) {
	t.Parallel()
	priced := map[string]string{
		"claude-quill-5":                    "anthropic",
		"vertex_ai/claude-quill-5@20300101": "vertex_ai-anthropic_models",
		"claude-quill-5-20300101":           "anthropic",
		"vertex_ai/claude-vertexonly-6":     "vertex_ai-anthropic_models",
		"gemini-9.1-pro":                    "vertex_ai-language-models",
		"gemini/gemini-9.1-pro":             "gemini",
		"gemini/gemini-9-argon":             "gemini",
		"vertex_ai/gemini-9-argon":          "vertex_ai-language-models",
		"gemini/gemini-9-devonly":           "gemini",
		"vertex_ai/zai-org/glm-9-maas":      "vertex_ai-zai_models",
		"gpt-9-mini":                        "openai",
		"xai/grok-9":                        "xai",
		"openrouter/google/gemini-9.1-pro":  "openrouter",
		"mistral-large-9":                   "mistral",
	}
	want := map[string]string{
		"anthropic/claude-quill-5":                 "claude-quill-5",
		"anthropic/claude-quill-5-20300101":        "claude-quill-5-20300101",
		"anthropic-vertex/claude-quill-5-20300101": "vertex_ai/claude-quill-5@20300101",
		// Only on Vertex: a qualified row and no bare one, because a
		// bare claude-* row is Anthropic first-party's rate.
		"anthropic-vertex/claude-vertexonly-6": "vertex_ai/claude-vertexonly-6",
		"vertex/gemini-9.1-pro":                "gemini-9.1-pro",
		"gemini/gemini-9.1-pro":                "gemini/gemini-9.1-pro",
		// Prefix-only: the bare row carries Vertex's rate, by convention.
		"gemini-9-argon":        "vertex_ai/gemini-9-argon",
		"vertex/gemini-9-argon": "vertex_ai/gemini-9-argon",
		"gemini/gemini-9-argon": "gemini/gemini-9-argon",
		// Developer API only: no bare row and no vertex/ row.
		"gemini/gemini-9-devonly":        "gemini/gemini-9-devonly",
		"vertex-maas/zai-org/glm-9-maas": "vertex_ai/zai-org/glm-9-maas",
		"openai/gpt-9-mini":              "gpt-9-mini",
		"xai/grok-9":                     "xai/grok-9",
	}
	if got := Qualify(priced); !maps.Equal(got, want) {
		t.Errorf("Qualify:\n got %v\nwant %v", got, want)
	}
}

func TestBareID(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"gemini-9-argon":           "gemini-9-argon",
		"claude-quill-7@20300101":  "claude-quill-7-20300101",
		"claude-quill-7@default":   "claude-quill-7",
		"gemini-1.0-pro@001":       "",
		"claude-quill-7@2030-0101": "",
	} {
		got, ok := BareID(in)
		if got != want || ok != (want != "") {
			t.Errorf("BareID(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
