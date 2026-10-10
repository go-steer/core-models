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

// Originally derived from go-steer/core-agent@15791a04de1575687424602e5f57395ae23fcb73:pkg/models/anthropic/transient_retry_test.go

// Transient-error retry. core-agent left this to anthropic-sdk-go's own
// retries (#935). In core-models the SDK's retries are off and package
// retry does it in the HTTP client, as for every dialect (retry's
// package doc). These tests pin that the swap kept the behavior: a 429
// or a 529 is retried, the budget ends, and a 400 is never retried.

package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"

	"github.com/go-steer/core-models/callctx"
)

// rejectThenServe answers the first reject requests with status, then
// serves the streaming fixture. offlineModel's policy sleeps instantly.
func rejectThenServe(t *testing.T, status, reject int) (*model, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(atomic.AddInt32(&n, 1)) <= reject {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(messagesSSEFixture))
	}))
	t.Cleanup(srv.Close)

	return offlineModel(t, srv.URL, nil, "claude-test", BuiltinTools{}), &n
}

func generate(t *testing.T, l *model) (texts []string, errs []error) {
	t.Helper()
	req := &llm.Request{
		Contents: []*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "hello"}},
		}},
	}
	for resp, err := range l.GenerateContent(context.Background(), req, false) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts, errs
}

// A 429 does not kill the call: the transport retries it. If this ever
// fails, the adapter's SDK retries are off and nothing replaced them.
func TestTheTransportRetriesARateLimit(t *testing.T) {
	l, n := rejectThenServe(t, http.StatusTooManyRequests, 1)

	texts, errs := generate(t, l)

	if len(errs) != 0 {
		t.Fatalf("429 surfaced to the caller: %v — nothing retried it", errs)
	}
	if got := atomic.LoadInt32(n); got != 2 {
		t.Errorf("server saw %d requests, want 2 (the rejected one and the retry)", got)
	}
	if len(texts) == 0 || !strings.Contains(strings.Join(texts, ""), "Hello world") {
		t.Errorf("texts = %v, want the fixture content after the retry", texts)
	}
}

// 529 overloaded is Anthropic's "come back in a moment"; the SDK retried
// it, so the transport must too.
func TestTheTransportRetriesOverloaded(t *testing.T) {
	l, n := rejectThenServe(t, 529, 1)

	if _, errs := generate(t, l); len(errs) != 0 {
		t.Fatalf("529 surfaced to the caller: %v", errs)
	}
	if got := atomic.LoadInt32(n); got != 2 {
		t.Errorf("server saw %d requests, want 2", got)
	}
}

// And the bound: the transport gives up after its configured retries
// rather than hammering. Three rejections exhaust the default two
// retries, so the error reaches the caller — and only three requests
// were made, which is what the SDK's retries being off guarantees: the
// two layers stacked would make nine.
func TestTheTransportStopsRetryingAndSurfacesTheError(t *testing.T) {
	l, n := rejectThenServe(t, http.StatusTooManyRequests, 3)

	if _, errs := generate(t, l); len(errs) == 0 {
		t.Fatal("want the 429 to surface once the retries are exhausted")
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Errorf("server saw %d requests, want 3 (initial + two retries)", got)
	}
}

// core-agent #1247 retries Vertex's bare 400 once a session has been
// served. This adapter deliberately does not: an Anthropic 400 is an
// invalid_request_error whose message names what was invalid, and no
// transient 400 has been observed from it. A marked record on ctx
// must leave a 400 exactly as it was — one request, error surfaced.
func TestGenerateContent_Bare400AfterSuccessIsNotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Request contains an invalid argument."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(messagesSSEFixture))
	}))
	t.Cleanup(srv.Close)
	l := offlineModel(t, srv.URL, nil, "claude-test", BuiltinTools{})
	rec := callctx.NewPriorSuccess()
	rec.Mark()
	ctx := callctx.WithPriorSuccess(context.Background(), rec)
	req := &llm.Request{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello"}}}}}

	var errs []error
	for _, err := range l.GenerateContent(ctx, req, false) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) != 1 {
		t.Fatalf("errs = %v, want the 400 surfaced once", errs)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("server saw %d requests, want 1 — nothing may retry an Anthropic 400", got)
	}
}

// midStreamErrorSSE starts a response and then fails it, the way the API
// reports overload partway through generation: a 200 and an error event.
const midStreamErrorSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_mid","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`

// TestMidStreamErrorReportsItsTypesStatus: an error event after the
// stream started carries a 200 from the response, which would read as
// success to any status check. The error reports the status its type
// stands for, and says it came mid-stream, which nothing retries.
func TestMidStreamErrorReportsItsTypesStatus(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(midStreamErrorSSE))
	}))
	t.Cleanup(srv.Close)
	_, errs := generate(t, offlineModel(t, srv.URL, nil, "claude-test", BuiltinTools{}))
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one", errs)
	}
	var apiErr *APIError
	if !errors.As(errs[0], &apiErr) {
		t.Fatalf("error %v is not an *APIError", errs[0])
	}
	if apiErr.HTTPStatus() != 529 || !apiErr.MidStream() {
		t.Errorf("HTTPStatus %d, MidStream %v; want 529, true", apiErr.HTTPStatus(), apiErr.MidStream())
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("server saw %d requests, want 1: a started stream is not replayed", got)
	}
}

// An error before the stream keeps its real status and is not mid-stream.
func TestUpFrontErrorKeepsItsStatus(t *testing.T) {
	l, _ := rejectThenServe(t, http.StatusTooManyRequests, 5)
	_, errs := generate(t, l)
	var apiErr *APIError
	if len(errs) != 1 || !errors.As(errs[0], &apiErr) {
		t.Fatalf("errs = %v, want one *APIError", errs)
	}
	if apiErr.HTTPStatus() != http.StatusTooManyRequests || apiErr.MidStream() {
		t.Errorf("HTTPStatus %d, MidStream %v; want 429, false", apiErr.HTTPStatus(), apiErr.MidStream())
	}
}
