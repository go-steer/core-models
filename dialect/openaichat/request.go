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

package openaichat

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

	"github.com/go-steer/core-models/llm"
)

// buildRequest converts a core-models request into a chat request.
// Anything genai can express that Chat Completions cannot is refused
// with an error naming it, never dropped: a silently missing tool or
// schema is the defect class toolwire exists for.
func (m *model) buildRequest(req *llm.Request, stream bool) (*chatRequest, map[string]any, error) {
	out := &chatRequest{Model: m.id}
	if stream {
		out.Stream = true
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	cfg := req.Config
	if cfg == nil {
		cfg = &genai.GenerateContentConfig{}
	}

	if sys := textOf(cfg.SystemInstruction); sys != "" {
		out.Messages = append(out.Messages, message{Role: "system", Content: sys})
	}
	msgs, err := m.convertContents(req.Contents)
	if err != nil {
		return nil, nil, err
	}
	out.Messages = append(out.Messages, msgs...)

	notes, err := m.applyConfig(out, cfg)
	if err != nil {
		return nil, nil, err
	}
	return out, notes, nil
}

func (m *model) applyConfig(out *chatRequest, cfg *genai.GenerateContentConfig) (map[string]any, error) {
	var notes map[string]any
	if cfg.CandidateCount > 1 {
		return nil, fmt.Errorf("openai-chat: candidate_count %d: only one candidate is supported", cfg.CandidateCount)
	}
	if cfg.MaxOutputTokens > 0 {
		out.MaxTokens = ptr(int64(cfg.MaxOutputTokens))
	}
	out.Temperature = f64(cfg.Temperature)
	out.TopP = f64(cfg.TopP)
	out.PresencePenalty = f64(cfg.PresencePenalty)
	out.FrequencyPenalty = f64(cfg.FrequencyPenalty)
	if cfg.Seed != nil {
		out.Seed = ptr(int64(*cfg.Seed))
	}
	out.Stop = cfg.StopSequences

	for i, t := range cfg.Tools {
		if t == nil {
			continue
		}
		rest := *t
		rest.FunctionDeclarations = nil
		if !reflect.ValueOf(rest).IsZero() {
			return nil, fmt.Errorf("openai-chat: tools[%d] carries a provider built-in (search, code execution, retrieval, …); this dialect sends function tools only", i)
		}
		for _, d := range t.FunctionDeclarations {
			params := parametersOf(d)
			out.Tools = append(out.Tools, tool{
				Type:     "function",
				Function: functionDef{Name: d.Name, Description: d.Description, Parameters: params},
			})
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
				// The model rejects a forced choice (gpt-oss on Vertex AI).
				// "auto" with the same tools is the closest request it will
				// take; the caller is told it was not forced.
				out.ToolChoice = "auto"
				notes = map[string]any{ToolChoiceKey: true}
			case len(fc.AllowedFunctionNames) == 1:
				var named namedToolChoice
				named.Type = "function"
				named.Function.Name = fc.AllowedFunctionNames[0]
				out.ToolChoice = named
			default:
				out.ToolChoice = "required"
			}
		}
	}
	// With tools on the request and no choice expressed, say "auto"
	// rather than leave it to the server: Qwen on Vertex AI documents
	// worse tool calling when tool_choice is unset, and "auto" is what
	// every other server assumes anyway.
	if out.ToolChoice == nil && len(out.Tools) > 0 {
		out.ToolChoice = "auto"
	}

	switch schema := responseSchemaOf(cfg); {
	case schema != nil:
		if !m.opts.ResponseSchema {
			return nil, errors.New("openai-chat: the request needs a response schema and this profile does not declare capabilities.response_schema; " +
				"a server that accepts the field and ignores it returns prose where JSON was promised")
		}
		out.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: &jsonSchema{Name: "response", Schema: schema}}
	case cfg.ResponseMIMEType == "application/json":
		out.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	return notes, nil
}

func responseSchemaOf(cfg *genai.GenerateContentConfig) any {
	switch {
	case cfg.ResponseJsonSchema != nil:
		return cfg.ResponseJsonSchema
	case cfg.ResponseSchema != nil:
		return schemaToJSON(cfg.ResponseSchema)
	}
	return nil
}

// parametersOf returns a declaration's argument schema as JSON Schema.
// A tool with no arguments still sends an empty object schema, which
// every server accepts and some require.
func parametersOf(d *genai.FunctionDeclaration) any {
	switch {
	case d.ParametersJsonSchema != nil:
		return d.ParametersJsonSchema
	case d.Parameters != nil:
		return schemaToJSON(d.Parameters)
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// convertContents turns the conversation into chat messages. A user
// content's function responses become tool messages, emitted before
// any text in the same content, because a tool message must directly
// follow the assistant turn that called it.
func (m *model) convertContents(contents []*genai.Content) ([]message, error) {
	ids := newIDAssigner()
	var out []message
	for ci, c := range contents {
		if c == nil {
			continue
		}
		switch c.Role {
		case genai.RoleModel:
			msg, ok, err := m.assistantMessage(c, ids)
			if err != nil {
				return nil, fmt.Errorf("openai-chat: contents[%d]: %w", ci, err)
			}
			if ok {
				out = append(out, msg)
			}
		case genai.RoleUser, "":
			msgs, err := userMessages(c, ids)
			if err != nil {
				return nil, fmt.Errorf("openai-chat: contents[%d]: %w", ci, err)
			}
			out = append(out, msgs...)
		default:
			return nil, fmt.Errorf("openai-chat: contents[%d]: unknown role %q", ci, c.Role)
		}
	}
	return out, nil
}

func (m *model) assistantMessage(c *genai.Content, ids *idAssigner) (message, bool, error) {
	var text, thought strings.Builder
	msg := message{Role: "assistant"}
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			args, err := json.Marshal(nonNilMap(p.FunctionCall.Args))
			if err != nil {
				return message{}, false, fmt.Errorf("tool call %q: arguments: %w", p.FunctionCall.Name, err)
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{
				ID:       ids.call(p.FunctionCall.ID, p.FunctionCall.Name),
				Type:     "function",
				Function: functionCall{Name: p.FunctionCall.Name, Arguments: string(args)},
			})
		case p.Thought:
			thought.WriteString(p.Text)
		case p.Text != "":
			text.WriteString(p.Text)
		default:
			if err := unsupported(p); err != nil {
				return message{}, false, err
			}
		}
	}
	if text.Len() > 0 {
		msg.Content = text.String()
	}
	// Reasoning goes back only where the profile says the server wants
	// it (DeepSeek- and Kimi-family servers require it on tool-call
	// turns); elsewhere a server may reject or misread the field.
	if m.opts.ReasoningEcho && thought.Len() > 0 {
		msg.ReasoningContent = ptr(thought.String())
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 && msg.ReasoningContent == nil {
		return message{}, false, nil
	}
	return msg, true, nil
}

func userMessages(c *genai.Content, ids *idAssigner) ([]message, error) {
	var tools []message
	var parts []contentPart
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionResponse != nil:
			body, err := json.Marshal(p.FunctionResponse.Response)
			if err != nil {
				return nil, fmt.Errorf("tool result %q: %w", p.FunctionResponse.Name, err)
			}
			tools = append(tools, message{
				Role:       "tool",
				Content:    string(body),
				ToolCallID: ids.response(p.FunctionResponse.ID, p.FunctionResponse.Name),
			})
		case p.InlineData != nil:
			if !strings.HasPrefix(p.InlineData.MIMEType, "image/") {
				return nil, fmt.Errorf("inline data of type %q: only images are supported", p.InlineData.MIMEType)
			}
			url := "data:" + p.InlineData.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.InlineData.Data)
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
		case p.FileData != nil:
			isHTTP := strings.HasPrefix(p.FileData.FileURI, "https://") || strings.HasPrefix(p.FileData.FileURI, "http://")
			if !strings.HasPrefix(p.FileData.MIMEType, "image/") || !isHTTP {
				return nil, fmt.Errorf("file data %q (%s): only http(s) image URLs are supported", p.FileData.FileURI, p.FileData.MIMEType)
			}
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: p.FileData.FileURI}})
		case p.Text != "":
			parts = append(parts, contentPart{Type: "text", Text: p.Text})
		default:
			if err := unsupported(p); err != nil {
				return nil, err
			}
		}
	}
	out := tools
	switch {
	case len(parts) == 0:
	case len(parts) == 1 && parts[0].Type == "text":
		out = append(out, message{Role: "user", Content: parts[0].Text})
	default:
		out = append(out, message{Role: "user", Content: parts})
	}
	return out, nil
}

// unsupported returns an error for a part this dialect cannot carry,
// and nil for a part with nothing in it.
func unsupported(p *genai.Part) error {
	switch {
	case p.ExecutableCode != nil, p.CodeExecutionResult != nil:
		return errors.New("a code-execution part (from a provider built-in) cannot be sent to an OpenAI-shaped server")
	case p.VideoMetadata != nil:
		return errors.New("video parts are not supported")
	}
	return nil
}

// idAssigner gives every tool call an id and every tool result the id
// of the call it answers. History written by another provider (Gemini
// does not set ids) has none; such calls get a generated id and their
// results are matched to them by name, in order.
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
