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

// Originally derived from go-steer/mast@6568618f531368b8fa4894645df292b1e2aca09f:internal/providers/anthropic/thinking_test.go

package anthropic

import (
	"testing"

	"google.golang.org/genai"
)

// These tests exist because the ones they replaced could not have
// caught mast #369. mast's convert_test.go asserted that a thinking
// budget produced `thinking.type=enabled` — a statement about what was
// sent and never about what the model accepts — so it passed while
// every live call to mast's own default model returned 400. core-agent
// still carried that assertion when the adapters were merged. The shapes below are pinned
// against a measured matrix rather than against the SDK's type set:
//
//	model             enabled    adaptive   disabled   effort
//	claude-haiku-4-5  accepted   400        accepted   400 (no such param)
//	claude-opus-4-5   accepted   400        accepted   accepted
//	claude-opus-4-6   accepted*  accepted   accepted   accepted, no "xhigh"
//	claude-opus-4-7   400        accepted   accepted   accepted
//	claude-opus-4-8   400        accepted   accepted   accepted
//	claude-opus-5     400        accepted   accepted   accepted
//	claude-sonnet-5   400        accepted   accepted   accepted
//
//	* with a deprecation warning naming adaptive as the replacement.
//
// Measured 2026-09-15 against Vertex region `global`, anthropic-sdk-go
// v1.43.0, one request per cell.

func thinkingFor(t *testing.T, modelID string, tc *genai.ThinkingConfig) (enabled *int64, adaptive string, disabled bool) {
	t.Helper()
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}
	p, err := buildParams(modelID, contents, &genai.GenerateContentConfig{ThinkingConfig: tc}, CacheOptions{}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams(%s): %v", modelID, err)
	}
	if p.Thinking.OfEnabled != nil {
		b := p.Thinking.OfEnabled.BudgetTokens
		enabled = &b
	}
	if p.Thinking.OfAdaptive != nil {
		adaptive = string(p.Thinking.OfAdaptive.Display)
		if adaptive == "" {
			adaptive = "(default)"
		}
	}
	disabled = p.Thinking.OfDisabled != nil
	return
}

// A positive budget must never produce the enabled shape on a model that
// 400s it. This is the defect in #369: mast's own DefaultModel is
// claude-opus-5, and the only thinking config mast could build was the
// one that model rejects.
func TestThinkingShapeSplitsByModel(t *testing.T) {
	t.Parallel()
	budget := &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048)}

	// Everything before the 4-6 generation that thinks at all, in both
	// naming schemes and with both snapshot spellings.
	for _, m := range []string{
		"claude-haiku-4-5", "claude-opus-4-5", "claude-sonnet-4-5",
		"claude-sonnet-4", "claude-sonnet-4-20250514", "claude-opus-4", "claude-opus-4-1",
		"claude-opus-4-1@20250805", "claude-3-7-sonnet", "claude-3-7-sonnet-20250219",
	} {
		t.Run("legacy/"+m, func(t *testing.T) {
			t.Parallel()
			enabled, adaptive, _ := thinkingFor(t, m, budget)
			if enabled == nil || *enabled != 2048 {
				t.Errorf("%s: enabled = %v, want budget 2048 — this model 400s adaptive", m, enabled)
			}
			if adaptive != "" {
				t.Errorf("%s: sent adaptive (%s), which this model 400s", m, adaptive)
			}
		})
	}

	// 4-6 accepts both and the vendor's own deprecation warning says to
	// prefer adaptive, so it sits on the adaptive side of the line.
	for _, m := range []string{
		"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-6",
	} {
		t.Run("adaptive/"+m, func(t *testing.T) {
			t.Parallel()
			enabled, adaptive, _ := thinkingFor(t, m, budget)
			if enabled != nil {
				t.Errorf("%s: sent enabled(%d), which this model 400s", m, *enabled)
			}
			if adaptive == "" {
				t.Errorf("%s: no adaptive thinking param built", m)
			}
		})
	}
}

// An unknown model takes adaptive, not the shape that used to be safe.
// Anthropic deprecated enabled at 4-6 and removed it at 4-7, so the
// migration runs one way: guessing "enabled" for a model released
// tomorrow, or for an alias the version cannot be read from, guesses
// wrong.
func TestUnknownModelTakesAdaptive(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"claude-something-9", "claude-latest"} {
		enabled, adaptive, _ := thinkingFor(t, m, &genai.ThinkingConfig{
			ThinkingBudget: genai.Ptr[int32](2048),
		})
		if enabled != nil {
			t.Errorf("%s: sent enabled(%d), want adaptive", m, *enabled)
		}
		if adaptive == "" {
			t.Errorf("%s: no adaptive thinking param built", m)
		}
	}
}

func TestClaudeVersion(t *testing.T) {
	t.Parallel()
	for id, want := range map[string][2]int{
		"claude-opus-4":     {4, 0},
		"claude-opus-4-1":   {4, 1},
		"claude-haiku-4-5":  {4, 5},
		"claude-sonnet-5":   {5, 0},
		"claude-haiku-5-5":  {5, 5},
		"claude-3-7-sonnet": {3, 7},
		"claude-3-opus":     {3, 0},
	} {
		major, minor, ok := claudeVersion(id)
		if !ok || major != want[0] || minor != want[1] {
			t.Errorf("claudeVersion(%q) = %d.%d, %v; want %d.%d", id, major, minor, ok, want[0], want[1])
		}
	}
	// Ids it cannot place take adaptive by way of ok=false.
	for _, id := range []string{"claude", "claude-latest", "gpt-5", "claude-opus-4-5-preview-1", "claude-opus-202511"} {
		if _, _, ok := claudeVersion(id); ok {
			t.Errorf("claudeVersion(%q) placed it; want ok=false", id)
		}
	}
}

// A zero budget is the only way genai can say "do not think", and
// before #369 it was the one request that could not say it: mast sent
// no thinking param and the model thought anyway (measured — opus-5
// returns a signed thinking block on a bare request). `disabled` is the
// one shape every model in the matrix accepts.
func TestZeroBudgetDisablesThinking(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"claude-opus-5", "claude-opus-4-5", "claude-haiku-4-5"} {
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []*genai.ThinkingConfig{
				{ThinkingBudget: genai.Ptr[int32](0)},
				{ThinkingBudget: genai.Ptr[int32](-1)},
				// "Show me the thinking" loses to "do not think":
				// there is nothing to show.
				{ThinkingBudget: genai.Ptr[int32](0), IncludeThoughts: true},
			} {
				enabled, adaptive, disabled := thinkingFor(t, m, tc)
				if !disabled {
					t.Errorf("%s %+v: thinking not disabled (enabled=%v adaptive=%q)", m, tc, enabled, adaptive)
				}
				if enabled != nil || adaptive != "" {
					t.Errorf("%s %+v: disabled must be the only shape set", m, tc)
				}
			}
		})
	}
}

// display is the Anthropic equivalent IncludeThoughts never had. The
// default is omitted: the signature still comes back so a tool loop
// still replays (#357), but mast does not pay for reasoning text that
// #370 filters out of every surface it has. Asking for it is the
// caller's explicit decision, and the one OQ 5 will route.
func TestIncludeThoughtsSelectsDisplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		tc      *genai.ThinkingConfig
		want    string
		wantAny bool
	}{
		{"budget alone omits the text", &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048)}, "omitted", true},
		{"IncludeThoughts asks for it", &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048), IncludeThoughts: true}, "summarized", true},
		// IncludeThoughts alone had no Anthropic equivalent before
		// adaptive existed; it has one now, so it stops being a no-op.
		{"IncludeThoughts alone", &genai.ThinkingConfig{IncludeThoughts: true}, "summarized", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, adaptive, _ := thinkingFor(t, "claude-opus-5", tt.tc)
			if adaptive != tt.want {
				t.Errorf("display = %q, want %q", adaptive, tt.want)
			}
		})
	}
}

// An absent ThinkingConfig still sends no thinking param at all, so a
// caller who never mentions thinking gets a byte-identical request and
// the vendor's own default. #369 changes what mast does when asked, not
// what it does when not asked.
func TestNoThinkingConfigSendsNothing(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"claude-opus-5", "claude-opus-4-5"} {
		enabled, adaptive, disabled := thinkingFor(t, m, nil)
		if enabled != nil || adaptive != "" || disabled {
			t.Errorf("%s: thinking param set for a nil ThinkingConfig", m)
		}
	}
	// IncludeThoughts is false and there is no budget: nothing was
	// asked for, so nothing is sent.
	enabled, adaptive, disabled := thinkingFor(t, "claude-opus-5", &genai.ThinkingConfig{})
	if enabled != nil || adaptive != "" || disabled {
		t.Error("empty ThinkingConfig: thinking param set")
	}
}

// A pinned snapshot has to resolve to its family or the shape is chosen
// off a string the table has never seen. Anthropic writes the suffix
// with a dash on the first-party API and with an @ on Vertex; both
// appear in mast configs, and anthropic.go's own doc comment uses the @
// form.
func TestBaseModelIDStripsSnapshots(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, want string }{
		{"claude-opus-4-5", "claude-opus-4-5"},
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"claude-opus-4-5@20251101", "claude-opus-4-5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"claude-opus-5", "claude-opus-5"},
		// Not a snapshot: eight digits is the shape, and a shorter or
		// longer run is part of the name.
		{"claude-opus-4-5-2025", "claude-opus-4-5-2025"},
		{"claude-opus-4-5-202511010", "claude-opus-4-5-202511010"},
		{"claude-opus-4-5-2025110x", "claude-opus-4-5-2025110x"},
	} {
		if got := baseModelID(tt.in); got != tt.want {
			t.Errorf("baseModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// The point of the stripping: a pinned legacy model still gets the
	// legacy shape.
	for _, m := range []string{"claude-opus-4-5-20251101", "claude-opus-4-5@20251101", "claude-haiku-4-5-20251001"} {
		if !legacyThinkingModel(m) {
			t.Errorf("legacyThinkingModel(%q) = false; a pinned snapshot would be sent adaptive, which it 400s", m)
		}
	}
}

// The enabled shape has a rule the API enforces with a 400:
// 1024 <= budget_tokens < max_tokens. A request outside it is fitted
// rather than sent to fail.
func TestLegacyBudgetIsFittedToTheAPIRule(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		budget, maxOut         int32
		wantBudget, wantMaxTok int64
	}{
		"in range is untouched":          {2048, 8192, 2048, 8192},
		"below the floor is raised":      {100, 8192, 1024, 8192},
		"at max_tokens is lowered":       {8192, 8192, 8191, 8192},
		"above max_tokens is lowered":    {50000, 8192, 8191, 8192},
		"tiny max_tokens makes room":     {2048, 512, 1024, 1025},
		"default max_tokens, big budget": {40000, 0, DefaultMaxTokens - 1, DefaultMaxTokens},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(tc.budget)}}
			if tc.maxOut > 0 {
				cfg.MaxOutputTokens = tc.maxOut
			}
			p, err := buildParams("claude-opus-4-5", userText("hi"), cfg, CacheOptions{}, BuiltinTools{})
			if err != nil {
				t.Fatal(err)
			}
			if p.Thinking.OfEnabled == nil {
				t.Fatal("no enabled thinking param")
			}
			if got := p.Thinking.OfEnabled.BudgetTokens; got != tc.wantBudget {
				t.Errorf("budget_tokens = %d, want %d", got, tc.wantBudget)
			}
			if p.MaxTokens != tc.wantMaxTok {
				t.Errorf("max_tokens = %d, want %d", p.MaxTokens, tc.wantMaxTok)
			}
		})
	}
}

// With thinking on, the API 400s a temperature other than 1, any top_k,
// and a top_p under 0.95. A config carrying them is fitted; with
// thinking off or disabled, every knob passes through as set.
func TestSamplingIsFittedWhenThinking(t *testing.T) {
	t.Parallel()
	sampling := func(tc *genai.ThinkingConfig, temp float32) *genai.GenerateContentConfig {
		return &genai.GenerateContentConfig{
			Temperature: genai.Ptr(temp), TopP: genai.Ptr[float32](0.5), TopK: genai.Ptr[float32](40),
			ThinkingConfig: tc,
		}
	}
	budget := &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](2048)}
	for _, m := range []string{"claude-opus-4-5", "claude-opus-5"} { // enabled, adaptive
		p, err := buildParams(m, userText("hi"), sampling(budget, 0.2), CacheOptions{}, BuiltinTools{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Temperature.Valid() || p.TopK.Valid() {
			t.Errorf("%s: temperature %v, top_k %v sent with thinking on", m, p.Temperature, p.TopK)
		}
		if !p.TopP.Valid() || p.TopP.Value != minThinkingTopP {
			t.Errorf("%s: top_p = %v, want raised to %v", m, p.TopP, minThinkingTopP)
		}
		p, _ = buildParams(m, userText("hi"), sampling(budget, 1), CacheOptions{}, BuiltinTools{})
		if !p.Temperature.Valid() || p.Temperature.Value != 1 {
			t.Errorf("%s: temperature 1 is allowed with thinking and was dropped", m)
		}
	}
	off := &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}
	for name, tc := range map[string]*genai.ThinkingConfig{"no thinking config": nil, "thinking disabled": off} {
		p, err := buildParams("claude-opus-5", userText("hi"), sampling(tc, 0.2), CacheOptions{}, BuiltinTools{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Temperature.Value != float64(float32(0.2)) || p.TopK.Value != 40 || p.TopP.Value != float64(float32(0.5)) {
			t.Errorf("%s: temperature %v, top_k %v, top_p %v; want them as set", name, p.Temperature, p.TopK, p.TopP)
		}
	}
}
