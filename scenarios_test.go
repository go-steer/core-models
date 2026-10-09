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

package coremodels_test

// The scenarios the live smoke runs against a real server and the
// conformance replay runs against that server's recording. One
// definition, so a recording is always replayed by exactly the code
// that made it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/usage"
)

// exchange is one recorded HTTP request and response.
type exchange struct {
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	RequestBody  json.RawMessage `json:"request_body"`
	Status       int             `json:"status"`
	ContentType  string          `json:"content_type"`
	ResponseBody string          `json:"response_body"`
}

type scenario struct {
	name string
	run  func(t *testing.T, m llm.LLM)
}

var podTools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
	Name: "get_pod_status", Description: "Return the status of a Kubernetes pod.",
	ParametersJsonSchema: map[string]any{
		"type": "object", "required": []any{"pod"},
		"properties": map[string]any{
			"pod":       map[string]any{"type": "string", "description": "pod name"},
			"namespace": map[string]any{"type": "string", "description": "namespace, default 'default'"},
		},
	},
}}}}

var scenarios = []scenario{
	{"text", func(t *testing.T, m llm.LLM) {
		r := run(t, m, false, &llm.Request{Contents: []*genai.Content{genai.NewContentFromText("Reply with exactly the word: pong", genai.RoleUser)}})
		report(t, r)
		if got := strings.ToLower(text(r)); !strings.Contains(got, "pong") || strings.Contains(got, "<think>") {
			t.Errorf("answer %q: want pong, with no reasoning in it", text(r))
		}
		assertUsage(t, r)
	}},
	{"tool_round_trip", func(t *testing.T, m llm.LLM) {
		cfg := &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("You are an SRE assistant. Use tools to answer.", genai.RoleUser),
			Tools:             podTools,
		}
		history := []*genai.Content{genai.NewContentFromText("What is the status of pod web-7 in namespace shop? Use the tool.", genai.RoleUser)}
		r := run(t, m, false, &llm.Request{Contents: history, Config: cfg})
		report(t, r)
		var call *genai.FunctionCall
		for _, p := range r.Content.Parts {
			if p.FunctionCall != nil {
				call = p.FunctionCall
			}
		}
		if call == nil {
			t.Fatalf("no tool call; answer was %q", text(r))
		}
		if call.Name != "get_pod_status" || call.Args["pod"] != "web-7" || call.ID == "" {
			t.Fatalf("call = %+v", call)
		}
		history = append(history, r.Content, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"output": "CrashLoopBackOff, 14 restarts"}},
		}}})
		r = run(t, m, false, &llm.Request{Contents: history, Config: cfg})
		report(t, r)
		if !strings.Contains(strings.ToLower(text(r)), "crash") {
			t.Errorf("final answer %q does not use the tool result", text(r))
		}
		assertUsage(t, r)
	}},
	{"stream", func(t *testing.T, m llm.LLM) {
		r := run(t, m, true, &llm.Request{Contents: []*genai.Content{genai.NewContentFromText("Count from 1 to 5, comma separated.", genai.RoleUser)}})
		report(t, r)
		if !strings.Contains(text(r), "3") || strings.Contains(text(r), "<think>") {
			t.Errorf("answer %q", text(r))
		}
		assertUsage(t, r)
	}},
}

func run(t *testing.T, m llm.LLM, stream bool, req *llm.Request) *llm.Response {
	t.Helper()
	var final *llm.Response
	partials := 0
	for r, err := range m.GenerateContent(context.Background(), req, stream) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if r.Partial {
			partials++
			continue
		}
		final = r
	}
	if stream && partials == 0 {
		t.Error("a streamed call yielded no partial responses")
	}
	if final == nil {
		t.Fatal("no final response")
	}
	return final
}

// assertUsage is the R3 floor every server meets: input and output
// tokens, and a Detail naming what served the call.
func assertUsage(t *testing.T, r *llm.Response) {
	t.Helper()
	if u := r.UsageMetadata; u == nil || u.PromptTokenCount == 0 || u.CandidatesTokenCount == 0 {
		t.Errorf("usage = %+v: every server must report input and output tokens", r.UsageMetadata)
	}
	d, ok := usage.FromMetadata(r.CustomMetadata)
	if !ok || d.ServedModel == "" || d.ProviderRequestID == "" || d.Backend == "" {
		t.Errorf("detail = %+v", d)
	}
}

func text(r *llm.Response) string {
	var b strings.Builder
	for _, p := range r.Content.Parts {
		if !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func report(t *testing.T, r *llm.Response) {
	t.Helper()
	d, _ := usage.FromMetadata(r.CustomMetadata)
	u := r.UsageMetadata
	us := "not reported"
	if u != nil {
		us = fmt.Sprintf("prompt=%d candidates=%d thoughts=%d total=%d", u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, u.TotalTokenCount)
	}
	show := func(p *int64) string {
		if p == nil {
			return "not reported"
		}
		return fmt.Sprint(*p)
	}
	t.Logf("finish=%s model_version=%q served=%q usage[%s] detail[cache_read=%s reasoning=%s]",
		r.FinishReason, r.ModelVersion, d.ServedModel, us, show(d.CacheReadTokens), show(d.ReasoningTokens))
	for _, p := range r.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			t.Logf("  call %s(%v) id=%s", p.FunctionCall.Name, p.FunctionCall.Args, p.FunctionCall.ID)
		case p.Thought:
			t.Logf("  thought: %.120q", p.Text)
		default:
			t.Logf("  text: %.200q", p.Text)
		}
	}
}
