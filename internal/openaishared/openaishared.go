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

// Package openaishared holds what the two OpenAI-shaped dialects
// (openai-chat and openai-responses) do identically: usage mapping,
// error-envelope parsing, tool-call id assignment and the small
// helpers around them. One copy, so the dialects cannot drift on the
// rules they share (usage.Detail's "not reported is not zero" above
// all).
package openaishared

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/usage"
)

// Usage is a server's token counts, normalized from either dialect's
// wire shape. A nil count was not reported.
type Usage struct {
	Input, Output, Total *int64
	// Reasoning is usually counted inside Output (OpenAI); some servers
	// (xAI) count it beside Output. MapUsage tells them apart by Total.
	Reasoning *int64
	// CacheRead and CacheWrite are subsets of Input; CacheWrite1h is the
	// one-hour share of CacheWrite, where a server breaks it out.
	CacheRead, CacheWrite, CacheWrite1h *int64
}

// Stamp says what the Detail beside the usage names.
type Stamp struct {
	RequestID, ServedModel, Backend, Region string
	// CachedUnreliable records cache counts as not reported even when
	// the server sends them (a profile's usage.cached_tokens: unreliable).
	CachedUnreliable bool
}

// MapUsage maps u onto genai's struct and attaches a usage.Detail to md,
// returning both. A count the server did not send is absent from both,
// never a zero; a nil u gives no genai usage at all. genai counts
// reasoning apart from candidates, so it is subtracted where reported.
func MapUsage(u *Usage, s Stamp, md map[string]any) (*genai.GenerateContentResponseUsageMetadata, map[string]any) {
	d := &usage.Detail{
		ServedModel:       s.ServedModel,
		ProviderRequestID: s.RequestID,
		Backend:           s.Backend,
		Region:            s.Region,
	}
	var um *genai.GenerateContentResponseUsageMetadata
	if u != nil {
		um = &genai.GenerateContentResponseUsageMetadata{}
		var reasoning int64
		if u.Reasoning != nil {
			reasoning = *u.Reasoning
			d.ReasoningTokens = Ptr(reasoning)
			um.ThoughtsTokenCount = Clamp32(reasoning)
		}
		if u.Input != nil {
			um.PromptTokenCount = Clamp32(*u.Input)
		}
		if u.Output != nil {
			um.CandidatesTokenCount = Clamp32(*u.Output - reasoningInOutput(u, reasoning))
		}
		switch {
		case u.Total != nil:
			um.TotalTokenCount = Clamp32(*u.Total)
		case u.Input != nil && u.Output != nil:
			um.TotalTokenCount = Clamp32(*u.Input + *u.Output)
		}
		if !s.CachedUnreliable {
			if u.CacheRead != nil {
				d.CacheReadTokens = Ptr(*u.CacheRead)
				um.CachedContentTokenCount = Clamp32(*u.CacheRead)
			}
			// genai's struct has no cache-write bucket; the Detail does.
			if u.CacheWrite != nil {
				d.CacheWriteTokens = Ptr(*u.CacheWrite)
			}
			if u.CacheWrite1h != nil {
				d.CacheWrite1hTokens = Ptr(*u.CacheWrite1h)
			}
		}
	}
	resp := &llm.Response{CustomMetadata: md}
	usage.Attach(resp, d)
	return um, resp.CustomMetadata
}

// reasoningInOutput is how much of the server's output count is
// reasoning. OpenAI counts reasoning inside completion/output tokens
// (prompt + output = total), so the visible answer is output minus
// reasoning. xAI counts it beside them (prompt + output + reasoning =
// total): there the output count is already the answer, and subtracting
// reasoning again lost it — a reasoning reply billed as all thought and
// no answer. The total says which convention the server follows; with
// no total, OpenAI's, the documented one, is assumed.
func reasoningInOutput(u *Usage, reasoning int64) int64 {
	if reasoning == 0 || u.Total == nil || u.Input == nil || u.Output == nil {
		return reasoning
	}
	if *u.Input+*u.Output+reasoning == *u.Total && *u.Input+*u.Output != *u.Total {
		return 0
	}
	return reasoning
}

// Clamp32 narrows a count to genai's int32 fields, saturating rather
// than wrapping: an absurd count gets a ceiling, never a negative
// number. Detail keeps the full int64.
func Clamp32(n int64) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < 0:
		return 0
	}
	return int32(n)
}

// ErrorFields is what an error answer says about itself.
type ErrorFields struct {
	Type, Code, Message, Param string
}

type errorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Param   *string         `json:"param"`
}

type errorEnvelope struct {
	Error   *errorBody `json:"error"`
	Message string     `json:"message"`
	Detail  string     `json:"detail"`
}

// ParseError reads an error answer's body: OpenAI's {"error": {...}},
// the same wrapped in a one-element array (Vertex AI), or a bare
// message or detail. With nothing recognizable, the body itself is the
// message.
func ParseError(raw []byte) ErrorFields {
	var env errorEnvelope
	var wrapped []errorEnvelope
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped) > 0 {
		env = wrapped[0]
	} else {
		_ = json.Unmarshal(raw, &env)
	}
	var f ErrorFields
	switch {
	case env.Error != nil:
		f.Message, f.Type, f.Code = env.Error.Message, env.Error.Type, CodeString(env.Error.Code)
		if env.Error.Param != nil {
			f.Param = *env.Error.Param
		}
	case env.Message != "":
		f.Message = env.Message
	case env.Detail != "":
		f.Message = env.Detail
	}
	if f.Message == "" {
		f.Message = strings.TrimSpace(string(raw))
	}
	return f
}

// CodeString renders an error code that a server sends as a string or
// as a number.
func CodeString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// IDAssigner gives every tool call an id and every tool result the id
// of the call it answers. History written by another provider (Gemini
// does not set ids) has none; such calls get a generated id and their
// results are matched to them by name, in order.
type IDAssigner struct {
	pending map[string][]string
}

// NewIDAssigner returns an empty IDAssigner, one per conversation.
func NewIDAssigner() *IDAssigner { return &IDAssigner{pending: map[string][]string{}} }

// Call returns a call's id, generating one when it has none.
func (a *IDAssigner) Call(id, name string) string {
	if id != "" {
		return id
	}
	id = NewCallID()
	a.pending[name] = append(a.pending[name], id)
	return id
}

// Response returns the id a result answers: its own, or the oldest
// unanswered generated id for the same tool name.
func (a *IDAssigner) Response(id, name string) string {
	if id != "" {
		return id
	}
	if q := a.pending[name]; len(q) > 0 {
		a.pending[name] = q[1:]
		return q[0]
	}
	return NewCallID()
}

// NewCallID returns a fresh tool-call id.
func NewCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// TextOf joins a content's non-thought text parts.
func TextOf(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var parts []string
	for _, p := range c.Parts {
		if p != nil && p.Text != "" && !p.Thought {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// NonNilMap returns m, or an empty map for nil, so a tool call with no
// arguments marshals as {}.
func NonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// F64 widens an optional float32.
func F64(v *float32) *float64 {
	if v == nil {
		return nil
	}
	return Ptr(float64(*v))
}

// StampNotes adds a request's notes (a downgraded tool choice, say) to
// a response's metadata.
func StampNotes(r *llm.Response, notes map[string]any) {
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

// StampRetry records the retries a call took under retry.MetadataKey,
// when it took any.
func StampRetry(r *llm.Response, rec *retry.Record) {
	snap := rec.Snapshot()
	if snap == (retry.Snapshot{}) {
		return
	}
	if r.CustomMetadata == nil {
		r.CustomMetadata = map[string]any{}
	}
	r.CustomMetadata[retry.MetadataKey] = snap
}

// UnsupportedConfig names the first generation setting neither OpenAI
// API (Chat Completions, Responses) can carry, or "" when there is none.
// Each changes what the model produces or how it is billed, so dropping
// it quietly would answer a different request than the one asked. A
// dialect checks its own extra limits first (Responses also has no stop
// sequences, seed or penalties). Thinking settings are deliberately not
// here: products pass them generically, and Chat Completions servers take
// thinking per model through extra_body.
func UnsupportedConfig(cfg *genai.GenerateContentConfig) string {
	switch {
	case cfg == nil:
		return ""
	case cfg.TopK != nil:
		return "top_k (a server that accepts it, such as vLLM, takes it through the profile's extra_body)"
	case cfg.ResponseLogprobs || cfg.Logprobs != nil:
		return "logprobs"
	case len(cfg.SafetySettings) > 0:
		return "safety settings"
	case cfg.CachedContent != "":
		return "a cached-content reference (a Gemini context cache)"
	case !TextOnly(cfg.ResponseModalities):
		return fmt.Sprintf("response modalities %v", cfg.ResponseModalities)
	case cfg.SpeechConfig != nil:
		return "speech config"
	case cfg.AudioTimestamp:
		return "audio timestamps"
	case cfg.ImageConfig != nil:
		return "image config"
	case cfg.MediaResolution != "" && cfg.MediaResolution != genai.MediaResolutionUnspecified:
		return "media resolution"
	case cfg.RoutingConfig != nil || cfg.ModelSelectionConfig != nil:
		return "model routing"
	case len(cfg.Labels) > 0:
		return "labels"
	case cfg.ServiceTier != "":
		return "service tier"
	case cfg.ModelArmorConfig != nil:
		return "Model Armor config"
	case cfg.EnableEnhancedCivicAnswers != nil:
		return "enhanced civic answers"
	case cfg.ResponseMIMEType != "" && cfg.ResponseMIMEType != "text/plain" && cfg.ResponseMIMEType != "application/json":
		return fmt.Sprintf("response MIME type %q", cfg.ResponseMIMEType)
	}
	return ""
}

// TextOnly reports whether modalities asks for nothing but text, which is
// all the OpenAI dialects produce. None asked for counts as text.
func TextOnly(modalities []string) bool {
	return len(modalities) == 0 || (len(modalities) == 1 && strings.EqualFold(modalities[0], "TEXT"))
}
