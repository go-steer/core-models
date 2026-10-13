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
	"encoding/json"
	"testing"
)

// TestLongContextOf reads the plain above-threshold fields, ignores the
// service-tier variants and a zero rate, and needs an input rate.
func TestLongContextOf(t *testing.T) {
	for name, tc := range map[string]struct {
		entry string
		want  LongContext
		ok    bool
	}{
		"openai 272k": {`{"input_cost_per_token": 2.5e-06, "input_cost_per_token_above_272k_tokens": 5e-06, "output_cost_per_token_above_272k_tokens": 2.25e-05, "cache_read_input_token_cost_above_272k_tokens": 5e-07, "input_cost_per_token_above_272k_tokens_priority": 9e-06}`,
			LongContext{ThresholdTokens: 272000, Input: 5e-06, Output: 2.25e-05, CacheRead: 5e-07}, true},
		"claude 100k with 1h write": {`{"input_cost_per_token_above_100k_tokens": 5e-07, "cache_creation_input_token_cost_above_100k_tokens": 6.25e-07, "cache_creation_input_token_cost_above_1hr_above_100k_tokens": 1e-06}`,
			LongContext{ThresholdTokens: 100000, Input: 5e-07, CacheCreation: 6.25e-07, CacheCreation1hr: 1e-06}, true},
		"none":              {`{"input_cost_per_token": 1e-06}`, LongContext{}, false},
		"zero is absent":    {`{"input_cost_per_token_above_200k_tokens": 0}`, LongContext{}, false},
		"no input rate":     {`{"output_cost_per_token_above_200k_tokens": 1e-05}`, LongContext{}, false},
		"tier variant only": {`{"input_cost_per_token_above_272k_tokens_flex": 2e-06}`, LongContext{}, false},
		"two thresholds, lowest wins": {`{"input_cost_per_token_above_200k_tokens": 2e-06, "input_cost_per_token_above_128k_tokens": 1.5e-06}`,
			LongContext{ThresholdTokens: 128000, Input: 1.5e-06}, true},
	} {
		got, ok := LongContextOf(json.RawMessage(tc.entry))
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: LongContextOf = %+v, %v; want %+v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}
