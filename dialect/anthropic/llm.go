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

// Originally derived from go-steer/core-agent@15791a04de1575687424602e5f57395ae23fcb73:pkg/models/anthropic/llm.go
// Originally derived from go-steer/mast@6568618f531368b8fa4894645df292b1e2aca09f:internal/providers/anthropic/llm.go

package anthropic

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/usage"
)

// maxPauseTurnContinuations bounds how many times GenerateContent
// re-issues a request after a pause_turn stop. pause_turn means the
// server-side tool loop (e.g. web_search) ran long and the API wants
// the turn resubmitted so it can keep working; a well-behaved turn
// finishes within a couple of continuations, so the cap only exists to
// stop a pathological server from spinning us forever.
const maxPauseTurnContinuations = 4

// model implements llm.LLM for one Claude model id.
type model struct {
	client   anthropic.Client
	modelID  string
	cache    CacheOptions
	builtins BuiltinTools
	backend  string
	region   string
}

// Name reports the model id.
func (m *model) Name() string { return m.modelID }

// GenerateContent implements llm.LLM. In streaming mode it yields
// partial-text Responses (Partial set) followed by exactly one terminal
// Response (TurnComplete set) carrying the full content, usage and
// mapped FinishReason; with stream false only the terminal one is
// yielded. The HTTP transport streams SSE either way — that is the shape
// the pause_turn continuation and the close discipline below are built
// around — and the flag only controls what the caller sees, so a
// non-streaming caller is not handed one event per text fragment (mast
// and core-agent #533). Errors are yielded inline and stop the
// iteration.
func (m *model) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		// One model serves an agent's loop and its one-shot side calls
		// alike, so the opt-outs are read per request rather than baked
		// in at construction (core-agent).
		cache := m.cache
		if callctx.PromptCacheSuppressed(ctx) {
			cache = CacheOptions{}
		}
		builtins := m.builtins
		if callctx.BuiltinsSuppressed(ctx) {
			builtins = BuiltinTools{}
		}
		modelID := req.Model
		if modelID == "" {
			modelID = m.modelID
		}
		params, err := buildParams(modelID, req.Contents, req.Config, cache, builtins)
		if err != nil {
			yield(nil, fmt.Errorf("anthropic: build request: %w", err))
			return
		}

		// The turn may span several requests: a long server-side tool
		// run (BuiltinTools.WebSearch) ends its request with stop_reason
		// pause_turn, and the API expects the paused assistant turn
		// replayed verbatim so it can resume. Content parts and usage
		// accumulate across all requests of the turn; exactly one
		// terminal response is yielded at the end.
		var parts []*genai.Part
		var u anthropic.Usage

		for continuation := 0; ; continuation++ {
			sse := m.client.Messages.NewStreaming(ctx, params)
			final := anthropic.Message{}

			// Drain inside a closure so the deferred Close releases this
			// request's HTTP connection on every exit path — an early
			// consumer stop, an accumulate or stream error, the
			// pause_turn continue, and the terminal return (mast and
			// core-agent #487). Returns false when GenerateContent must
			// stop (the consumer said stop, or an error was yielded).
			drained := func() bool {
				defer func() { _ = sse.Close() }()
				for sse.Next() {
					ev := sse.Current()
					if err := final.Accumulate(ev); err != nil {
						yield(nil, fmt.Errorf("anthropic: accumulate: %w", err))
						return false
					}
					if delta, ok := textDelta(ev); ok && stream {
						partial := &llm.Response{
							Content: &genai.Content{
								Role:  genai.RoleModel,
								Parts: []*genai.Part{{Text: delta}},
							},
							Partial: true,
						}
						if !yield(partial, nil) {
							return false
						}
					}
				}
				if err := sse.Err(); err != nil {
					yield(nil, wrapError(err))
					return false
				}
				return true
			}()
			if !drained {
				return
			}

			parts = append(parts, contentPartsFromMessage(&final)...)
			addUsage(&u, final.Usage)

			if final.StopReason == anthropic.StopReasonPauseTurn {
				if continuation < maxPauseTurnContinuations {
					// Replay the paused assistant message exactly as
					// received (ToParam preserves the server_tool_use and
					// web_search_tool_result blocks the API needs) and
					// re-issue; the server resumes where it left off.
					params.Messages = append(params.Messages, final.ToParam())
					// The replayed turn lands after every marker
					// buildParams placed, so re-place them: otherwise
					// its server-tool blocks are re-sent at full rate and
					// the tail marker drifts out of the next request's
					// lookback window (core-agent).
					reapplyCacheBreakpoints(&params, cache)
					continue
				}
				// Cap reached: surface what we have instead of spinning.
				// mapStopReason turns pause_turn into FinishReasonOther,
				// an honest "stopped for a non-standard reason".
				log.Printf("anthropic: pause_turn continuation cap (%d) reached on model %s; yielding accumulated content", maxPauseTurnContinuations, params.Model)
			}

			resp := &llm.Response{
				Content: &genai.Content{Role: genai.RoleModel, Parts: parts},
				// ModelVersion names the model these tokens were billed
				// against, so a consumer summing usage across a multi-tier
				// agent can price each tier at its own rate (mast #210,
				// core-agent #756). See responseModel.
				ModelVersion:  responseModel(&final, params.Model),
				UsageMetadata: usageMetadata(u),
				FinishReason:  mapStopReason(final.StopReason),
				TurnComplete:  true,
			}
			// The Detail carries the cache-write counts genai's usage
			// shape has nowhere for, so a meter bills them at the
			// cache-write rate instead of folding them into fresh input.
			// Token counts are the turn's, summed across every request of
			// a pause_turn loop; ServedModel and the request id name the
			// last request, the one a support ticket would be about.
			// ServedModel is the unedited echo: ModelVersion may have
			// substituted the requested id for a resource path.
			usage.Attach(resp, usageDetail(u, final.Model, final.ID, m.backend, m.region))
			yield(resp, nil)
			return
		}
	}
}

// responseModel reports which model served the turn, for
// Response.ModelVersion.
//
// The server's echo wins over the model asked for. They normally agree,
// but an alias resolves server-side to a dated snapshot and the
// snapshot is the one billed.
//
// Two echoes are rejected in favor of the requested id, because what
// the field feeds is a pricing key:
//
//   - Empty. A well-formed Message never is, but a stream that died
//     before message_start leaves one.
//   - A resource path. Price tables key on bare model ids and fall back
//     to the longest prefix, so "claude-opus-4-5@20251101" resolves but
//     "projects/…/models/claude-…" matches nothing and prices the turn
//     at zero.
func responseModel(final *anthropic.Message, requested anthropic.Model) string {
	if final != nil && final.Model != "" && !strings.Contains(final.Model, "/") {
		return final.Model
	}
	return requested
}

// textDelta extracts incremental assistant text from a stream event.
// Returns ("", false) for everything other than a content_block_delta
// carrying a TextDelta — tool-use input deltas, message-stop events
// and the rest are accumulated by Message.Accumulate but not surfaced
// as partials.
func textDelta(ev anthropic.MessageStreamEventUnion) (string, bool) {
	delta, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent)
	if !ok {
		return "", false
	}
	td, ok := delta.Delta.AsAny().(anthropic.TextDelta)
	if !ok {
		return "", false
	}
	return td.Text, td.Text != ""
}

// APIError is an error the Anthropic API answered with. It wraps the
// SDK's *anthropic.Error, which errors.As still finds, and adds the
// HTTPStatus method products classify retryable failures by — the same
// method openaichat.APIError has, so one check covers every dialect.
//
// An error can arrive two ways. Before the stream starts it is an
// ordinary non-2xx response, which package retry has already retried
// if the status allowed. After it starts, the server sends an SSE error
// event on a response that answered 200 — an overloaded_error midway
// through generation, say. The transport cannot retry that: tokens may
// already have reached the caller, and replaying the request would
// deliver them twice. HTTPStatus then reports the status the error's
// type stands for (overloaded_error is 529), not the stream's 200, so a
// product's own retry policy can still tell a transient failure from a
// permanent one; MidStream says which case it was.
type APIError struct {
	err *anthropic.Error
}

func (e *APIError) Error() string { return "anthropic: " + e.err.Error() }
func (e *APIError) Unwrap() error { return e.err }

// HTTPStatus is the response's status, or for a mid-stream error the
// status its type stands for.
func (e *APIError) HTTPStatus() int {
	if e.MidStream() {
		if s, ok := errorTypeStatus[string(e.err.Type())]; ok {
			return s
		}
		return http.StatusInternalServerError
	}
	return e.err.StatusCode
}

// MidStream reports whether the error arrived as an SSE event after the
// response had started, which no transport retries.
func (e *APIError) MidStream() bool {
	return e.err.StatusCode >= 200 && e.err.StatusCode < 300
}

// errorTypeStatus is the HTTP status Anthropic documents for each error
// type, used for errors whose own response said 200.
var errorTypeStatus = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"billing_error":         http.StatusPaymentRequired,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"api_error":             http.StatusInternalServerError,
	"timeout_error":         http.StatusGatewayTimeout,
	"overloaded_error":      529,
}

// wrapError gives an SDK API error the HTTPStatus method and prefixes
// anything else.
func wrapError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return &APIError{err: apiErr}
	}
	return fmt.Errorf("anthropic: stream: %w", err)
}
