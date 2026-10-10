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

// Originally derived from google.golang.org/adk/v2@v2.5.0:model/gemini/gemini.go and internal/llminternal/converters/converters.go

package gemini

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"runtime"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/usage"
)

// baseModel is a Gemini model over genai, speaking core-models' llm
// types: the part of ADK's model/gemini every product used, without
// ADK. Both mast and core-agent built their adapter on
// adkgemini.NewModel; the core module cannot import ADK (decision D2),
// so this is that model ported.
//
// Three deliberate differences from ADK's:
//
//   - A response with no candidates is an empty response, never an
//     error. ADK's non-streaming path returned fmt.Errorf("empty
//     response") there while its streaming path yielded an empty
//     response, and both products string-matched the error to tell the
//     two apart. One shape means the empty-response wrapper (wrap.go)
//     handles both.
//   - Every response carrying usage gets the usage.Detail sidecar here,
//     where the genai response's ResponseID is still in hand.
//   - Each call's retry record (package retry) is stamped on the
//     complete response.
//
// Like ADK v2 (and unlike v1), a request with no contents is sent as
// it is rather than given a placeholder user turn.
type baseModel struct {
	client      *genai.Client
	name        string
	backendName string
	region      string
}

func (m *baseModel) Name() string { return m.name }

func (m *baseModel) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	if req == nil {
		req = &llm.Request{}
	}
	contents := withUserTail(req.Contents)
	cfg := withHeaders(req.Config)
	name := req.Model
	if name == "" {
		name = m.name
	}
	return func(yield func(*llm.Response, error) bool) {
		ctx, rec := retry.WithRecord(ctx)
		if !stream {
			resp, err := m.client.Models.GenerateContent(ctx, name, contents, cfg)
			if err != nil {
				yield(nil, fmt.Errorf("gemini: %w", err))
				return
			}
			r := m.convert(resp)
			r.TurnComplete = true
			stampRetry(r, rec)
			yield(r, nil)
			return
		}
		agg := &aggregator{}
		var lastID string
		for resp, err := range m.client.Models.GenerateContentStream(ctx, name, contents, cfg) {
			if err != nil {
				yield(nil, fmt.Errorf("gemini: %w", err))
				return
			}
			if resp.ResponseID != "" {
				lastID = resp.ResponseID
			}
			r := m.convert(resp)
			if len(resp.Candidates) > 0 && resp.Candidates[0] != nil {
				r.TurnComplete = resp.Candidates[0].FinishReason != ""
			}
			agg.add(r)
			if !yield(r, nil) {
				return
			}
		}
		if final := agg.close(); final != nil {
			m.describe(final, lastID)
			final.TurnComplete = true
			stampRetry(final, rec)
			yield(final, nil)
		}
	}
}

// withUserTail appends a user turn when the history ends on another
// role, so the model has something to answer. Gemini rejects a request
// whose last turn is the model's.
func withUserTail(contents []*genai.Content) []*genai.Content {
	if len(contents) == 0 {
		return contents
	}
	if last := contents[len(contents)-1]; last == nil || last.Role == genai.RoleUser {
		return contents
	}
	out := make([]*genai.Content, len(contents), len(contents)+1)
	copy(out, contents)
	return append(out, genai.NewContentFromText("Continue processing previous requests as instructed. Exit or provide a summary if no more outputs are needed.", genai.RoleUser))
}

// clientHeader identifies core-models to Google in x-goog-api-client
// and user-agent, alongside the value genai sets itself (mergeHeaders
// joins the two).
var clientHeader = "go-steer-core-models gl-go/" + strings.TrimPrefix(runtime.Version(), "go")

// withHeaders returns a copy of cfg carrying the identification
// headers. A copy, because cfg belongs to the caller.
func withHeaders(cfg *genai.GenerateContentConfig) *genai.GenerateContentConfig {
	var c genai.GenerateContentConfig
	if cfg != nil {
		c = *cfg
	}
	var ho genai.HTTPOptions
	if c.HTTPOptions != nil {
		ho = *c.HTTPOptions
	}
	h := make(http.Header, len(ho.Headers)+2)
	for k, v := range ho.Headers {
		h[k] = append([]string(nil), v...)
	}
	h.Set("x-goog-api-client", clientHeader)
	h.Set("user-agent", clientHeader)
	ho.Headers = h
	c.HTTPOptions = &ho
	return &c
}

// mergeHeaders joins repeated identification headers into one value,
// so genai's own and core-models' both reach Google.
type mergeHeaders struct{ base http.RoundTripper }

func (h mergeHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	merged := false
	for _, name := range []string{"x-goog-api-client", "user-agent"} {
		if v := req.Header.Values(name); len(v) > 1 {
			if !merged {
				req = req.Clone(req.Context())
				merged = true
			}
			req.Header.Set(name, strings.Join(v, " "))
		}
	}
	return h.base.RoundTrip(req)
}

// convert maps one genai response onto an llm.Response: the first
// candidate's content and metadata, a block or a non-STOP finish as
// the error fields, and a candidate-less chunk (Vertex sends usage-only
// heartbeats while a Gemini 3 stream warms up) as an empty response.
func (m *baseModel) convert(res *genai.GenerateContentResponse) *llm.Response {
	r := &llm.Response{UsageMetadata: res.UsageMetadata, ModelVersion: res.ModelVersion}
	switch {
	case len(res.Candidates) > 0 && res.Candidates[0] != nil:
		c := res.Candidates[0]
		r.GroundingMetadata = c.GroundingMetadata
		r.FinishReason = c.FinishReason
		r.CitationMetadata = c.CitationMetadata
		r.AvgLogprobs = c.AvgLogprobs
		r.LogprobsResult = c.LogprobsResult
		if (c.Content != nil && len(c.Content.Parts) > 0) || c.FinishReason == genai.FinishReasonStop {
			r.Content = c.Content
		} else {
			r.ErrorCode = string(c.FinishReason)
			r.ErrorMessage = c.FinishMessage
		}
	case res.PromptFeedback != nil:
		r.ErrorCode = string(res.PromptFeedback.BlockReason)
		r.ErrorMessage = res.PromptFeedback.BlockReasonMessage
	default:
		r.Content = &genai.Content{Parts: []*genai.Part{}, Role: genai.RoleModel}
	}
	m.describe(r, res.ResponseID)
	return r
}

// describe attaches the usage sidecar to a response that carries usage.
//
// Not reported is not zero (AGENTS.md rule 11), and genai cannot tell
// the two apart: its counts are plain int32s, decoded with omitempty,
// and the raw body is gone by the time a response reaches here. The
// three buckets the sidecar takes from Gemini — cachedContentTokenCount,
// thoughtsTokenCount, toolUsePromptTokenCount — are optional in the API
// and omitted when they do not apply (no cache hit, no thinking, no
// tool-use prompt), so a 0 is read as "not reported" and left nil.
// The cost is that a reported zero also reads as nil, which prices the
// same and claims nothing.
//
// CacheWriteTokens stays nil on every Gemini response: explicit context
// caches bill storage per hour, so there is no written-token count, and
// nil says so where a zero would claim a cache was warmed for free.
func (m *baseModel) describe(r *llm.Response, responseID string) {
	u := r.UsageMetadata
	if u == nil {
		return
	}
	usage.Attach(r, &usage.Detail{
		CacheReadTokens:   reported(u.CachedContentTokenCount),
		ReasoningTokens:   reported(u.ThoughtsTokenCount),
		ToolUseTokens:     reported(u.ToolUsePromptTokenCount),
		ServedModel:       r.ModelVersion,
		ProviderRequestID: responseID,
		Backend:           m.backendName,
		Region:            m.region,
	})
}

// reported is n as a count, or nil for the 0 an omitted field decodes
// to.
func reported(n int32) *int64 {
	if n == 0 {
		return nil
	}
	return usage.Int64(int64(n))
}

func stampRetry(r *llm.Response, rec *retry.Record) {
	snap := rec.Snapshot()
	if snap == (retry.Snapshot{}) {
		return
	}
	if r.CustomMetadata == nil {
		r.CustomMetadata = map[string]any{}
	}
	r.CustomMetadata[retry.MetadataKey] = snap
}
