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

package openairesp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/internal/schemajson"
	"github.com/go-steer/core-models/llm"
)

// buildRequest converts a core-models request into a Responses request.
// Anything genai can express that the Responses API cannot is refused
// with an error naming it, never dropped (docs/design.md §7.1).
func (m *model) buildRequest(req *llm.Request, stream bool) (*responsesRequest, map[string]any, error) {
	out := &responsesRequest{
		Model: m.id,
		// Stateless: nothing is kept server-side, so a tool loop carries
		// its reasoning forward as encrypted content (§7.2).
		Store:   false,
		Include: []string{"reasoning.encrypted_content"},
		Stream:  stream,
	}
	cfg := req.Config
	if cfg == nil {
		cfg = &genai.GenerateContentConfig{}
	}
	// instructions is the Responses API's field for the system prompt,
	// and keeps the input list to the conversation itself.
	out.Instructions = textOf(cfg.SystemInstruction)
	items, err := convertContents(req.Contents)
	if err != nil {
		return nil, nil, err
	}
	out.Input = items
	notes, err := m.applyConfig(out, cfg)
	if err != nil {
		return nil, nil, err
	}
	return out, notes, nil
}

func (m *model) applyConfig(out *responsesRequest, cfg *genai.GenerateContentConfig) (map[string]any, error) {
	var notes map[string]any
	switch {
	case cfg.CandidateCount > 1:
		return nil, fmt.Errorf("openai-responses: candidate_count %d: only one candidate is supported", cfg.CandidateCount)
	case len(cfg.StopSequences) > 0:
		return nil, errors.New("openai-responses: stop sequences: the Responses API has no stop parameter")
	case cfg.Seed != nil:
		return nil, errors.New("openai-responses: seed: the Responses API has no seed parameter")
	case cfg.PresencePenalty != nil || cfg.FrequencyPenalty != nil:
		return nil, errors.New("openai-responses: presence and frequency penalties: the Responses API has neither")
	}
	if cfg.MaxOutputTokens > 0 {
		out.MaxOutputTokens = ptr(int64(cfg.MaxOutputTokens))
	}
	out.Temperature = f64(cfg.Temperature)
	out.TopP = f64(cfg.TopP)
	out.Reasoning = reasoningOf(cfg.ThinkingConfig)

	for i, t := range cfg.Tools {
		if t == nil {
			continue
		}
		rest := *t
		rest.FunctionDeclarations = nil
		if !reflect.ValueOf(rest).IsZero() {
			return nil, fmt.Errorf("openai-responses: tools[%d] carries a provider built-in (search, code execution, retrieval, …); this dialect sends function tools only", i)
		}
		for _, d := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, tool{Type: "function", Name: d.Name, Description: d.Description, Parameters: parametersOf(d)})
		}
	}

	if tc := cfg.ToolConfig; tc != nil && tc.FunctionCallingConfig != nil {
		fc := tc.FunctionCallingConfig
		switch fc.Mode {
		case genai.FunctionCallingConfigModeAuto, genai.FunctionCallingConfigModeValidated:
			out.ToolChoice = "auto"
		case genai.FunctionCallingConfigModeNone:
			out.ToolChoice = "none"
		case genai.FunctionCallingConfigModeAny:
			switch {
			case m.opts.NoForcedToolChoice:
				// The model rejects a forced choice: "auto" with the same
				// tools is the closest request it will take, and the
				// caller is told it was not forced.
				out.ToolChoice = "auto"
				notes = map[string]any{ToolChoiceKey: true}
			case len(fc.AllowedFunctionNames) == 1:
				out.ToolChoice = namedToolChoice{Type: "function", Name: fc.AllowedFunctionNames[0]}
			default:
				out.ToolChoice = "required"
			}
		}
	}

	switch schema := responseSchemaOf(cfg); {
	case schema != nil:
		if !m.opts.ResponseSchema {
			return nil, errors.New("openai-responses: the request needs a response schema and this profile does not declare capabilities.response_schema; " +
				"a server that accepts the field and ignores it returns prose where JSON was promised")
		}
		out.Text = &textOpts{Format: &textFormat{Type: "json_schema", Name: "response", Schema: schema}}
	case cfg.ResponseMIMEType == "application/json":
		out.Text = &textOpts{Format: &textFormat{Type: "json_object"}}
	}
	return notes, nil
}

// reasoningOf maps genai's thinking settings onto reasoning: a thinking
// level names an effort directly, and a budget of zero is the least
// reasoning the API offers. A positive budget is not mapped: the
// Responses API has no token budget, so the server's default effort
// applies. IncludeThoughts asks for reasoning summaries, which OpenAI
// returns only on request.
func reasoningOf(tc *genai.ThinkingConfig) *reasoningOpts {
	if tc == nil {
		return nil
	}
	r := &reasoningOpts{}
	switch tc.ThinkingLevel {
	case genai.ThinkingLevelMinimal:
		r.Effort = "minimal"
	case genai.ThinkingLevelLow:
		r.Effort = "low"
	case genai.ThinkingLevelMedium:
		r.Effort = "medium"
	case genai.ThinkingLevelHigh:
		r.Effort = "high"
	default:
		if tc.ThinkingBudget != nil && *tc.ThinkingBudget == 0 {
			r.Effort = "minimal"
		}
	}
	if tc.IncludeThoughts {
		r.Summary = "auto"
	}
	if *r == (reasoningOpts{}) {
		return nil
	}
	return r
}

func responseSchemaOf(cfg *genai.GenerateContentConfig) any {
	switch {
	case cfg.ResponseJsonSchema != nil:
		return cfg.ResponseJsonSchema
	case cfg.ResponseSchema != nil:
		return schemajson.FromGenai(cfg.ResponseSchema)
	}
	return nil
}

// parametersOf returns a declaration's argument schema as JSON Schema.
// A tool with no arguments still sends an empty object schema.
func parametersOf(d *genai.FunctionDeclaration) any {
	switch {
	case d.ParametersJsonSchema != nil:
		return d.ParametersJsonSchema
	case d.Parameters != nil:
		return schemajson.FromGenai(d.Parameters)
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// convertContents turns the conversation into input items, keeping the
// order of parts within each turn: a model turn's reasoning, text and
// function calls; a user turn's function outputs and message.
func convertContents(contents []*genai.Content) ([]inputItem, error) {
	ids := newIDAssigner()
	var out []inputItem
	for ci, c := range contents {
		if c == nil {
			continue
		}
		var items []inputItem
		var err error
		switch c.Role {
		case genai.RoleModel:
			items, err = modelItems(c, ids)
		case genai.RoleUser, "":
			items, err = userItems(c, ids)
		default:
			err = fmt.Errorf("unknown role %q", c.Role)
		}
		if err != nil {
			return nil, fmt.Errorf("openai-responses: contents[%d]: %w", ci, err)
		}
		out = append(out, items...)
	}
	return out, nil
}

func modelItems(c *genai.Content, ids *idAssigner) ([]inputItem, error) {
	var out []inputItem
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, inputItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: text.String()}}})
			text.Reset()
		}
	}
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			flush()
			args, err := json.Marshal(nonNilMap(p.FunctionCall.Args))
			if err != nil {
				return nil, fmt.Errorf("tool call %q: arguments: %w", p.FunctionCall.Name, err)
			}
			out = append(out, inputItem{
				Type: "function_call", CallID: ids.call(p.FunctionCall.ID, p.FunctionCall.Name),
				Name: p.FunctionCall.Name, Arguments: string(args),
			})
		case p.Thought:
			// Reasoning goes back only as the encrypted item this dialect
			// received: with store false, OpenAI accepts a reasoning item
			// only with its encrypted content, and a thought from another
			// provider, or with none, has nothing to replay.
			if r, ok := decodeSignature(p.ThoughtSignature); ok {
				flush()
				summary := []summaryPart{}
				if p.Text != "" {
					summary = append(summary, summaryPart{Type: "summary_text", Text: p.Text})
				}
				out = append(out, inputItem{Type: "reasoning", ID: r.ID, Summary: &summary, EncryptedContent: r.Encrypted})
			}
		case p.Text != "":
			text.WriteString(p.Text)
		default:
			if err := unsupported(p); err != nil {
				return nil, err
			}
		}
	}
	flush()
	return out, nil
}

func userItems(c *genai.Content, ids *idAssigner) ([]inputItem, error) {
	var out []inputItem
	var parts []contentPart
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionResponse != nil:
			body, err := json.Marshal(p.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("tool result %q: %w", p.FunctionResponse.Name, err)
			}
			out = append(out, inputItem{Type: "function_call_output", CallID: ids.response(p.FunctionResponse.ID, p.FunctionResponse.Name), Output: string(body)})
		case p.InlineData != nil:
			if !strings.HasPrefix(p.InlineData.MIMEType, "image/") {
				return nil, fmt.Errorf("inline data of type %q: only images are supported", p.InlineData.MIMEType)
			}
			url := "data:" + p.InlineData.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.InlineData.Data)
			parts = append(parts, contentPart{Type: "input_image", ImageURL: url})
		case p.FileData != nil:
			isHTTP := strings.HasPrefix(p.FileData.FileURI, "https://") || strings.HasPrefix(p.FileData.FileURI, "http://")
			if !strings.HasPrefix(p.FileData.MIMEType, "image/") || !isHTTP {
				return nil, fmt.Errorf("file data %q (%s): only http(s) image URLs are supported", p.FileData.FileURI, p.FileData.MIMEType)
			}
			parts = append(parts, contentPart{Type: "input_image", ImageURL: p.FileData.FileURI})
		case p.Text != "":
			parts = append(parts, contentPart{Type: "input_text", Text: p.Text})
		default:
			if err := unsupported(p); err != nil {
				return nil, err
			}
		}
	}
	if len(parts) > 0 {
		out = append(out, inputItem{Type: "message", Role: "user", Content: parts})
	}
	return out, nil
}

// unsupported returns an error for a part this dialect cannot carry,
// and nil for a part with nothing in it.
func unsupported(p *genai.Part) error {
	switch {
	case p.ExecutableCode != nil, p.CodeExecutionResult != nil:
		return errors.New("a code-execution part (from a provider built-in) cannot be sent to the Responses API")
	case p.VideoMetadata != nil:
		return errors.New("video parts are not supported")
	}
	return nil
}

// reasoningSignature is what a reasoning item leaves in
// genai.Part.ThoughtSignature: its id and encrypted content, both of
// which the API needs to accept the item back. Marked with a version so
// a signature from another provider (Gemini, Anthropic), which is
// opaque bytes, never decodes as one.
type reasoningSignature struct {
	V         string `json:"v"`
	ID        string `json:"id"`
	Encrypted string `json:"enc"`
}

const signatureVersion = "openai-responses/1"

func encodeSignature(id, encrypted string) []byte {
	b, _ := json.Marshal(reasoningSignature{V: signatureVersion, ID: id, Encrypted: encrypted})
	return b
}

func decodeSignature(b []byte) (reasoningSignature, bool) {
	var r reasoningSignature
	if len(b) == 0 || json.Unmarshal(b, &r) != nil || r.V != signatureVersion || r.ID == "" || r.Encrypted == "" {
		return reasoningSignature{}, false
	}
	return r, true
}

// idAssigner gives every function call a call_id and every output the
// call_id of the call it answers. History written by another provider
// (Gemini does not set ids) has none; such calls get a generated id and
// their outputs are matched to them by name, in order.
type idAssigner struct {
	pending map[string][]string
}

func newIDAssigner() *idAssigner { return &idAssigner{pending: map[string][]string{}} }

func (a *idAssigner) call(id, name string) string {
	if id != "" {
		return id
	}
	id = newCallID()
	a.pending[name] = append(a.pending[name], id)
	return id
}

func (a *idAssigner) response(id, name string) string {
	if id != "" {
		return id
	}
	if q := a.pending[name]; len(q) > 0 {
		a.pending[name] = q[1:]
		return q[0]
	}
	return newCallID()
}

func newCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

func textOf(c *genai.Content) string {
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

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func ptr[T any](v T) *T { return &v }

func f64(v *float32) *float64 {
	if v == nil {
		return nil
	}
	return ptr(float64(*v))
}
