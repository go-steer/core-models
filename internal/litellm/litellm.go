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

// Package litellm reads the parts of LiteLLM's
// model_prices_and_context_window.json that more than one caller in
// this module needs: the pricing package's Refresh and the
// dev/regen-builtin-pricing generator. One reader, so the two can't
// disagree about what a field means.
package litellm

import (
	"encoding/json"
	"regexp"
	"strconv"
)

// LongContext is one entry's above-threshold prices, per token, as
// LiteLLM publishes them. A zero rate means the field was absent.
type LongContext struct {
	// ThresholdTokens is the prompt size above which these rates apply:
	// LiteLLM's "_above_<N>k_tokens" suffix, with k = 1,000 (OpenAI's
	// "272K" is 272,000 tokens).
	ThresholdTokens  int
	Input            float64
	CacheRead        float64
	CacheCreation    float64
	CacheCreation1hr float64
	Output           float64
}

// aboveRE matches the plain above-threshold fields. The service-tier
// variants ("…_tokens_priority", "_flex", "_batches") deliberately do
// not match: a product bills the standard tier.
var aboveRE = regexp.MustCompile(`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost|cache_creation_input_token_cost_above_1hr)_above_(\d+)k_tokens$`)

// LongContextOf returns an entry's above-threshold prices, or false when
// it publishes none or publishes no above-threshold input rate (the one
// field the others are relative to). LiteLLM publishes at most one
// threshold per model; if an entry ever carried two, the lowest wins,
// because billing the higher tier from the lower threshold would
// overstate and the reverse would understate — the lower threshold is
// the one the provider applies first.
func LongContextOf(entry json.RawMessage) (LongContext, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		return LongContext{}, false
	}
	byThreshold := map[int]*LongContext{}
	for name, raw := range fields {
		m := aboveRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
			continue
		}
		k, err := strconv.Atoi(m[2])
		if err != nil || k <= 0 {
			continue
		}
		lc := byThreshold[k]
		if lc == nil {
			lc = &LongContext{ThresholdTokens: k * 1000}
			byThreshold[k] = lc
		}
		switch m[1] {
		case "input_cost_per_token":
			lc.Input = v
		case "output_cost_per_token":
			lc.Output = v
		case "cache_read_input_token_cost":
			lc.CacheRead = v
		case "cache_creation_input_token_cost":
			lc.CacheCreation = v
		case "cache_creation_input_token_cost_above_1hr":
			lc.CacheCreation1hr = v
		}
	}
	best := 0
	for k, lc := range byThreshold {
		if lc.Input > 0 && (best == 0 || k < best) {
			best = k
		}
	}
	if best == 0 {
		return LongContext{}, false
	}
	return *byThreshold[best], true
}
