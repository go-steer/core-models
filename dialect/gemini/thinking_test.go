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

package gemini

import (
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
)

func level(l genai.ThinkingLevel) *genai.ThinkingConfig {
	return &genai.ThinkingConfig{ThinkingLevel: l}
}
func budget(n int32) *genai.ThinkingConfig {
	return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(n)}
}

// Gemini 4 takes a thinking level and nothing else (measured on Neon,
// Hydrogen and Carbon; see fitThinking). Rows marked otherwise follow the
// documented or assumed cases in fitThinking.
func TestFitThinkingGemini4(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		model string
		in    *genai.ThinkingConfig
		want  genai.ThinkingLevel // "" means no level
	}{
		{"budget 0 cannot turn thinking off", "gemini-4-neon", budget(0), genai.ThinkingLevelLow},
		{"small budget", "gemini-4-neon", budget(2048), genai.ThinkingLevelLow},
		{"medium budget", "gemini-4-hydrogen", budget(8192), genai.ThinkingLevelMedium},
		{"large budget", "gemini-4-carbon", budget(32768), genai.ThinkingLevelHigh},
		{"dynamic budget is the model default", "gemini-4-neon", budget(-1), ""},
		{"MINIMAL", "gemini-4-neon", level(genai.ThinkingLevelMinimal), genai.ThinkingLevelLow},
		{"HIGH passes", "gemini-4-carbon", level(genai.ThinkingLevelHigh), genai.ThinkingLevelHigh},
		{"XHIGH on neon", "gemini-4-neon", level(genai.ThinkingLevelXhigh), genai.ThinkingLevelXhigh},
		{"XHIGH on hydrogen", "gemini-4-hydrogen", level(genai.ThinkingLevelXhigh), genai.ThinkingLevelXhigh},
		{"XHIGH on carbon", "gemini-4-carbon", level(genai.ThinkingLevelXhigh), genai.ThinkingLevelHigh},
		{"XHIGH on carbon variant (assumed as carbon)", "gemini-4-carbon-cyber-permissive", level(genai.ThinkingLevelXhigh), genai.ThinkingLevelHigh},
		{"XHIGH on argon (documented as carbon)", "gemini-4-argon", level(genai.ThinkingLevelXhigh), genai.ThinkingLevelHigh},
		{"lowercase level", "gemini-4-neon", level("xhigh"), genai.ThinkingLevelXhigh},
		{"uppercase model id", "GEMINI-4-NEON", level(llm.ThinkingLevelMax), genai.ThinkingLevelXhigh},
		{"later minor version (assumed)", "gemini-4.1-flash", budget(2048), genai.ThinkingLevelLow},
		{"later major version (assumed)", "gemini-5-pro", level(llm.ThinkingLevelMax), genai.ThinkingLevelHigh},
		{"MAX on neon", "gemini-4-neon", level(llm.ThinkingLevelMax), genai.ThinkingLevelXhigh},
		{"MAX on carbon", "gemini-4-carbon", level(llm.ThinkingLevelMax), genai.ThinkingLevelHigh},
		{"resource path", "publishers/google/models/gemini-4-neon", level(llm.ThinkingLevelMax), genai.ThinkingLevelXhigh},
		{"level outranks budget", "gemini-4-neon", &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelMedium, ThinkingBudget: genai.Ptr[int32](32768)}, genai.ThinkingLevelMedium},
	} {
		got := fitThinking(tc.model, tc.in)
		if got.ThinkingBudget != nil {
			t.Errorf("%s: budget %d sent; Gemini 4 refuses any budget", tc.name, *got.ThinkingBudget)
		}
		if got.ThinkingLevel != tc.want {
			t.Errorf("%s: level %q, want %q", tc.name, got.ThinkingLevel, tc.want)
		}
	}
}

// The caller's config is never changed, and other fields survive.
func TestFitThinkingCopies(t *testing.T) {
	t.Parallel()
	in := &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0), IncludeThoughts: true}
	got := fitThinking("gemini-4-neon", in)
	if in.ThinkingBudget == nil || *in.ThinkingBudget != 0 || in.ThinkingLevel != "" {
		t.Errorf("caller's config changed: %+v", in)
	}
	if got == in || !got.IncludeThoughts {
		t.Errorf("fitted config %+v: want a copy keeping IncludeThoughts", got)
	}
	if fitThinking("gemini-4-neon", nil) != nil {
		t.Error("nil config should stay nil")
	}
}

// Other Gemini models are not measured: they pass through, except
// llm.ThinkingLevelMax, which only core-models defines.
func TestFitThinkingOtherModels(t *testing.T) {
	t.Parallel()
	in := budget(1024)
	if got := fitThinking("gemini-3.8-flash", in); got != in {
		t.Errorf("gemini-3.8-flash: config changed to %+v", got)
	}
	if got := fitThinking("gemini-3.8-flash", level(llm.ThinkingLevelMax)); got.ThinkingLevel != genai.ThinkingLevelHigh {
		t.Errorf("MAX on gemini-3.8-flash: %q, want HIGH", got.ThinkingLevel)
	}
}
