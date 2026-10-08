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

// Package adkv1 adapts core-models to the ADK major its import path
// names. mast is on ADK v2 and core-agent on ADK v1; there is one
// adapter package per major, and they are the same file with the
// import path changed — dev/ci/presubmits/shims-lockstep.sh holds them
// to that, so edit this one and regenerate the other with --fix.
//
// Every conversion is a field copy: llm.Request and llm.Response mirror
// ADK's types, and this module's tests fail the day ADK adds a field
// the mirror lacks. Pointers and maps are shared, not cloned, exactly as
// passing the ADK value through would share them.
package adkv1

import (
	"context"
	"iter"

	"google.golang.org/adk/model"

	"github.com/go-steer/core-models/llm"
)

// Wrap adapts a core-models LLM to ADK's model.LLM. Wrapping the result
// of FromADK hands back the original ADK model rather than stacking two
// adapters.
func Wrap(m llm.LLM) model.LLM {
	if f, ok := m.(fromADK); ok {
		return f.inner
	}
	return toADK{inner: m}
}

// FromADK adapts an ADK model.LLM to llm.LLM, so a decorator written
// against core-models can sit in front of a model ADK built. Unwrapping
// the result of Wrap hands back the original.
func FromADK(m model.LLM) llm.LLM {
	if t, ok := m.(toADK); ok {
		return t.inner
	}
	return fromADK{inner: m}
}

type toADK struct{ inner llm.LLM }

func (t toADK) Name() string { return t.inner.Name() }

func (t toADK) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range t.inner.GenerateContent(ctx, Request(req), stream) {
			if !yield(ADKResponse(resp), err) {
				return
			}
		}
	}
}

type fromADK struct{ inner model.LLM }

func (f fromADK) Name() string { return f.inner.Name() }

func (f fromADK) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		for resp, err := range f.inner.GenerateContent(ctx, ADKRequest(req), stream) {
			if !yield(Response(resp), err) {
				return
			}
		}
	}
}

// Request converts an ADK request. Its Tools map has no counterpart and
// is dropped: it holds ADK tool objects for ADK's own dispatch, and the
// declarations a provider sends are in Config.Tools.
func Request(r *model.LLMRequest) *llm.Request {
	if r == nil {
		return nil
	}
	return &llm.Request{Model: r.Model, Contents: r.Contents, Config: r.Config}
}

// ADKRequest converts a core-models request. Tools is left nil.
func ADKRequest(r *llm.Request) *model.LLMRequest {
	if r == nil {
		return nil
	}
	return &model.LLMRequest{Model: r.Model, Contents: r.Contents, Config: r.Config}
}

// Response converts an ADK response.
func Response(r *model.LLMResponse) *llm.Response {
	if r == nil {
		return nil
	}
	return &llm.Response{
		Content:                 r.Content,
		CitationMetadata:        r.CitationMetadata,
		GroundingMetadata:       r.GroundingMetadata,
		UsageMetadata:           r.UsageMetadata,
		CustomMetadata:          r.CustomMetadata,
		LogprobsResult:          r.LogprobsResult,
		InputTranscription:      r.InputTranscription,
		OutputTranscription:     r.OutputTranscription,
		ModelVersion:            r.ModelVersion,
		Partial:                 r.Partial,
		TurnComplete:            r.TurnComplete,
		Interrupted:             r.Interrupted,
		SessionResumptionHandle: r.SessionResumptionHandle,
		ErrorCode:               r.ErrorCode,
		ErrorMessage:            r.ErrorMessage,
		FinishReason:            r.FinishReason,
		AvgLogprobs:             r.AvgLogprobs,
	}
}

// ADKResponse converts a core-models response.
func ADKResponse(r *llm.Response) *model.LLMResponse {
	if r == nil {
		return nil
	}
	return &model.LLMResponse{
		Content:                 r.Content,
		CitationMetadata:        r.CitationMetadata,
		GroundingMetadata:       r.GroundingMetadata,
		UsageMetadata:           r.UsageMetadata,
		CustomMetadata:          r.CustomMetadata,
		LogprobsResult:          r.LogprobsResult,
		InputTranscription:      r.InputTranscription,
		OutputTranscription:     r.OutputTranscription,
		ModelVersion:            r.ModelVersion,
		Partial:                 r.Partial,
		TurnComplete:            r.TurnComplete,
		Interrupted:             r.Interrupted,
		SessionResumptionHandle: r.SessionResumptionHandle,
		ErrorCode:               r.ErrorCode,
		ErrorMessage:            r.ErrorMessage,
		FinishReason:            r.FinishReason,
		AvgLogprobs:             r.AvgLogprobs,
	}
}
