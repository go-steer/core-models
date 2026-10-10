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

// Originally derived from go-steer/core-agent@181327cb6ffcfd65c1634ad4e74bd7e9c5825f83:pkg/models/anthropic/cache_ttl_test.go

package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// ttlOf collects the TTL from every marker the request carries, so a
// test can assert the whole request agrees rather than spot-checking
// one block. A request that mixes TTLs would make "which breakpoint
// expired" depend on marker position.
func ttlOf(p anthropic.MessageNewParams) []anthropic.CacheControlEphemeralTTL {
	var out []anthropic.CacheControlEphemeralTTL
	for _, b := range p.System {
		if b.CacheControl.Type != "" {
			out = append(out, b.CacheControl.TTL)
		}
	}
	for _, m := range p.Messages {
		for _, blk := range m.Content {
			if cc := blk.GetCacheControl(); cc != nil && cc.Type != "" {
				out = append(out, cc.TTL)
			}
		}
	}
	return out
}

func ttlParams(t *testing.T, opts CacheOptions) anthropic.MessageNewParams {
	t.Helper()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("be helpful", genai.RoleUser),
	}
	p, err := buildParams("claude-opus-4-7", longHistory(40), cfg, opts, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	return p
}

// The request half of #770: asking for the 1-hour breakpoint has to
// reach the wire. Without the TTL on the marker the provider writes a
// 5-minute entry, the operator's config is silently ignored, and the
// cache expires between the turns it was chosen for.
func TestApplyCacheBreakpoints_StampsTheOneHourTTLOnEveryMarker(t *testing.T) {
	t.Parallel()
	got := ttlOf(ttlParams(t, CacheOptions{System: true, History: true, TTL: TTL1h}))
	if len(got) < 2 {
		t.Fatalf("markers = %d, want the system marker plus history ones", len(got))
	}
	for i, ttl := range got {
		if ttl != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Errorf("marker %d TTL = %q, want %q", i, ttl, anthropic.CacheControlEphemeralTTLTTL1h)
		}
	}
}

// The default must stay 5m and must stay *unset* on the wire: the API
// documents 5m as the default, and omitzero keeps the request byte-
// identical to every pre-#770 one. A request whose bytes changed would
// miss every cache entry written by the previous build.
func TestApplyCacheBreakpoints_DefaultLeavesTheTTLUnset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts CacheOptions
	}{
		{"explicit 5m", CacheOptions{System: true, History: true, TTL: TTL5m}},
		{"zero TTL", CacheOptions{System: true, History: true}},
		{"unrecognized value", CacheOptions{System: true, History: true, TTL: "1hr"}},
	} {
		got := ttlOf(ttlParams(t, tc.opts))
		if len(got) < 2 {
			t.Fatalf("%s: markers = %d, want several", tc.name, len(got))
		}
		for i, ttl := range got {
			if ttl != "" {
				t.Errorf("%s: marker %d TTL = %q, want it omitted (the API default is 5m)", tc.name, i, ttl)
			}
		}
	}
}

// reapplyCacheBreakpoints clears and re-marks a request that grew after
// buildParams returned (the pause_turn continuation). The TTL has to
// survive that round trip or a long-running turn silently downgrades.
func TestReapplyCacheBreakpoints_KeepsTheOneHourTTL(t *testing.T) {
	t.Parallel()
	opts := CacheOptions{System: true, History: true, TTL: TTL1h}
	p := ttlParams(t, opts)
	if got := reapplyCacheBreakpoints(&p, opts); got == 0 {
		t.Fatal("reapply placed no markers")
	}
	for i, ttl := range ttlOf(p) {
		if ttl != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Errorf("marker %d TTL = %q after reapply, want %q", i, ttl, anthropic.CacheControlEphemeralTTLTTL1h)
		}
	}
}

// The response half: Anthropic reports which TTL produced the writes,
// so there is a right answer to bill rather than a guess about what the
// request asked for. The one-hour share is a subset of the total.
func TestUsageDetail_CarriesTheOneHourShare(t *testing.T) {
	t.Parallel()
	u := anthropic.Usage{CacheCreationInputTokens: 1000}
	u.CacheCreation.Ephemeral1hInputTokens = 400
	u.CacheCreation.Ephemeral5mInputTokens = 600

	d := usageDetail(u, "", "", "", "")
	if d.CacheWriteTokens == nil || *d.CacheWriteTokens != 1000 {
		t.Errorf("CacheWriteTokens = %v, want 1000", d.CacheWriteTokens)
	}
	if d.CacheWrite1hTokens == nil || *d.CacheWrite1hTokens != 400 {
		t.Errorf("CacheWrite1hTokens = %v, want 400", d.CacheWrite1hTokens)
	}
}

// An all-5m turn states a one-hour share of zero: the adapter knows the
// answer, and zero is a measurement, not a silence (mast #352).
func TestUsageDetail_StatesAZeroOneHourShare(t *testing.T) {
	t.Parallel()
	u := anthropic.Usage{CacheCreationInputTokens: 1000}
	u.CacheCreation.Ephemeral5mInputTokens = 1000
	d := usageDetail(u, "", "", "", "")
	if d.CacheWrite1hTokens == nil || *d.CacheWrite1hTokens != 0 {
		t.Errorf("CacheWrite1hTokens = %v, want a stated 0", d.CacheWrite1hTokens)
	}
}

// The pause_turn continuation loop issues several requests and folds
// their usage into one turn. Missing the per-TTL fold would leave a
// continuation's 1-hour writes priced at the 5-minute rate.
func TestAddUsage_FoldsThePerTTLSplit(t *testing.T) {
	t.Parallel()
	var dst anthropic.Usage
	for range 3 {
		src := anthropic.Usage{CacheCreationInputTokens: 100}
		src.CacheCreation.Ephemeral1hInputTokens = 70
		src.CacheCreation.Ephemeral5mInputTokens = 30
		addUsage(&dst, src)
	}
	if dst.CacheCreationInputTokens != 300 {
		t.Errorf("total = %d, want 300", dst.CacheCreationInputTokens)
	}
	if dst.CacheCreation.Ephemeral1hInputTokens != 210 {
		t.Errorf("1h = %d, want 210", dst.CacheCreation.Ephemeral1hInputTokens)
	}
	if dst.CacheCreation.Ephemeral5mInputTokens != 90 {
		t.Errorf("5m = %d, want 90", dst.CacheCreation.Ephemeral5mInputTokens)
	}
}
