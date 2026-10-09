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

// Package openaichat is the openai-chat dialect: POST
// {base_url}/chat/completions, the wire shape Vertex AI's partner
// models, xAI, vLLM, SGLang, Ollama, llama.cpp, NVIDIA NIM and the
// managed long tail all speak (docs/design.md §7.1).
//
// It is written on net/http and its own wire types rather than an SDK.
// These servers are OpenAI-shaped, not OpenAI: they add fields
// (reasoning_content), omit fields (usage details, tool-call ids) and
// differ on what they accept, and pointer-typed wire structs keep a
// count the server never sent distinct from a zero it did — the
// distinction usage.Detail exists for.
//
// Retries happen in the HTTP client (package retry), so every attempt
// gets a fresh credential and a provider's Retry-After is honored.
package openaichat

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
	// BaseURL is the API root; requests go to BaseURL + "/chat/completions".
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

	// ResponseSchema: the server enforces response_format json_schema.
	// A request that needs one is refused when false.
	ResponseSchema bool
	// ReasoningEcho: send prior reasoning back as reasoning_content.
	ReasoningEcho bool
	// CachedTokensUnreliable: record cached tokens as not reported even
	// when the server sends them.
	CachedTokensUnreliable bool
	// ThinkTags: the server puts reasoning inline in content as a leading
	// <think>…</think> block; split it into a reasoning part.
	ThinkTags bool
	// NoForcedToolChoice: the model rejects "required" and named tool
	// choices; send "auto" instead and mark the response.
	NoForcedToolChoice bool
}

// ModelOptions are the per-model refinements of Options: one server
// serves many models, and what it honors can differ between them.
type ModelOptions struct {
	ResponseSchema     *bool
	ReasoningEcho      *bool
	NoForcedToolChoice *bool
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
		return nil, errors.New("openai-chat: BaseURL is required")
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
		endpoint: strings.TrimRight(opts.BaseURL, "/") + "/chat/completions",
		http:     &hc,
	}, nil
}

// Model returns the model id on this server as an llm.LLM, with the
// Client's options.
func (c *Client) Model(id string) llm.LLM {
	return c.ModelWith(id, ModelOptions{})
}

// ModelWith returns the model id with mo laid over the Client's
// options.
func (c *Client) ModelWith(id string, mo ModelOptions) llm.LLM {
	o := c.opts
	if mo.ResponseSchema != nil {
		o.ResponseSchema = *mo.ResponseSchema
	}
	if mo.ReasoningEcho != nil {
		o.ReasoningEcho = *mo.ReasoningEcho
	}
	if mo.NoForcedToolChoice != nil {
		o.NoForcedToolChoice = *mo.NoForcedToolChoice
	}
	return &model{Client: c, id: id, opts: o}
}

type model struct {
	*Client
	id   string
	opts Options // the Client's, refined for this model
}

func (m *model) Name() string { return m.id }

// APIError is a non-2xx answer, or an error event inside a stream.
// StatusCode is 0 for an in-stream error.
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("openai-chat: ")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	} else {
		b.WriteString("stream error")
	}
	if e.Type != "" {
		fmt.Fprintf(&b, " (%s)", e.Type)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// HTTPStatus reports the status, for retry classifiers that sit above
// this package.
func (e *APIError) HTTPStatus() int { return e.StatusCode }

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
		resp, err := m.send(ctx, body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !stream {
			final, err := m.decodeComplete(resp.Body)
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

func (m *model) send(ctx context.Context, body *chatRequest) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-chat: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("openai-chat: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if body.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-chat: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		return nil, apiError(resp)
	}
	return resp, nil
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{StatusCode: resp.StatusCode}
	var env errorEnvelope
	// Vertex AI wraps its error object in a one-element array.
	var wrapped []errorEnvelope
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped) > 0 {
		env = wrapped[0]
	} else {
		_ = json.Unmarshal(raw, &env)
	}
	{
		switch {
		case env.Error != nil:
			e.Message, e.Type, e.Code = env.Error.Message, env.Error.Type, codeString(env.Error.Code)
		case env.Message != "":
			e.Message = env.Message
		case env.Detail != "":
			e.Message = env.Detail
		}
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

func (m *model) decodeComplete(r io.Reader) (*llm.Response, error) {
	var cr chatResponse
	if err := json.NewDecoder(r).Decode(&cr); err != nil {
		return nil, fmt.Errorf("openai-chat: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("openai-chat: the response has no choices")
	}
	ch := cr.Choices[0]
	acc := &accumulator{}
	if rs := ch.Message.reasoning(); rs != nil {
		acc.reasoning.WriteString(*rs)
	}
	if ch.Message.Content != nil {
		m.addContent(acc, *ch.Message.Content)
	}
	m.flushContent(acc)
	if ch.Message.Refusal != nil {
		acc.refusal = *ch.Message.Refusal
	}
	for i, tc := range ch.Message.ToolCalls {
		acc.addToolCall(i, tc)
	}
	return m.finish(acc, cr.ID, cr.Model, ch.FinishReason, cr.Usage)
}

// decodeStream yields a partial Response per text or reasoning delta,
// then one complete Response carrying everything, the usage and the
// finish reason — the shape ADK's runner expects in streaming mode.
func (m *model) decodeStream(r io.Reader) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		acc := &accumulator{}
		var id, served, finish string
		var u *wireUsage
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			line := sc.Text()
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue // blank separators, comments, event: lines
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			var c chunk
			if err := json.Unmarshal([]byte(data), &c); err != nil {
				yield(nil, fmt.Errorf("openai-chat: decode stream chunk: %w", err))
				return
			}
			if c.Error != nil {
				yield(nil, &APIError{Type: c.Error.Type, Code: codeString(c.Error.Code), Message: c.Error.Message})
				return
			}
			if c.ID != "" {
				id = c.ID
			}
			if c.Model != "" {
				served = c.Model
			}
			if c.Usage != nil {
				u = c.Usage
			}
			for _, ch := range c.Choices {
				if ch.Index != 0 {
					continue
				}
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					finish = *ch.FinishReason
				}
				d := ch.Delta
				if rs := d.reasoning(); rs != nil && *rs != "" {
					acc.reasoning.WriteString(*rs)
					if !yield(partial(&genai.Part{Text: *rs, Thought: true}, m.id), nil) {
						return
					}
				}
				if d.Content != nil && *d.Content != "" {
					rs, txt := m.addContent(acc, *d.Content)
					if rs != "" && !yield(partial(&genai.Part{Text: rs, Thought: true}, m.id), nil) {
						return
					}
					if txt != "" && !yield(partial(&genai.Part{Text: txt}, m.id), nil) {
						return
					}
				}
				if d.Refusal != nil {
					acc.refusal += *d.Refusal
				}
				for i, tc := range d.ToolCalls {
					idx := i
					if tc.Index != nil {
						idx = *tc.Index
					}
					acc.addToolCall(idx, tc)
				}
			}
		}
		if err := sc.Err(); err != nil {
			yield(nil, fmt.Errorf("openai-chat: read stream: %w", err))
			return
		}
		if rs, txt := m.flushContent(acc); rs != "" || txt != "" {
			for _, p := range []*genai.Part{{Text: rs, Thought: true}, {Text: txt}} {
				if p.Text != "" && !yield(partial(p, m.id), nil) {
					return
				}
			}
		}
		final, err := m.finish(acc, id, served, finish, u)
		if err != nil {
			yield(nil, err)
			return
		}
		final.TurnComplete = true
		yield(final, nil)
	}
}

func partial(p *genai.Part, modelID string) *llm.Response {
	return &llm.Response{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{p}},
		Partial:      true,
		ModelVersion: modelID,
	}
}

// accumulator collects one choice across a whole response or stream.
// Tool calls arrive as fragments keyed by index: the first carries the
// id and name, later ones extend the arguments.
type accumulator struct {
	think     thinkSplitter
	reasoning strings.Builder
	text      strings.Builder
	refusal   string
	calls     []*pendingCall
}

type pendingCall struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// addContent takes a piece of content and returns the reasoning and
// answer text it contributes, after think-tag splitting when the
// profile asks for it.
func (m *model) addContent(acc *accumulator, s string) (reasoning, answer string) {
	if !m.opts.ThinkTags {
		acc.text.WriteString(s)
		return "", s
	}
	reasoning, answer = acc.think.feed(s)
	acc.reasoning.WriteString(reasoning)
	acc.text.WriteString(answer)
	return reasoning, answer
}

// flushContent commits whatever the think splitter still holds.
func (m *model) flushContent(acc *accumulator) (reasoning, answer string) {
	if !m.opts.ThinkTags {
		return "", ""
	}
	reasoning, answer = acc.think.flush()
	acc.reasoning.WriteString(reasoning)
	acc.text.WriteString(answer)
	return reasoning, answer
}

func (a *accumulator) addToolCall(index int, tc respToolCall) {
	var pc *pendingCall
	for _, c := range a.calls {
		if c.index == index {
			pc = c
			break
		}
	}
	if pc == nil {
		pc = &pendingCall{index: index}
		a.calls = append(a.calls, pc)
	}
	if tc.ID != "" {
		pc.id = tc.ID
	}
	if tc.Function.Name != "" {
		pc.name = tc.Function.Name
	}
	pc.args.WriteString(tc.Function.Arguments)
}

// finish builds the complete Response: reasoning, text and tool calls
// as parts, genai usage beside a usage.Detail, the finish reason.
func (m *model) finish(acc *accumulator, id, served, finish string, u *wireUsage) (*llm.Response, error) {
	content := &genai.Content{Role: genai.RoleModel}
	if acc.reasoning.Len() > 0 {
		content.Parts = append(content.Parts, &genai.Part{Text: acc.reasoning.String(), Thought: true})
	}
	if acc.text.Len() > 0 {
		content.Parts = append(content.Parts, &genai.Part{Text: acc.text.String()})
	}
	if acc.refusal != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: acc.refusal})
	}
	for _, c := range acc.calls {
		args := map[string]any{}
		if raw := strings.TrimSpace(c.args.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return nil, fmt.Errorf("openai-chat: the model called %q with arguments that are not a JSON object: %.200q", c.name, raw)
			}
		}
		callID := c.id
		if callID == "" {
			callID = newCallID()
		}
		content.Parts = append(content.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: callID, Name: c.name, Args: args}})
	}

	resp := &llm.Response{
		Content: content,
		// The requested id, not the echoed one: the echo can be a weights
		// path or a publisher-qualified name no price table resolves, and
		// ModelVersion is read as a pricing key. The echo is kept in
		// usage.Detail.ServedModel.
		ModelVersion: m.id,
		FinishReason: finishReason(finish),
	}
	if acc.refusal != "" {
		resp.CustomMetadata = map[string]any{RefusalKey: true}
	}
	if resp.FinishReason == genai.FinishReasonOther && finish != "" {
		if resp.CustomMetadata == nil {
			resp.CustomMetadata = map[string]any{}
		}
		resp.CustomMetadata[FinishReasonKey] = finish
	}
	resp.UsageMetadata, resp.CustomMetadata = m.mapUsage(u, id, served, resp.CustomMetadata)
	return resp, nil
}

// Metadata keys this dialect writes beside usage.MetadataKey.
const (
	// RefusalKey marks a response whose text is the model's refusal.
	RefusalKey = "core_models.refusal"
	// FinishReasonKey holds a finish reason with no genai equivalent.
	FinishReasonKey = "core_models.finish_reason"
)

func finishReason(s string) genai.FinishReason {
	switch s {
	case "":
		return genai.FinishReasonUnspecified
	case "stop", "tool_calls", "function_call":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	}
	return genai.FinishReasonOther
}

// mapUsage maps the server's usage onto genai's struct and a Detail. A
// field the server did not send is absent from both — never a zero.
// genai counts reasoning apart from candidates; OpenAI counts it inside
// completion_tokens, so it is subtracted where reported.
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
		if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens != nil {
			reasoning = *u.CompletionTokensDetails.ReasoningTokens
			d.ReasoningTokens = ptr(reasoning)
			um.ThoughtsTokenCount = clamp32(reasoning)
		}
		if u.PromptTokens != nil {
			um.PromptTokenCount = clamp32(*u.PromptTokens)
		}
		if u.CompletionTokens != nil {
			um.CandidatesTokenCount = clamp32(*u.CompletionTokens - reasoning)
		}
		switch {
		case u.TotalTokens != nil:
			um.TotalTokenCount = clamp32(*u.TotalTokens)
		case u.PromptTokens != nil && u.CompletionTokens != nil:
			um.TotalTokenCount = clamp32(*u.PromptTokens + *u.CompletionTokens)
		}
		if !m.opts.CachedTokensUnreliable && u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
			cached := *u.PromptTokensDetails.CachedTokens
			d.CacheReadTokens = ptr(cached)
			um.CachedContentTokenCount = clamp32(cached)
		}
	}
	resp := &llm.Response{CustomMetadata: md}
	usage.Attach(resp, d)
	return um, resp.CustomMetadata
}

// clamp32 narrows a count to genai's int32 fields, saturating rather
// than wrapping: a server that reports an absurd count gets a ceiling,
// never a negative number. Detail keeps the full int64.
func clamp32(n int64) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < 0:
		return 0
	}
	return int32(n)
}

// ToolChoiceKey marks a response whose request asked for a forced tool
// call the model cannot take, and was sent with "auto" instead.
const ToolChoiceKey = "core_models.tool_choice_downgraded"

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
