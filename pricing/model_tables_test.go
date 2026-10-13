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

// Originally derived from go-steer/mast@020004fe:internal/pricing/model_tables_test.go (the two tests that check the generated tables against each other; mast's modeltier check stays with mast)

package pricing_test

import (
	"testing"

	"github.com/go-steer/core-models/pricing"
)

// TestBuiltinTablesHaveMatchingKeys pins the two generated maps to each
// other. They are emitted from one pass over one filtered slice, so
// they can only diverge if someone hand-edits the DO-NOT-EDIT file —
// and a rate row with no window row would silently drop that model back
// onto a product's coarse substring fallback, which is the exact failure
// the window table was generated to end.
func TestBuiltinTablesHaveMatchingKeys(t *testing.T) {
	rates, windows := pricing.Builtin(), pricing.BuiltinContextWindows()
	for id := range rates {
		if _, ok := windows[id]; !ok {
			t.Errorf("%q has a rate but no context window — regenerate with "+
				"`go run ./dev/regen-builtin-pricing`", id)
		}
	}
	for id := range windows {
		if _, ok := rates[id]; !ok {
			t.Errorf("%q has a context window but no rate — regenerate with "+
				"`go run ./dev/regen-builtin-pricing`", id)
		}
	}
}

// TestBuiltinContextWindowsArePlausible catches a schema shift upstream
// turning max_input_tokens into something that is not a token count.
// The generator refuses to emit a non-positive window, so the interesting
// bound is the upper one: a window an order of magnitude past anything
// shipping today would push compaction triggers out past the point the
// provider hard-fails, and the symptom is a dead long session rather
// than a failed build.
func TestBuiltinContextWindowsArePlausible(t *testing.T) {
	const (
		floor   = 100_000    // no current Gemini/Anthropic chat model is smaller
		ceiling = 20_000_000 // ~10x the largest window on offer in 2026
	)
	for id, n := range pricing.BuiltinContextWindows() {
		if n < floor || n > ceiling {
			t.Errorf("BuiltinContextWindow(%q) = %d, outside the plausible "+
				"[%d, %d] range — check LiteLLM's max_input_tokens for a "+
				"units change", id, n, floor, ceiling)
		}
	}
}
