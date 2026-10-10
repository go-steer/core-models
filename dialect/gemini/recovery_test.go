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

// Originally derived from go-steer/mast@6568618:internal/providers/gemini/{cache_eviction_retry,empty_response}_test.go and go-steer/core-agent@9d3eba89:pkg/models/gemini/{cache_eviction_retry,empty_response}_test.go

package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
)

// The three shapes Vertex sends for one dead cache. The first two are
// verbatim from live daemons (mast #325's reaped handle; core-agent
// #902's 27-hour-uptime expiry, note "Cache content" with no d).
const (
	reapedText   = "Error 404, Message: Not found: cached content metadata for 6116704758662168576., Status: NOT_FOUND, Details: []"
	expiredText  = "Error 400, Message: Cache content 7016131366404227072 is expired., Status: INVALID_ARGUMENT, Details: []"
	updateText   = "Error 404, Message: Cached content 123 is not found., Status: NOT_FOUND, Details: []"
	notFoundText = "Error 404, Message: Publisher model gemini-9 was not found, Status: NOT_FOUND, Details: []"
)

var (
	reapedErr  = vertexErr(reapedText)
	expiredErr = vertexErr(expiredText)
	updateErr  = vertexErr(updateText)
)

// vertexErr is an error with Vertex's own text, capitals and all.
func vertexErr(text string) error { return errors.New(text) }

func cachedRequest() *llm.Request {
	return &llm.Request{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "system prompt"}}},
		Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "my_func"}}}},
	}}
}

// TestEvictionRetriesUncachedAndInvalidates walks every gone shape
// through the wrapper: invalidate once, retry once uncached with the
// stripped fields restored, and hand the caller only the retry.
func TestEvictionRetriesUncachedAndInvalidates(t *testing.T) {
	t.Parallel()
	for name, gone := range map[string]error{"reaped (mast #325)": reapedErr, "expired (core-agent #902)": expiredErr, "update shape": updateErr} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeLLM{scripts: [][]fakeEvent{{{err: gone}}, {{resp: text("recovered")}}}}
			var reasons []string
			m := &model{
				inner:      fake,
				cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "projects/p/locations/l/cachedContents/dead" },
				cacheInvalidate: func(_, r string) { reasons = append(reasons, r) },
			}
			out, err := drain(t, m.GenerateContent(context.Background(), cachedRequest(), false))
			if err != nil || len(out) != 1 || out[0].Content.Parts[0].Text != "recovered" {
				t.Fatalf("caller saw %v, %v; want only the retry's answer", out, err)
			}
			if len(reasons) != 1 || !strings.Contains(reasons[0], gone.Error()) {
				t.Errorf("invalidate reasons = %q; want one, quoting the error", reasons)
			}
			if fake.calls() != 2 {
				t.Fatalf("inner calls = %d, want 2", fake.calls())
			}
			retry := fake.last().Config
			if retry.CachedContent != "" || retry.SystemInstruction == nil || len(retry.Tools) != 1 {
				t.Errorf("retry sent cache %q, sys %v, %d tools; want uncached with everything restored",
					retry.CachedContent, retry.SystemInstruction, len(retry.Tools))
			}
		})
	}
}

func TestEvictionLeavesOtherErrorsAndUncachedTurnsAlone(t *testing.T) {
	t.Parallel()
	notCache := errors.New(notFoundText)
	fake := &fakeLLM{scripts: [][]fakeEvent{{{err: notCache}}}}
	invalidated := 0
	m := &model{inner: fake, cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "c" }, cacheInvalidate: func(string, string) { invalidated++ }}
	if _, err := drain(t, m.GenerateContent(context.Background(), cachedRequest(), false)); !errors.Is(err, notCache) {
		t.Errorf("err = %v, want the model-not-found error untouched", err)
	}
	if invalidated != 0 || fake.calls() != 1 {
		t.Errorf("a plain NOT_FOUND invalidated %d times over %d calls; want 0 and 1", invalidated, fake.calls())
	}

	// An uncached turn has no cache to lose: a gone-shaped error passes
	// through.
	fake = &fakeLLM{scripts: [][]fakeEvent{{{err: expiredErr}}}}
	m = &model{inner: fake, cacheInvalidate: func(string, string) { invalidated++ }}
	if _, err := drain(t, m.GenerateContent(context.Background(), cachedRequest(), false)); !errors.Is(err, expiredErr) || fake.calls() != 1 {
		t.Errorf("uncached turn: err %v over %d calls; want the error, one call", err, fake.calls())
	}
}

func TestEvictionWithoutAnInvalidateHookStillRetries(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{scripts: [][]fakeEvent{{{err: reapedErr}}, {{resp: text("ok")}}}}
	m := &model{inner: fake, cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "c" }}
	if out, err := drain(t, m.GenerateContent(context.Background(), cachedRequest(), false)); err != nil || len(out) != 1 {
		t.Errorf("got %v, %v", out, err)
	}
}

func TestUsable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		r    *llm.Response
		want bool
	}{
		"nil":                   {nil, false},
		"parts":                 {text("hi"), true},
		"no parts":              {&llm.Response{Content: &genai.Content{}}, false},
		"bare STOP (#220)":      {&llm.Response{FinishReason: genai.FinishReasonStop}, false},
		"SAFETY":                {&llm.Response{FinishReason: genai.FinishReasonSafety}, true},
		"MAX_TOKENS":            {&llm.Response{FinishReason: genai.FinishReasonMaxTokens}, true},
		"error code":            {&llm.Response{ErrorCode: "PROHIBITED_CONTENT"}, true},
		"heartbeat, usage only": {&llm.Response{Content: &genai.Content{Parts: []*genai.Part{}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{}}, false},
	} {
		if got := usable(tc.r); got != tc.want {
			t.Errorf("%s: usable = %v, want %v", name, got, tc.want)
		}
	}
}

// TestEmptyAnswerIsRetriedOnceThenSurfaced is mast #220 end to end
// through the wrapper: a silent turn is retried, and a second silent
// turn becomes ErrEmptyResponse — which callers can recognise through
// the shared sentinel without importing this package.
func TestEmptyAnswerIsRetriedOnceThenSurfaced(t *testing.T) {
	t.Parallel()
	silent := []fakeEvent{{resp: &llm.Response{FinishReason: genai.FinishReasonStop}}}

	var notes []string
	fake := &fakeLLM{scripts: [][]fakeEvent{silent, {{resp: text("second time lucky")}}}}
	m := &model{inner: fake, logf: func(f string, _ ...any) { notes = append(notes, f) }}
	out, err := drain(t, m.GenerateContent(context.Background(), &llm.Request{}, false))
	if err != nil || len(out) != 1 || out[0].Content.Parts[0].Text != "second time lucky" {
		t.Fatalf("got %v, %v; want only the retry's answer — the empty attempt must not reach the caller", out, err)
	}
	if fake.calls() != 2 || len(notes) != 2 {
		t.Errorf("calls %d, notices %d; want 2 and 2 (detected, recovered)", fake.calls(), len(notes))
	}

	fake = &fakeLLM{scripts: [][]fakeEvent{silent}}
	m = &model{inner: fake}
	_, err = drain(t, m.GenerateContent(context.Background(), &llm.Request{}, false))
	if !errors.Is(err, ErrEmptyResponse) || !errors.Is(err, llm.ErrEmptyResponse) {
		t.Errorf("err = %v, want ErrEmptyResponse wrapping llm.ErrEmptyResponse", err)
	}
	if fake.calls() != 2 {
		t.Errorf("calls = %d, want 2 (one retry, never more)", fake.calls())
	}
}

func TestARealErrorIsNotRetriedAsEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{scripts: [][]fakeEvent{{{err: errBoom}}}}
	m := &model{inner: fake}
	if _, err := drain(t, m.GenerateContent(context.Background(), &llm.Request{}, false)); !errors.Is(err, errBoom) || errors.Is(err, ErrEmptyResponse) {
		t.Errorf("err = %v, want the real error alone", err)
	}
	if fake.calls() != 1 {
		t.Errorf("calls = %d, want 1", fake.calls())
	}
}

// TestHeartbeatsBeforeContentPassThrough: a stream that warms up with
// usage-only chunks and then answers is usable, and nothing is lost.
func TestHeartbeatsBeforeContentPassThrough(t *testing.T) {
	t.Parallel()
	hb := &llm.Response{Content: &genai.Content{Parts: []*genai.Part{}}, Partial: true}
	fake := &fakeLLM{scripts: [][]fakeEvent{{{resp: hb}, {resp: hb}, {resp: text("grounded answer")}}}}
	m := &model{inner: fake}
	out, err := drain(t, m.GenerateContent(context.Background(), &llm.Request{}, true))
	if err != nil || len(out) != 3 || fake.calls() != 1 {
		t.Errorf("got %d responses, err %v, %d calls; want all 3, no error, 1 call", len(out), err, fake.calls())
	}
}
