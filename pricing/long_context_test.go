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
	"encoding/json"
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// gemini-2.5-pro's shape: one threshold, every bucket published.
var longRates = Rates{
	InputPerMTok: 1.25, CachedInputPerMTok: 0.125, OutputPerMTok: 10,
	LongContext: &LongContext{ThresholdTokens: 200_000, InputPerMTok: 2.5, CachedInputPerMTok: 0.25, OutputPerMTok: 15},
}

// TestLongContextAppliesAboveTheThreshold: a prompt at or under the
// threshold bills at the base rates; one token over, every bucket and
// the output bill at the long-context rates (the provider charges the
// whole call at the higher tier).
func TestLongContextAppliesAboveTheThreshold(t *testing.T) {
	at := longRates.CostUSDWithCache(200_000, 0, 1_000_000)
	if want := 0.2*1.25 + 1*10; !near(at, want) {
		t.Errorf("at threshold = %v, want base %v", at, want)
	}
	over := longRates.CostUSDWithCache(150_001, 50_000, 1_000_000)
	if want := 0.150001*2.5 + 0.05*0.25 + 1*15; !near(over, want) {
		t.Errorf("over threshold = %v, want long-context %v", over, want)
	}
	if got := longRates.At(200_001); got.LongContext != nil || got.InputPerMTok != 2.5 {
		t.Errorf("At(200001) = %+v, want long rates and no nested LongContext", got)
	}
	if got := longRates.At(10); got.InputPerMTok != 1.25 {
		t.Errorf("At(10) input = %v, want base 1.25", got.InputPerMTok)
	}
}

// TestLongContextUnpublishedBucketTracksTheInputIncrease: a bucket the
// long-context row leaves at zero is not billed at the cheaper base
// price — input-side buckets scale by the input multiplier, output
// falls back to base.
func TestLongContextUnpublishedBucketTracksTheInputIncrease(t *testing.T) {
	r := Rates{
		InputPerMTok: 3, CachedInputPerMTok: 0.3, CacheCreationInputPerMTok: 3.75, CacheCreation1hInputPerMTok: 6, OutputPerMTok: 15,
		LongContext: &LongContext{ThresholdTokens: 100_000, InputPerMTok: 6},
	}
	got := r.At(100_001)
	if !near(got.CachedInputPerMTok, 0.6) || !near(got.CacheCreationInputPerMTok, 7.5) || !near(got.CacheCreation1hInputPerMTok, 12) {
		t.Errorf("scaled buckets = %+v, want 2x the base input-side rates", got)
	}
	if got.OutputPerMTok != 15 {
		t.Errorf("output = %v, want the base 15 when the long row publishes none", got.OutputPerMTok)
	}
}

// TestLongContextRoundTripsThroughThePricingFile: an operator's file can
// carry a long-context row, and it reaches Rates unchanged.
func TestLongContextRoundTripsThroughThePricingFile(t *testing.T) {
	raw := `{"input_per_mtok": 1.25, "output_per_mtok": 10, "long_context": {"threshold_tokens": 200000, "input_per_mtok": 2.5, "output_per_mtok": 15}}`
	var m ModelRates
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	r := m.Rates()
	if r.LongContext == nil || r.LongContext.ThresholdTokens != 200000 || r.LongContext.OutputPerMTok != 15 {
		t.Errorf("Rates() = %+v, want the long-context row carried", r)
	}
}

// TestBuiltinCarriesLongContextRows pins that the regen populates
// LongContext from LiteLLM's above-threshold fields: gemini-2.5-pro has
// published a 200K tier for as long as it has existed.
func TestBuiltinCarriesLongContextRows(t *testing.T) {
	r, ok := builtin["gemini-2.5-pro"]
	if !ok {
		t.Skip("gemini-2.5-pro left the builtin table; pick another long-context model")
	}
	if r.LongContext == nil || r.LongContext.ThresholdTokens != 200_000 || r.LongContext.InputPerMTok <= r.InputPerMTok {
		t.Errorf("gemini-2.5-pro LongContext = %+v, want a 200K tier above the base input", r.LongContext)
	}
	n := 0
	for _, r := range BuiltinByBackend() {
		if r.LongContext != nil {
			n++
		}
	}
	if n == 0 {
		t.Error("no qualified row carries LongContext; OpenAI's 272K tier should")
	}
}

// TestUnpricedIsAMiss: core-models invents no default rate. A
// self-hosted model nobody priced is a lookup miss ("$—"), on the bare
// id and through LookupFor alike (design §9).
func TestUnpricedIsAMiss(t *testing.T) {
	c, err := NewCatalog(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := c.Lookup("google/gemma-4-26b-a4b-it"); ok {
		t.Errorf("an unpriced self-hosted model resolved to %+v", r)
	}
	if r, ok := c.LookupFor("house-vllm", "google/gemma-4-26b-a4b-it"); ok {
		t.Errorf("LookupFor priced an unpriced model at %+v", r)
	}
}

// TestRefreshCarriesLongContext: Refresh's LiteLLM reader fills
// ModelRates.LongContext the same way the generator fills builtin.
func TestRefreshCarriesLongContext(t *testing.T) {
	body := []byte(`{"gpt-x": {"mode": "chat", "input_cost_per_token": 2.5e-06, "output_cost_per_token": 1.5e-05,
		"input_cost_per_token_above_272k_tokens": 5e-06, "output_cost_per_token_above_272k_tokens": 2.25e-05}}`)
	got, err := parseLiteLLMBody(body)
	if err != nil {
		t.Fatal(err)
	}
	lc := got["gpt-x"].LongContext
	if lc == nil || lc.ThresholdTokens != 272000 || !near(lc.InputPerMTok, 5) || !near(lc.OutputPerMTok, 22.5) {
		t.Errorf("LongContext = %+v, want 272K at $5/$22.5", lc)
	}
}
