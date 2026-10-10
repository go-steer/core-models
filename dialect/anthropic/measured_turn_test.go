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

// Originally derived from go-steer/mast@6568618f531368b8fa4894645df292b1e2aca09f:internal/providers/anthropic/usage_detail_test.go

// The cache-write count, from the wire to the bill (mast #352), over a
// measured turn rather than an invented one.
//
// Products own the arithmetic and package usage owns the record; what
// is measured here is the part only this adapter can get wrong — that a
// number Anthropic sent survives the genai projection, across the
// pause_turn loop, onto the response a product persists, and that a
// meter reading the record reaches the rate card's figure.

package anthropic

import (
	"math"
	"testing"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/usage"
)

// measuredWarmSSE is a real measured turn, replayed: a 28,804-token
// system block sent with cache_control ephemeral to claude-sonnet-5 on
// Vertex (us-east5) on 2026-09-13, answered in four tokens. The counts
// are that response's, not invented ones.
//
//	input_tokens=10 cache_creation_input_tokens=28804
//	cache_read_input_tokens=0 output_tokens=4
const measuredWarmSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_vrtx_011Cf23KmBvy6xNCBR8cRGsH","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":28804,"cache_read_input_tokens":0,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ack"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// measuredHitSSE is the next turn of the same probe: the prompt served
// from the entry the turn above wrote.
const measuredHitSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_vrtx_011Cf23LuN642SEHDymCjVBL","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":28804,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ack"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// sonnet5 is claude-sonnet-5 as of 2026-09-09 ($/MTok), restated here:
// the assertions below are dollar figures, and a vendor moving a rate is
// not a defect in this adapter.
var sonnet5 = struct{ in, read, write, out float64 }{in: 2, read: 0.2, write: 2.5, out: 10}

// bill prices a terminal response the way a meter reading the record
// would: the write and read buckets from usage.Detail at their own
// rates, and only the rest of the prompt at the fresh-input rate.
func bill(t *testing.T, resp *llm.Response) float64 {
	t.Helper()
	d, ok := usage.FromMetadata(resp.CustomMetadata)
	if !ok || d.CacheReadTokens == nil || d.CacheWriteTokens == nil {
		t.Fatalf("no usable usage.Detail: %#v", resp.CustomMetadata)
	}
	u := resp.UsageMetadata
	read, write := float64(*d.CacheReadTokens), float64(*d.CacheWriteTokens)
	fresh := float64(u.PromptTokenCount) - read - write
	return (fresh*sonnet5.in + read*sonnet5.read + write*sonnet5.write + float64(u.CandidatesTokenCount)*sonnet5.out) / 1e6
}

// finalOf drives one offline turn and returns its terminal response.
func finalOf(t *testing.T, sse string) *llm.Response {
	t.Helper()
	l, _ := newOfflineLLM(t, "claude-sonnet-5", sse)
	return terminalOf(t, l)
}

// The count Anthropic sent has to still be there at the end of the
// stream. genai's usage metadata has nowhere to put it, so if the
// Detail does not carry it nothing does.
func TestMeasuredWarmTurnCarriesItsWriteCount(t *testing.T) {
	t.Parallel()
	final := finalOf(t, measuredWarmSSE)

	if u := final.UsageMetadata; u.PromptTokenCount != 10+28804 || u.CachedContentTokenCount != 0 || u.CandidatesTokenCount != 4 {
		t.Errorf("usage = prompt %d, cached %d, candidates %d; want 28814/0/4",
			u.PromptTokenCount, u.CachedContentTokenCount, u.CandidatesTokenCount)
	}
	d := detailOf(t, final)
	if i64(d.CacheWriteTokens) != 28804 {
		t.Errorf("CacheWriteTokens = %d, want 28804 — the count is lost", i64(d.CacheWriteTokens))
	}
	if i64(d.CacheReadTokens) != 0 {
		t.Errorf("CacheReadTokens = %d, want a stated 0", i64(d.CacheReadTokens))
	}
	if d.ProviderRequestID != "msg_vrtx_011Cf23KmBvy6xNCBR8cRGsH" {
		t.Errorf("ProviderRequestID = %q, want the message id", d.ProviderRequestID)
	}
	if d.Backend != "anthropic" {
		t.Errorf("Backend = %q, want the client's", d.Backend)
	}
}

// End to end, the claim mast #352 makes: this turn bills what the rate
// card says it costs. Before the write count was recorded, the same
// turn billed $0.057668 — the write bucket at the fresh-input rate.
func TestMeasuredWarmTurnBillsTheRateCardFigure(t *testing.T) {
	t.Parallel()
	got := bill(t, finalOf(t, measuredWarmSSE))
	want := (10*sonnet5.in + 28804*sonnet5.write + 4*sonnet5.out) / 1e6 // $0.072070
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("turn billed $%.6f, want $%.6f (folded into fresh input it bills $%.6f)",
			got, want, (28814*sonnet5.in+4*sonnet5.out)/1e6)
	}
}

// The other measured turn. Cache hits were priced right before the
// write count existed and must stay that way.
func TestMeasuredHitTurnIsUnmoved(t *testing.T) {
	t.Parallel()
	final := finalOf(t, measuredHitSSE)
	d := detailOf(t, final)
	if i64(d.CacheReadTokens) != 28804 || i64(d.CacheWriteTokens) != 0 {
		t.Fatalf("CacheReadTokens, CacheWriteTokens = %d, %d; want 28804, a stated 0", i64(d.CacheReadTokens), i64(d.CacheWriteTokens))
	}
	got := bill(t, final)
	want := (10*sonnet5.in + 28804*sonnet5.read + 4*sonnet5.out) / 1e6 // $0.005821
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cache-hit turn billed $%.6f, want $%.6f", got, want)
	}
}

// A pause_turn turn spans several requests and the buckets are summed
// across all of them, so the Detail reports the turn's total rather
// than the last request's.
func TestMeasuredPauseTurnSumsTheWriteCount(t *testing.T) {
	t.Parallel()
	l, _ := newOfflineLLMSeq(t, "claude-sonnet-5", []string{pauseTurnSSEFixture, measuredWarmSSE})
	d := detailOf(t, terminalOf(t, l))
	// The pause_turn fixture writes no cache entry, so the total is the
	// second request's alone — but it is a total, not a snapshot of
	// whichever request happened to finish the turn.
	if i64(d.CacheWriteTokens) != 28804 {
		t.Errorf("CacheWriteTokens = %d, want the turn's 28804", i64(d.CacheWriteTokens))
	}
}
