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

import "encoding/json"

// The request and response shapes of POST /v1/responses, as OpenAI
// documents them, with the fields vLLM adds. Pointers keep a value the
// server did not send distinct from a zero it did (usage.Detail's rule).

type responsesRequest struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []inputItem    `json:"input"`
	Tools           []tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxOutputTokens *int64         `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Reasoning       *reasoningOpts `json:"reasoning,omitempty"`
	Text            *textOpts      `json:"text,omitempty"`
	Store           bool           `json:"store"`
	Include         []string       `json:"include,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
}

// inputItem is one entry of the request's input list: a message, a
// function call the model made, its output, or a reasoning item being
// replayed. Only the fields of its Type are set.
type inputItem struct {
	Type string `json:"type"`

	// message
	Role    string        `json:"role,omitempty"`
	Content []contentPart `json:"content,omitempty"`

	// function_call, function_call_output
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`

	// reasoning
	ID               string         `json:"id,omitempty"`
	Summary          *[]summaryPart `json:"summary,omitempty"`
	EncryptedContent string         `json:"encrypted_content,omitempty"`
}

type contentPart struct {
	Type     string `json:"type"` // input_text, output_text, input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type summaryPart struct {
	Type string `json:"type"` // summary_text
	Text string `json:"text"`
}

// tool is a function tool in the Responses API's flat shape: name and
// parameters at the top, not nested under "function" as in Chat
// Completions.
type tool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

type namedToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// allowedTools restricts the model to a subset of the request's tools,
// unforced (mode auto) or forced (mode required).
type allowedTools struct {
	Type  string            `json:"type"` // allowed_tools
	Mode  string            `json:"mode"`
	Tools []namedToolChoice `json:"tools"`
}

type reasoningOpts struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type textOpts struct {
	Format *textFormat `json:"format,omitempty"`
}

type textFormat struct {
	Type   string `json:"type"` // json_schema, json_object
	Name   string `json:"name,omitempty"`
	Schema any    `json:"schema,omitempty"`
}

type response struct {
	ID                string             `json:"id"`
	Model             string             `json:"model"`
	Status            string             `json:"status"`
	Output            []outputItem       `json:"output"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Error             *errorBody         `json:"error"`
	Usage             *wireUsage         `json:"usage"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

// outputItem is one entry of a response's output: a message, a
// function call, or a reasoning item.
type outputItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// message
	Role    string          `json:"role"`
	Content []outputContent `json:"content"`

	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// reasoning. OpenAI returns summaries (summary_text) and, with
	// include reasoning.encrypted_content, the encrypted reasoning;
	// vLLM returns the reasoning itself as content of type
	// reasoning_text.
	Summary          []summaryPart `json:"summary"`
	EncryptedContent *string       `json:"encrypted_content"`
}

type outputContent struct {
	Type    string  `json:"type"` // output_text, refusal, reasoning_text
	Text    string  `json:"text"`
	Refusal *string `json:"refusal"`
}

type wireUsage struct {
	InputTokens         *int64        `json:"input_tokens"`
	OutputTokens        *int64        `json:"output_tokens"`
	TotalTokens         *int64        `json:"total_tokens"`
	InputTokensDetails  *inputDetail  `json:"input_tokens_details"`
	OutputTokensDetails *outputDetail `json:"output_tokens_details"`
}

type inputDetail struct {
	CachedTokens *int64 `json:"cached_tokens"`
	// CacheWriteTokens is vLLM's; OpenAI does not send it.
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type outputDetail struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

// streamEvent is one server-sent event. Type is the event's type;
// Delta carries a text delta; Response the whole response on
// response.completed, response.incomplete and response.failed; Code and
// Message an error event's.
type streamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Response *response       `json:"response"`
	Code     json.RawMessage `json:"code"`
	Message  string          `json:"message"`
}

type errorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}
