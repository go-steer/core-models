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

package gemini_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/go-steer/core-models/dialect/gemini"
)

// TestAStreamCutBeforeItsFinishEndsInErrIncompleteStream: a stream that
// carries content and then ends, with no error, before any chunk gives a
// finish reason ends in ErrIncompleteStream, and the aggregate an agent
// loop would record as the whole answer is not yielded. A stream that
// finishes, and a cut unary answer, are unchanged.
func TestAStreamCutBeforeItsFinishEndsInErrIncompleteStream(t *testing.T) {
	const part = `{"candidates":[{"content":{"role":"model","parts":[{"text":"the pod is "}]}}]}`
	const done = `{"candidates":[{"content":{"role":"model","parts":[{"text":"crashlooping"}]},"finishReason":"STOP"}]}`

	t.Run("cut", func(t *testing.T) {
		s := &server{steps: []func(_ http.ResponseWriter, _ *http.Request){sse(part)}}
		m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
		out, err := collect(t, m, context.Background(), userReq("why?"), true)
		if !errors.Is(err, gemini.ErrIncompleteStream) {
			t.Fatalf("err %v, want ErrIncompleteStream", err)
		}
		if !gemini.IsTransient(err) {
			t.Error("IsTransient refused ErrIncompleteStream")
		}
		for _, r := range out {
			if r != nil && !r.Partial {
				t.Errorf("a complete answer was yielded from a cut stream: %+v", r)
			}
		}
	})
	t.Run("finished", func(t *testing.T) {
		s := &server{steps: []func(_ http.ResponseWriter, _ *http.Request){sse(part, done)}}
		m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
		out, err := collect(t, m, context.Background(), userReq("why?"), true)
		if err != nil {
			t.Fatal(err)
		}
		var final int
		for _, r := range out {
			if r != nil && !r.Partial {
				final++
			}
		}
		if final != 1 {
			t.Errorf("%d complete answers, want 1", final)
		}
	})
	t.Run("unary", func(t *testing.T) {
		s := &server{steps: []func(_ http.ResponseWriter, _ *http.Request){jsonReply(200, part)}}
		m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
		if _, err := collect(t, m, context.Background(), userReq("why?"), false); errors.Is(err, gemini.ErrIncompleteStream) {
			t.Error("a unary answer was judged as a stream")
		}
	})
}
