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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/internal/openaishared"
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
		// its reasoning forward as encrypted content (§7.2) — when the
		// profile replays reasoning at all (capabilities.reasoning_echo).
		// Without replay the encrypted content would be fetched for
		// nothing, and a server that does not offer it would refuse.
		Store:  false,
		Stream: stream,
	}
	if m.opts.ReasoningEcho {
		out.Include = []string{includeEncryptedReasoning}
	}
	cfg := req.Config
	if cfg == nil {
		cfg = &genai.GenerateContentConfig{}
	}
	// instructions is the Responses API's field for the system prompt,
	// and keeps the input list to the conversation itself.
	out.Instructions = textOf(cfg.SystemInstruction)
	items, err := convertContents(req.Contents, m.opts.ReasoningEcho)
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
	if cfg.CandidateCount > 1 {
		return nil, fmt.Errorf("openai-responses: candidate_count %d: only one candidate is supported", cfg.CandidateCount)
	}
	if field := unsupportedConfig(cfg); field != "" {
		return nil, fmt.Errorf("openai-responses: %s: the Responses API has no equivalent, so the request is refused rather than sent without it", field)
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
		choice, downgraded := m.toolChoice(tc.FunctionCallingConfig)
		out.ToolChoice = choice
		if downgraded {
			notes = map[string]any{ToolChoiceKey: true}
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

// toolChoice maps a function-calling config onto tool_choice. An
// allow-list is kept, never widened to every tool: one name forced is a
// named choice, and otherwise the list goes in an allowed_tools choice
// with mode auto or required. The bool reports a forced choice sent as
// unforced because the model rejects forcing (NoForcedToolChoice).
func (m *model) toolChoice(fc *genai.FunctionCallingConfig) (any, bool) {
	names := fc.AllowedFunctionNames
	allowed := func(mode string) any {
		tools := make([]namedToolChoice, len(names))
		for i, n := range names {
			tools[i] = namedToolChoice{Type: "function", Name: n}
		}
		return allowedTools{Type: "allowed_tools", Mode: mode, Tools: tools}
	}
	switch fc.Mode {
	case genai.FunctionCallingConfigModeNone:
		return "none", false
	case genai.FunctionCallingConfigModeAny:
		switch {
		case m.opts.NoForcedToolChoice:
			// The model rejects a forced choice: the closest request it
			// takes is the same tools unforced, and the caller is told.
			if len(names) > 0 {
				return allowed("auto"), true
			}
			return "auto", true
		case len(names) == 1:
			return namedToolChoice{Type: "function", Name: names[0]}, false
		case len(names) > 1:
			return allowed("required"), false
		}
		return "required", false
	case genai.FunctionCallingConfigModeAuto, genai.FunctionCallingConfigModeValidated:
		if len(names) > 0 {
			return allowed("auto"), false
		}
		return "auto", false
	}
	return nil, false
}

// unsupportedConfig names the first generation setting the Responses
// API cannot carry, or "" when there is none. Every one of them changes
// what the model produces or how it is billed, so dropping it quietly
// would answer a different request than the one asked.
func unsupportedConfig(cfg *genai.GenerateContentConfig) string {
	switch {
	case len(cfg.StopSequences) > 0:
		return "stop sequences"
	case cfg.Seed != nil:
		return "seed"
	case cfg.PresencePenalty != nil || cfg.FrequencyPenalty != nil:
		return "presence and frequency penalties"
	}
	return openaishared.UnsupportedConfig(cfg)
}

// reasoningOf maps genai's thinking settings onto reasoning. A thinking
// level names an effort directly. A thinking budget is not mapped, not
// even zero: the Responses API has no token budget, and the value that
// means "no reasoning" differs by model ("minimal" on some, "none" on
// later ones, neither on others), so guessing would make a valid
// request invalid. Thinking off is expressible only as a ThinkingLevel.
// IncludeThoughts asks for reasoning summaries, which OpenAI returns
// only on request.
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
func convertContents(contents []*genai.Content, echoReasoning bool) ([]inputItem, error) {
	ids := openaishared.NewIDAssigner()
	var out []inputItem
	for ci, c := range contents {
		if c == nil {
			continue
		}
		var items []inputItem
		var err error
		switch c.Role {
		case genai.RoleModel:
			items, err = modelItems(c, ids, echoReasoning)
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

func modelItems(c *genai.Content, ids *openaishared.IDAssigner, echoReasoning bool) ([]inputItem, error) {
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
			args, err := json.Marshal(openaishared.NonNilMap(p.FunctionCall.Args))
			if err != nil {
				return nil, fmt.Errorf("tool call %q: arguments: %w", p.FunctionCall.Name, err)
			}
			out = append(out, inputItem{
				Type: "function_call", CallID: ids.Call(p.FunctionCall.ID, p.FunctionCall.Name),
				Name: p.FunctionCall.Name, Arguments: string(args),
			})
		case p.Thought:
			// Reasoning goes back only where the profile replays it
			// (capabilities.reasoning_echo), and only as the encrypted item
			// this dialect received: with store false, OpenAI accepts a
			// reasoning item only with its encrypted content, and a thought
			// from another provider, or with none, has nothing to replay.
			if r, ok := decodeSignature(p.ThoughtSignature); ok && echoReasoning {
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

// userItems turns a user turn into input items in part order: a run of
// text and images is one message, and a function output between two
// runs splits them, so [text, output, text] stays three items in that
// order rather than the output hoisted and the text merged.
func userItems(c *genai.Content, ids *openaishared.IDAssigner) ([]inputItem, error) {
	var out []inputItem
	var parts []contentPart
	flush := func() {
		if len(parts) > 0 {
			out = append(out, inputItem{Type: "message", Role: "user", Content: parts})
			parts = nil
		}
	}
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionResponse != nil:
			body, err := json.Marshal(p.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("tool result %q: %w", p.FunctionResponse.Name, err)
			}
			flush()
			out = append(out, inputItem{Type: "function_call_output", CallID: ids.Response(p.FunctionResponse.ID, p.FunctionResponse.Name), Output: string(body)})
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
	flush()
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

func textOf(c *genai.Content) string { return openaishared.TextOf(c) }

func ptr[T any](v T) *T { return &v }

func f64(v *float32) *float64 { return openaishared.F64(v) }
