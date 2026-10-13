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

// Package openairesp is the openai-responses dialect: POST
// {base_url}/responses, OpenAI's Responses API (docs/design.md §7.2).
// The built-in openai profile speaks it, and any OpenAI-shaped server
// that serves /v1/responses can opt in with `dialect: openai-responses`
// (vLLM does).
//
// Like openai-chat it is written on net/http and its own wire types,
// not an SDK: pointer-typed wire structs keep a count the server never
// sent distinct from a zero it did, servers differ in what they return
// (vLLM puts reasoning in reasoning_text content, OpenAI in summaries
// and encrypted content), and no new module enters the graph.
//
// Requests are stateless: store false, with the reasoning's encrypted
// content requested and carried forward through
// genai.Part.ThoughtSignature, so a tool loop keeps its reasoning
// without a server-side conversation.
//
// Retries happen in the HTTP client (package retry), so every attempt
// gets a fresh credential and a provider's Retry-After is honored.
package openairesp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/usage"
)

// Options configures a Client. A coremodels.Open caller never builds
// one by hand; it comes from a resolved profile.
type Options struct {
	// BaseURL is the API root; requests go to BaseURL + "/responses".
	BaseURL string
	// Credential authenticates every request as a bearer token. Nil
	// sends none.
	Credential *auth.Credential
	// HTTPClient supplies the base transport and timeout. Nil means a
	// client with http.DefaultTransport.
	HTTPClient *http.Client
	// Retry is the HTTP-layer retry policy. Nil means retry.Default().
	Retry *retry.Policy

	// Backend and Region are stamped into every usage.Detail.
	Backend string
	Region  string

	// ResponseSchema: the server enforces text.format json_schema. A
	// request that needs one is refused when false.
	ResponseSchema bool
	// CachedTokensUnreliable: record cached tokens as not reported even
	// when the server sends them.
	CachedTokensUnreliable bool
	// NoForcedToolChoice: the model rejects "required" and named tool
	// choices; send "auto" instead and mark the response.
	NoForcedToolChoice bool
	// ExtraBody is merged into every request body: vendor fields the
	// dialect does not model. It must not repeat a field the adapter sets.
	ExtraBody map[string]any
}

// ModelOptions are the per-model refinements of Options.
type ModelOptions struct {
	ResponseSchema     *bool
	NoForcedToolChoice *bool
	// ExtraBody, when non-nil, replaces the Client's.
	ExtraBody map[string]any
}

// Client is a connection to one server. Safe for concurrent use.
type Client struct {
	opts     Options
	endpoint string
	http     *http.Client
}

// New returns a Client for opts.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("openai-responses: BaseURL is required")
	}
	base := opts.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if opts.Credential != nil {
		rt = opts.Credential.BearerTransport(rt)
	}
	policy := retry.Default()
	if opts.Retry != nil {
		policy = *opts.Retry
	}
	hc := *base
	hc.Transport = policy.Transport(rt)
	return &Client{
		opts:     opts,
		endpoint: strings.TrimRight(opts.BaseURL, "/") + "/responses",
		http:     &hc,
	}, nil
}

// Model returns the model id on this server as an llm.LLM.
func (c *Client) Model(id string) llm.LLM { return c.ModelWith(id, ModelOptions{}) }

// ModelWith returns the model id with mo laid over the Client's
// options.
func (c *Client) ModelWith(id string, mo ModelOptions) llm.LLM {
	o := c.opts
	if mo.ResponseSchema != nil {
		o.ResponseSchema = *mo.ResponseSchema
	}
	if mo.NoForcedToolChoice != nil {
		o.NoForcedToolChoice = *mo.NoForcedToolChoice
	}
	if mo.ExtraBody != nil {
		o.ExtraBody = mo.ExtraBody
	}
	return &model{Client: c, id: id, opts: o}
}

type model struct {
	*Client
	id   string
	opts Options
}

func (m *model) Name() string { return m.id }

// APIError is a non-2xx answer, an error event inside a stream, or a
// response whose status is failed. StatusCode is 0 for the last two.
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("openai-responses: ")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	} else {
		b.WriteString("response failed")
	}
	switch {
	case e.Type != "":
		fmt.Fprintf(&b, " (%s)", e.Type)
	case e.Code != "":
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// HTTPStatus reports the status, for retry classifiers above this
// package. Zero for a failure reported inside a 200 response.
func (e *APIError) HTTPStatus() int { return e.StatusCode }

// Metadata keys this dialect writes beside usage.MetadataKey. The
// values match openaichat's, so a product reads one key for both.
const (
	// RefusalKey marks a response whose text is the model's refusal.
	RefusalKey = "core_models.refusal"
	// FinishReasonKey holds a finish reason with no genai equivalent.
	FinishReasonKey = "core_models.finish_reason"
	// ToolChoiceKey marks a response whose request asked for a forced
	// tool call the model cannot take, and was sent with "auto" instead.
	ToolChoiceKey = "core_models.tool_choice_downgraded"
)

func (m *model) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		if req == nil {
			req = &llm.Request{}
		}
		body, notes, err := m.buildRequest(req, stream)
		if err != nil {
			yield(nil, err)
			return
		}
		ctx, rec := retry.WithRecord(ctx)
		if !stream {
			// A non-streaming answer arrives whole: the wait for headers
			// is its generation time, bounded by UnaryHeaderTimeout.
			ctx = retry.MarkUnary(ctx)
		}
		resp, err := m.send(ctx, body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !stream {
			var r response
			if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
				yield(nil, fmt.Errorf("openai-responses: decode response: %w", err))
				return
			}
			final, err := m.finish(&r)
			if err != nil {
				yield(nil, err)
				return
			}
			stampRetry(final, rec)
			stampNotes(final, notes)
			yield(final, nil)
			return
		}
		for r, err := range m.decodeStream(resp.Body) {
			if err != nil {
				yield(nil, err)
				return
			}
			if !r.Partial {
				stampRetry(r, rec)
				stampNotes(r, notes)
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

func (m *model) send(ctx context.Context, body *responsesRequest) (*http.Response, error) {
	raw, err := m.encode(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("openai-responses: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if body.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		return nil, apiError(resp)
	}
	return resp, nil
}

// encode marshals the request, with ExtraBody's fields added. A field
// the request already carries is an error: ExtraBody adds vendor
// switches, it never overrides what the adapter decided.
func (m *model) encode(body *responsesRequest) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-responses: encode request: %w", err)
	}
	if len(m.opts.ExtraBody) == 0 {
		return raw, nil
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, fmt.Errorf("openai-responses: encode request: %w", err)
	}
	for k, v := range m.opts.ExtraBody {
		if _, set := merged[k]; set {
			return nil, fmt.Errorf("openai-responses: extra_body sets %q, which this request already sets", k)
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{StatusCode: resp.StatusCode}
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env)
	switch {
	case env.Error != nil:
		e.Message, e.Type, e.Code = env.Error.Message, env.Error.Type, codeString(env.Error.Code)
	case env.Message != "":
		e.Message = env.Message
	case env.Detail != "":
		e.Message = env.Detail
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	return e
}

func codeString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// decodeStream yields a partial Response per text or reasoning delta,
// then the complete Response built from the response.completed (or
// response.incomplete) event, which carries the whole response — the
// same decoding as a non-streamed call, so the two cannot disagree. The
// complete Response is the call's one TurnComplete (llm.Response).
func (m *model) decodeStream(r io.Reader) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		// An event's data may span several data: lines, joined with
		// newlines and ended by a blank line (the SSE format).
		var buf []string
		next := func() (string, bool) {
			for sc.Scan() {
				line := sc.Text()
				if line == "" {
					if len(buf) > 0 {
						data := strings.Join(buf, "\n")
						buf = buf[:0]
						return data, true
					}
					continue
				}
				if d, ok := strings.CutPrefix(line, "data:"); ok {
					buf = append(buf, strings.TrimPrefix(d, " "))
				}
				// event:, id:, retry: and comments carry nothing the
				// data does not.
			}
			if len(buf) > 0 {
				data := strings.Join(buf, "\n")
				buf = buf[:0]
				return data, true
			}
			return "", false
		}
		for {
			data, ok := next()
			if !ok {
				break
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}
			var ev streamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				yield(nil, fmt.Errorf("openai-responses: decode stream event: %w", err))
				return
			}
			switch ev.Type {
			case "response.output_text.delta":
				if ev.Delta != "" && !yield(partial(&genai.Part{Text: ev.Delta}, m.id), nil) {
					return
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if ev.Delta != "" && !yield(partial(&genai.Part{Text: ev.Delta, Thought: true}, m.id), nil) {
					return
				}
			case "response.completed", "response.incomplete", "response.failed":
				if ev.Response == nil {
					yield(nil, fmt.Errorf("openai-responses: %s event carries no response", ev.Type))
					return
				}
				final, err := m.finish(ev.Response)
				if err != nil {
					yield(nil, err)
					return
				}
				final.TurnComplete = true
				yield(final, nil)
				return
			case "error":
				yield(nil, &APIError{Code: codeString(ev.Code), Message: ev.Message})
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(nil, fmt.Errorf("openai-responses: read stream: %w", err))
			return
		}
		yield(nil, errors.New("openai-responses: the stream ended without a completed response"))
	}
}

func partial(p *genai.Part, modelID string) *llm.Response {
	return &llm.Response{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{p}},
		Partial:      true,
		ModelVersion: modelID,
	}
}

// finish builds the complete Response from a whole response: its output
// items as parts, in order; genai usage beside a usage.Detail; the
// finish reason from its status.
func (m *model) finish(r *response) (*llm.Response, error) {
	if r.Status == "failed" || r.Status == "cancelled" {
		e := &APIError{Message: "status " + r.Status}
		if r.Error != nil {
			e.Message, e.Type, e.Code = r.Error.Message, r.Error.Type, codeString(r.Error.Code)
		}
		return nil, e
	}
	content := &genai.Content{Role: genai.RoleModel}
	refused := false
	for _, it := range r.Output {
		switch it.Type {
		case "reasoning":
			p := &genai.Part{Thought: true, Text: reasoningText(it)}
			if it.EncryptedContent != nil && *it.EncryptedContent != "" {
				p.ThoughtSignature = encodeSignature(it.ID, *it.EncryptedContent)
			}
			// A reasoning item with neither text nor encrypted content has
			// nothing to show and nothing to replay.
			if p.Text != "" || p.ThoughtSignature != nil {
				content.Parts = append(content.Parts, p)
			}
		case "message":
			for _, c := range it.Content {
				switch {
				case c.Type == "refusal" && c.Refusal != nil:
					refused = true
					content.Parts = append(content.Parts, &genai.Part{Text: *c.Refusal})
				case c.Text != "":
					content.Parts = append(content.Parts, &genai.Part{Text: c.Text})
				}
			}
		case "function_call":
			args := map[string]any{}
			if raw := strings.TrimSpace(it.Arguments); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					return nil, fmt.Errorf("openai-responses: the model called %q with arguments that are not a JSON object: %.200q", it.Name, raw)
				}
			}
			id := it.CallID
			if id == "" {
				id = newCallID()
			}
			content.Parts = append(content.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: it.Name, Args: args}})
		}
	}

	finish, raw := finishReason(r)
	resp := &llm.Response{
		Content: content,
		// The requested id, not the echoed one, as in openai-chat: the
		// echo is kept in usage.Detail.ServedModel.
		ModelVersion: m.id,
		FinishReason: finish,
	}
	if refused {
		resp.CustomMetadata = map[string]any{RefusalKey: true}
	}
	if finish == genai.FinishReasonOther && raw != "" {
		if resp.CustomMetadata == nil {
			resp.CustomMetadata = map[string]any{}
		}
		resp.CustomMetadata[FinishReasonKey] = raw
	}
	resp.UsageMetadata, resp.CustomMetadata = m.mapUsage(r.Usage, r.ID, r.Model, resp.CustomMetadata)
	return resp, nil
}

// reasoningText is what a reasoning item shows: OpenAI's summaries, or
// vLLM's reasoning_text content.
func reasoningText(it outputItem) string {
	var parts []string
	for _, s := range it.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	for _, c := range it.Content {
		if c.Type == "reasoning_text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// finishReason maps a response's status, and an incomplete response's
// reason, onto genai's. The raw reason is returned for one with no
// equivalent.
func finishReason(r *response) (genai.FinishReason, string) {
	switch r.Status {
	case "completed":
		return genai.FinishReasonStop, ""
	case "incomplete":
		reason := ""
		if r.IncompleteDetails != nil {
			reason = r.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			return genai.FinishReasonMaxTokens, ""
		case "content_filter":
			return genai.FinishReasonSafety, ""
		}
		return genai.FinishReasonOther, "incomplete: " + reason
	case "":
		return genai.FinishReasonUnspecified, ""
	}
	return genai.FinishReasonOther, r.Status
}

// mapUsage maps the server's usage onto genai's struct and a Detail. A
// field the server did not send is absent from both — never a zero.
// Reasoning tokens are counted inside output_tokens, as in Chat
// Completions, so they are subtracted from candidates where reported.
func (m *model) mapUsage(u *wireUsage, id, served string, md map[string]any) (*genai.GenerateContentResponseUsageMetadata, map[string]any) {
	d := &usage.Detail{
		ServedModel:       served,
		ProviderRequestID: id,
		Backend:           m.opts.Backend,
		Region:            m.opts.Region,
	}
	var um *genai.GenerateContentResponseUsageMetadata
	if u != nil {
		um = &genai.GenerateContentResponseUsageMetadata{}
		var reasoning int64
		if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens != nil {
			reasoning = *u.OutputTokensDetails.ReasoningTokens
			d.ReasoningTokens = ptr(reasoning)
			um.ThoughtsTokenCount = clamp32(reasoning)
		}
		if u.InputTokens != nil {
			um.PromptTokenCount = clamp32(*u.InputTokens)
		}
		if u.OutputTokens != nil {
			um.CandidatesTokenCount = clamp32(*u.OutputTokens - reasoning)
		}
		switch {
		case u.TotalTokens != nil:
			um.TotalTokenCount = clamp32(*u.TotalTokens)
		case u.InputTokens != nil && u.OutputTokens != nil:
			um.TotalTokenCount = clamp32(*u.InputTokens + *u.OutputTokens)
		}
		if !m.opts.CachedTokensUnreliable && u.InputTokensDetails != nil {
			if c := u.InputTokensDetails.CachedTokens; c != nil {
				d.CacheReadTokens = ptr(*c)
				um.CachedContentTokenCount = clamp32(*c)
			}
			// genai's struct has no cache-write bucket; the Detail does.
			if w := u.InputTokensDetails.CacheWriteTokens; w != nil {
				d.CacheWriteTokens = ptr(*w)
			}
		}
	}
	resp := &llm.Response{CustomMetadata: md}
	usage.Attach(resp, d)
	return um, resp.CustomMetadata
}

// clamp32 narrows a count to genai's int32 fields, saturating rather
// than wrapping. Detail keeps the full int64.
func clamp32(n int64) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < 0:
		return 0
	}
	return int32(n)
}

func stampNotes(r *llm.Response, notes map[string]any) {
	if len(notes) == 0 {
		return
	}
	if r.CustomMetadata == nil {
		r.CustomMetadata = map[string]any{}
	}
	for k, v := range notes {
		r.CustomMetadata[k] = v
	}
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
