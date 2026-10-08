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

// Package llm is the contract every core-models provider adapter
// implements. It mirrors ADK's model package field for field, so that
// the adkv1 and adkv2 modules can adapt it to either ADK major with
// plain copies — and it imports no ADK, so the core module never puts
// an ADK major into a consumer's build.
//
// mast is on google.golang.org/adk/v2 and core-agent on
// google.golang.org/adk v1. Their model.LLM interfaces are identical in
// shape and distinct as Go types; this package is the third, neutral
// spelling both can reach (docs/design.md §4, decision D2).
package llm

import (
	"context"
	"iter"

	"google.golang.org/genai"
)

// LLM is a model a caller can send one request to. It is ADK's
// model.LLM over this package's Request and Response.
type LLM interface {
	// Name reports the model this LLM serves.
	Name() string

	// GenerateContent sends req. With stream false it yields one
	// complete Response; with stream true it yields partial Responses
	// (Partial set) followed by a final complete one.
	GenerateContent(ctx context.Context, req *Request, stream bool) iter.Seq2[*Response, error]
}

// Request mirrors ADK's model.LLMRequest without its Tools field. That
// field maps tool names to ADK tool objects for ADK's own flows to
// dispatch on; no adapter reads it, since the declarations a provider
// sends come from Config.Tools.
type Request struct {
	Model    string
	Contents []*genai.Content
	Config   *genai.GenerateContentConfig
}

// Response mirrors ADK's model.LLMResponse, JSON tags included (ADK v2
// spells them; v1 has none, and Go's decoder matches either).
//
// CustomMetadata is where an adapter attaches what genai's usage struct
// cannot hold — see package usage.
type Response struct {
	Content                 *genai.Content                              `json:"content,omitempty"`
	CitationMetadata        *genai.CitationMetadata                     `json:"citationMetadata,omitempty"`
	GroundingMetadata       *genai.GroundingMetadata                    `json:"groundingMetadata,omitempty"`
	UsageMetadata           *genai.GenerateContentResponseUsageMetadata `json:"usageMetadata,omitempty"`
	CustomMetadata          map[string]any                              `json:"customMetadata,omitempty"`
	LogprobsResult          *genai.LogprobsResult                       `json:"logprobsResult,omitempty"`
	InputTranscription      *genai.Transcription                        `json:"inputTranscription,omitempty"`
	OutputTranscription     *genai.Transcription                        `json:"outputTranscription,omitempty"`
	ModelVersion            string                                      `json:"modelVersion,omitempty"`
	Partial                 bool                                        `json:"partial,omitempty"`
	TurnComplete            bool                                        `json:"turnComplete,omitempty"`
	Interrupted             bool                                        `json:"interrupted,omitempty"`
	SessionResumptionHandle string                                      `json:"sessionResumptionHandle,omitempty"`
	ErrorCode               string                                      `json:"errorCode,omitempty"`
	ErrorMessage            string                                      `json:"errorMessage,omitempty"`
	FinishReason            genai.FinishReason                          `json:"finishReason,omitempty"`
	AvgLogprobs             float64                                     `json:"avgLogprobs,omitempty"`
}
