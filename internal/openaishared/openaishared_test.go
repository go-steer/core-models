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

package openaishared

import "testing"

func i64(n int64) *int64 { return &n }

// TestMapUsageTellsTheReasoningConventionsApart: OpenAI counts reasoning
// inside completion tokens, xAI beside them. Both must yield the same
// visible-answer count, or an xAI reasoning reply bills as all thought
// and no answer (found live against grok-4.20-0309-reasoning).
func TestMapUsageTellsTheReasoningConventionsApart(t *testing.T) {
	for name, tc := range map[string]struct {
		u                Usage
		answer, thoughts int32
	}{
		"openai: reasoning inside output":   {Usage{Input: i64(13), Output: i64(87), Reasoning: i64(64), Total: i64(100)}, 23, 64},
		"xai: reasoning beside output":      {Usage{Input: i64(193), Output: i64(1), Reasoning: i64(99), Total: i64(293)}, 1, 99},
		"xai: answer longer than reasoning": {Usage{Input: i64(100), Output: i64(500), Reasoning: i64(200), Total: i64(800)}, 500, 200},
		"no total: openai's convention":     {Usage{Input: i64(13), Output: i64(87), Reasoning: i64(64)}, 23, 64},
		"no reasoning":                      {Usage{Input: i64(10), Output: i64(5), Total: i64(15)}, 5, 0},
	} {
		um, _ := MapUsage(&tc.u, Stamp{}, nil)
		if um.CandidatesTokenCount != tc.answer || um.ThoughtsTokenCount != tc.thoughts {
			t.Errorf("%s: answer=%d thoughts=%d, want %d and %d", name, um.CandidatesTokenCount, um.ThoughtsTokenCount, tc.answer, tc.thoughts)
		}
	}
}
