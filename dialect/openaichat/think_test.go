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

package openaichat

import "testing"

// Every input is fed whole, then split at every possible point, then
// one byte at a time; the result must not depend on how the server
// chunked it, because servers chunk however they like (Ollama sends
// "<think>" as a token of its own).
func TestThinkSplitterIgnoresChunking(t *testing.T) {
	for name, tc := range map[string]struct{ in, reasoning, answer string }{
		"block then answer":     {"<think>\nweigh it\n</think>\n\npong", "\nweigh it\n", "pong"},
		"leading whitespace":    {"  \n<think>x</think>y", "x", "y"},
		"no block":              {"just an answer", "", "just an answer"},
		"tag later is answer":   {"use <think> tags like </think> this", "", "use <think> tags like </think> this"},
		"unterminated block":    {"<think>still going", "still going", ""},
		"empty block":           {"<think></think>answer", "", "answer"},
		"lookalike close":       {"<think>a </thin b</think>c", "a </thin b", "c"},
		"answer keeps its tail": {"<think>r</think>a\n\nb ", "r", "a\n\nb "},
		"only whitespace":       {"   ", "", ""},
		"prefix of open tag":    {"<thi", "", "<thi"},
	} {
		t.Run(name, func(t *testing.T) {
			check := func(label string, pieces []string) {
				t.Helper()
				var sp thinkSplitter
				var r, a string
				for _, p := range pieces {
					pr, pa := sp.feed(p)
					r, a = r+pr, a+pa
				}
				fr, fa := sp.flush()
				r, a = r+fr, a+fa
				if r != tc.reasoning || a != tc.answer {
					t.Errorf("%s %q: reasoning %q answer %q, want %q / %q", label, pieces, r, a, tc.reasoning, tc.answer)
				}
			}
			check("whole", []string{tc.in})
			for i := 1; i < len(tc.in); i++ {
				check("split", []string{tc.in[:i], tc.in[i:]})
			}
			bytes := make([]string, len(tc.in))
			for i := range tc.in {
				bytes[i] = tc.in[i : i+1]
			}
			check("bytewise", bytes)
		})
	}
}
