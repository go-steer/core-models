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

import "encoding/json"

// The wire types are this package's own rather than an SDK's. The
// servers this dialect reaches are OpenAI-shaped, not OpenAI: they add
// fields (reasoning_content, reasoning), omit fields (usage details,
// tool-call ids), and differ on what they accept. Pointer fields keep
// "absent" distinct from "zero" on the way in, which usage.Detail
// requires, and nothing unknown is rejected.

type chatRequest struct {
	Model            string          `json:"model"`
	Messages         []message       `json:"messages"`
	Tools            []tool          `json:"tools,omitempty"`
	ToolChoice       any             `json:"tool_choice,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *streamOptions  `json:"stream_options,omitempty"`
	MaxTokens        *int64          `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	ResponseFormat   *responseFormat `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// message is one chat message. Content is a string, a []contentPart,
// or nil (an assistant turn that only calls tools).
type message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type tool struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type namedToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string `json:"name"`
	Schema any    `json:"schema"`
}

// Responses.

type chatResponse struct {
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *wireUsage `json:"usage"`
}

type choice struct {
	Index        int         `json:"index"`
	Message      respMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type respMessage struct {
	Content          *string        `json:"content"`
	ReasoningContent *string        `json:"reasoning_content"`
	Reasoning        *string        `json:"reasoning"`
	Refusal          *string        `json:"refusal"`
	ToolCalls        []respToolCall `json:"tool_calls"`
}

// reasoning is whichever of the two spellings the server used.
func (m respMessage) reasoning() *string {
	if m.ReasoningContent != nil {
		return m.ReasoningContent
	}
	return m.Reasoning
}

type respToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	TotalTokens         *int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
		// CreatedCacheTokens is vLLM's count of prompt tokens written to
		// its prefix cache (with --enable-prompt-tokens-details).
		CreatedCacheTokens *int64 `json:"created_cache_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *wireUsage    `json:"usage"`
	Error   *errorBody    `json:"error"`
}

type chunkChoice struct {
	Index        int         `json:"index"`
	Delta        respMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type errorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

type errorEnvelope struct {
	Error *errorBody `json:"error"`
	// Some servers (older vLLM, llama.cpp) put the message at the top.
	Message string `json:"message"`
	Detail  string `json:"detail"`
}
