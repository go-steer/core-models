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

package coremodels_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/profile"
	"github.com/go-steer/core-models/retry"
)

// TestOpenSendsProfileHeaders: every dialect sends a profile's headers on
// every attempt, a retry included, with ${VAR}s read and a header that
// comes out empty left off.
func TestOpenSendsProfileHeaders(t *testing.T) {
	const respBody = `{"id":"resp_2","object":"response","status":"completed","model":"gpt-5-mini","output":[
 {"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],
 "usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`
	const chatBody = `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	const geminiBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4},"modelVersion":"gemini-3.8-flash"}`

	cases := []struct {
		name, extends, model, contentType, body string
		env                                     map[string]string
	}{
		{"openai-chat", "openai-compatible", "m", "application/json", chatBody, nil},
		{"openai-responses", "openai", "gpt-5-mini", "application/json", respBody, map[string]string{"OPENAI_API_KEY": "sk-test"}},
		{"anthropic", "anthropic", "claude-haiku-4-5", "text/event-stream", claudeSSE, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}},
		{"gemini", "gemini", "gemini-3.8-flash", "application/json", geminiBody, map[string]string{"GEMINI_API_KEY": "k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				first := len(seen) == 1
				mu.Unlock()
				if first {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", c.contentType)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()

			env := map[string]string{"TENANT": "acme", "SUB_KEY": "s3cret"}
			for k, v := range c.env {
				env[k] = v
			}
			o := opts(env)
			o.Retry = &retry.Policy{MaxRetries: 1, InitialBackoff: time.Millisecond, BackoffFactor: 1, MaxBackoff: time.Millisecond}
			p := profile.Profile{Name: "gw", Extends: c.extends, BaseURL: srv.URL, Headers: map[string]string{
				"x-tenant":       "${TENANT}",
				"X-Subscription": "${SUB_KEY}",
				"X-Region":       "${REGION:-eu}",
				"X-Dropped":      "${UNSET_OPTIONAL:-}",
				"anthropic-beta": "context-1m-2025-08-07",
			}}
			if c.extends == "openai-compatible" {
				p.Models = []profile.Model{{ID: "m"}}
			}
			prov, err := coremodels.Open(context.Background(), p, o)
			if err != nil {
				t.Fatal(err)
			}
			callOnce(t, prov, c.model)

			if len(seen) != 2 {
				t.Fatalf("server saw %d requests, want 2 (a 503 and its retry)", len(seen))
			}
			for i, h := range seen {
				for name, want := range map[string]string{"X-Tenant": "acme", "X-Subscription": "s3cret", "X-Region": "eu", "Anthropic-Beta": "context-1m-2025-08-07"} {
					if got := h.Values(name); len(got) != 1 || got[0] != want {
						t.Errorf("attempt %d: %s = %q, want %q", i+1, name, got, want)
					}
				}
				if _, ok := h["X-Dropped"]; ok {
					t.Errorf("attempt %d: X-Dropped sent though it resolved empty", i+1)
				}
			}
		})
	}
}
