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

package openairesp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/dialect/openairesp"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/usage"
)

// server is a fake /v1/responses endpoint: it records each request body
// and answers with the next scripted reply.
type server struct {
	t       *testing.T
	mu      sync.Mutex
	bodies  []map[string]any
	replies []reply
	srv     *httptest.Server
}

type reply struct {
	status int
	sse    bool
	body   string
}

func newServer(t *testing.T, replies ...reply) *server {
	t.Helper()
	s := &server{t: t, replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if n >= len(s.replies) {
			http.Error(w, "script exhausted", http.StatusTeapot)
			return
		}
		rp := s.replies[n]
		if rp.sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		if rp.status != 0 {
			w.WriteHeader(rp.status)
		}
		_, _ = io.WriteString(w, rp.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		s.t.Fatalf("request %d was never sent (%d were)", i, len(s.bodies))
	}
	return s.bodies[i]
}

func client(t *testing.T, s *server, mod func(*openairesp.Options)) *openairesp.Client {
	t.Helper()
	p := retry.Default()
	p.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	opts := openairesp.Options{BaseURL: s.srv.URL + "/v1", Backend: "openai", Retry: &p}
	if mod != nil {
		mod(&opts)
	}
	c, err := openairesp.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func collect(t *testing.T, m llm.LLM, req *llm.Request, stream bool) ([]*llm.Response, error) {
	t.Helper()
	var out []*llm.Response
	for r, err := range m.GenerateContent(context.Background(), req, stream) {
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

func complete(t *testing.T, m llm.LLM, req *llm.Request) *llm.Response {
	t.Helper()
	out, err := collect(t, m, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("%d responses, want 1", len(out))
	}
	return out[0]
}

func userText(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleUser) }

// reasoningReply is OpenAI's documented shape for a reasoning model's
// tool call with include reasoning.encrypted_content: a reasoning item
// with a summary and encrypted content, then the function call.
const reasoningReply = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-2026","output":[
 {"type":"reasoning","id":"rs_abc","summary":[{"type":"summary_text","text":"Need the pod's status."}],"encrypted_content":"gAAAA-opaque"},
 {"type":"function_call","id":"fc_1","call_id":"call_xyz","name":"get_pod_status","arguments":"{\"pod\":\"web-7\"}","status":"completed"}],
 "usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":64},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":30},"total_tokens":170}}`

const textReply = `{"id":"resp_2","object":"response","status":"completed","model":"gpt-5-2026","output":[
 {"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"CrashLoopBackOff.","annotations":[]}]}],
 "usage":{"input_tokens":200,"output_tokens":5,"total_tokens":205}}`

// TestTheRequestShape pins what goes on the wire: instructions from the
// system prompt, input items in order, flat function tools, stateless
// with encrypted reasoning requested, sampling and limits.
func TestTheRequestShape(t *testing.T) {
	s := newServer(t, reply{body: textReply})
	temp, topP := float32(0.2), float32(0.9)
	complete(t, client(t, s, nil).Model("gpt-5"), &llm.Request{
		Contents: []*genai.Content{userText("why is web-7 down?")},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: userText("You are an SRE."),
			Temperature:       &temp, TopP: &topP, MaxOutputTokens: 512,
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "get_pod_status", Description: "pod status"}}}},
		},
	})
	b := s.body(0)
	if b["model"] != "gpt-5" || b["instructions"] != "You are an SRE." || b["store"] != false || b["max_output_tokens"] != float64(512) {
		t.Errorf("request = %v", b)
	}
	if inc, _ := b["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v", b["include"])
	}
	if b["temperature"] != 0.2 && b["temperature"] != float64(temp) {
		t.Errorf("temperature = %v", b["temperature"])
	}
	input := b["input"].([]any)
	msg := input[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "user" || msg["content"].([]any)[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("input[0] = %v", msg)
	}
	tl := b["tools"].([]any)[0].(map[string]any)
	if tl["type"] != "function" || tl["name"] != "get_pod_status" || tl["function"] != nil || tl["parameters"] == nil {
		t.Errorf("tool = %v; want the flat Responses shape", tl)
	}
	if _, set := b["tool_choice"]; set {
		t.Errorf("tool_choice = %v with no ToolConfig; want the server's default", b["tool_choice"])
	}
}

// TestEncryptedReasoningRoundTrips is §7.2's exit criterion offline: a
// reasoning item's encrypted content comes back as a thought part's
// ThoughtSignature, and the next request replays it as a reasoning item
// with its id, before the function call it led to.
func TestEncryptedReasoningRoundTrips(t *testing.T) {
	s := newServer(t, reply{body: reasoningReply}, reply{body: textReply})
	m := client(t, s, nil).Model("gpt-5")
	cfg := &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "get_pod_status"}}}}}
	history := []*genai.Content{userText("status of web-7?")}
	r := complete(t, m, &llm.Request{Contents: history, Config: cfg})

	parts := r.Content.Parts
	if len(parts) != 2 || !parts[0].Thought || parts[0].Text != "Need the pod's status." || len(parts[0].ThoughtSignature) == 0 {
		t.Fatalf("parts = %+v; want the summary as a thought carrying a signature, then the call", parts)
	}
	call := parts[1].FunctionCall
	if call == nil || call.ID != "call_xyz" || call.Args["pod"] != "web-7" {
		t.Fatalf("call = %+v", call)
	}

	history = append(history, r.Content, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"output": "CrashLoopBackOff"}},
	}}})
	complete(t, m, &llm.Request{Contents: history, Config: cfg})
	input := s.body(1)["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("replayed %d items, want user, reasoning, call, output: %v", len(input), input)
	}
	rs := input[1].(map[string]any)
	if rs["type"] != "reasoning" || rs["id"] != "rs_abc" || rs["encrypted_content"] != "gAAAA-opaque" {
		t.Errorf("reasoning item = %v; want it replayed with its id and encrypted content", rs)
	}
	if sum, _ := rs["summary"].([]any); len(sum) != 1 || sum[0].(map[string]any)["text"] != "Need the pod's status." {
		t.Errorf("summary = %v", rs["summary"])
	}
	fc, out := input[2].(map[string]any), input[3].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_xyz" || fc["name"] != "get_pod_status" {
		t.Errorf("function_call = %v", fc)
	}
	if out["type"] != "function_call_output" || out["call_id"] != "call_xyz" || !strings.Contains(out["output"].(string), "CrashLoopBackOff") {
		t.Errorf("function_call_output = %v", out)
	}
}

// A thought with no signature from this dialect — vLLM's plain
// reasoning, or another provider's — is not replayed: with store false,
// OpenAI accepts a reasoning item only with its encrypted content.
func TestReasoningWithoutASignatureIsNotReplayed(t *testing.T) {
	s := newServer(t, reply{body: textReply})
	complete(t, client(t, s, nil).Model("gpt-5"), &llm.Request{Contents: []*genai.Content{
		userText("hi"),
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Thought: true, Text: "plain reasoning"},
			{Thought: true, Text: "gemini's", ThoughtSignature: []byte("opaque-gemini-bytes")},
			{Text: "hello"},
		}},
		userText("again"),
	}})
	input := s.body(0)["input"].([]any)
	for _, it := range input {
		if it.(map[string]any)["type"] == "reasoning" {
			t.Errorf("replayed %v; a thought without this dialect's signature has nothing to replay", it)
		}
	}
	if len(input) != 3 || input[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("input = %v; want user, assistant text, user", input)
	}
}

// TestUsage maps usage, subtracting reasoning from candidates, and
// leaves counts the server did not send nil (AGENTS.md rule 11).
func TestUsage(t *testing.T) {
	s := newServer(t, reply{body: reasoningReply}, reply{body: textReply})
	m := client(t, s, nil).Model("gpt-5")
	r := complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}})
	u := r.UsageMetadata
	if u.PromptTokenCount != 120 || u.CandidatesTokenCount != 20 || u.ThoughtsTokenCount != 30 || u.CachedContentTokenCount != 64 || u.TotalTokenCount != 170 {
		t.Errorf("usage = %+v", u)
	}
	d, ok := usage.FromMetadata(r.CustomMetadata)
	if !ok || d.CacheReadTokens == nil || *d.CacheReadTokens != 64 || d.ReasoningTokens == nil || *d.ReasoningTokens != 30 || d.CacheWriteTokens != nil {
		t.Errorf("detail = %+v; want cache read 64, reasoning 30, cache write not reported", d)
	}
	if d.ServedModel != "gpt-5-2026" || d.ProviderRequestID != "resp_1" || d.Backend != "openai" || r.ModelVersion != "gpt-5" {
		t.Errorf("detail = %+v, ModelVersion %q", d, r.ModelVersion)
	}

	r = complete(t, m, &llm.Request{Contents: []*genai.Content{userText("y")}})
	d, _ = usage.FromMetadata(r.CustomMetadata)
	if d.CacheReadTokens != nil || d.ReasoningTokens != nil || d.CacheWriteTokens != nil {
		t.Errorf("detail = %+v; counts the server did not send must stay nil", d)
	}
}

// vLLM sends cache_write_tokens; it maps to CacheWriteTokens. vLLM's
// reasoning arrives as reasoning_text content, not a summary.
func TestVLLMShapes(t *testing.T) {
	s := newServer(t, reply{body: `{"id":"resp_v","status":"completed","model":"google/gemma-4-26B-A4B-it","output":[
	 {"type":"reasoning","id":"rs_v","summary":[],"content":[{"type":"reasoning_text","text":"17*3=51"}],"encrypted_content":null},
	 {"type":"message","id":"msg_v","role":"assistant","content":[{"type":"output_text","text":"51"}]}],
	 "usage":{"input_tokens":32,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":16},"output_tokens":144,"output_tokens_details":{"reasoning_tokens":139},"total_tokens":176}}`})
	r := complete(t, client(t, s, nil).Model("google/gemma-4-26B-A4B-it"), &llm.Request{Contents: []*genai.Content{userText("17*3?")}})
	p := r.Content.Parts
	if len(p) != 2 || !p[0].Thought || p[0].Text != "17*3=51" || p[0].ThoughtSignature != nil || p[1].Text != "51" {
		t.Errorf("parts = %+v", p)
	}
	d, _ := usage.FromMetadata(r.CustomMetadata)
	if d.CacheWriteTokens == nil || *d.CacheWriteTokens != 16 || d.CacheReadTokens == nil || *d.CacheReadTokens != 0 {
		t.Errorf("detail = %+v", d)
	}
}

func TestToolChoice(t *testing.T) {
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "a"}, {Name: "b"}}}}
	for name, tc := range map[string]struct {
		mode    genai.FunctionCallingConfigMode
		allowed []string
		noForce bool
		want    any
		noted   bool
	}{
		"auto":           {genai.FunctionCallingConfigModeAuto, nil, false, "auto", false},
		"none":           {genai.FunctionCallingConfigModeNone, nil, false, "none", false},
		"required":       {genai.FunctionCallingConfigModeAny, nil, false, "required", false},
		"named":          {genai.FunctionCallingConfigModeAny, []string{"b"}, false, map[string]any{"type": "function", "name": "b"}, false},
		"forced refused": {genai.FunctionCallingConfigModeAny, []string{"b"}, true, "auto", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, reply{body: textReply})
			m := client(t, s, func(o *openairesp.Options) { o.NoForcedToolChoice = tc.noForce }).Model("m")
			r := complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}, Config: &genai.GenerateContentConfig{
				Tools:      tools,
				ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: tc.mode, AllowedFunctionNames: tc.allowed}},
			}})
			got, _ := json.Marshal(s.body(0)["tool_choice"])
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Errorf("tool_choice = %s, want %s", got, want)
			}
			if noted := r.CustomMetadata[openairesp.ToolChoiceKey] == true; noted != tc.noted {
				t.Errorf("downgrade noted = %v, want %v", noted, tc.noted)
			}
		})
	}
}

func TestStructuredOutputIsGated(t *testing.T) {
	cfg := &genai.GenerateContentConfig{ResponseJsonSchema: map[string]any{"type": "object"}}
	s := newServer(t, reply{body: textReply})
	if _, err := collect(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}, Config: cfg}, false); err == nil {
		t.Error("a schema was sent to a profile without capabilities.response_schema")
	}
	complete(t, client(t, s, func(o *openairesp.Options) { o.ResponseSchema = true }).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}, Config: cfg})
	format := s.body(0)["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil || format["name"] == "" {
		t.Errorf("text.format = %v", format)
	}
}

func TestReasoningEffort(t *testing.T) {
	zero := int32(0)
	for name, tc := range map[string]struct {
		tc   *genai.ThinkingConfig
		want any
	}{
		"level":           {&genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}, map[string]any{"effort": "high"}},
		"zero budget":     {&genai.ThinkingConfig{ThinkingBudget: &zero}, map[string]any{"effort": "minimal"}},
		"summaries":       {&genai.ThinkingConfig{IncludeThoughts: true}, map[string]any{"summary": "auto"}},
		"positive budget": {&genai.ThinkingConfig{ThinkingBudget: new(int32(4096))}, nil},
		"none":            {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, reply{body: textReply})
			complete(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}, Config: &genai.GenerateContentConfig{ThinkingConfig: tc.tc}})
			got, _ := json.Marshal(s.body(0)["reasoning"])
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Errorf("reasoning = %s, want %s", got, want)
			}
		})
	}
}

// Fields the Responses API does not have are refused, not dropped.
func TestUnsupportedSettingsAreRefused(t *testing.T) {
	seed := int32(1)
	pen := float32(0.5)
	for name, cfg := range map[string]*genai.GenerateContentConfig{
		"stop":    {StopSequences: []string{"x"}},
		"seed":    {Seed: &seed},
		"penalty": {PresencePenalty: &pen},
		"builtin": {Tools: []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			if _, err := collect(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}, Config: cfg}, false); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestFinishReasons(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want genai.FinishReason
		raw  string
	}{
		"truncated": {`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, genai.FinishReasonMaxTokens, ""},
		"filtered":  {`{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, genai.FinishReasonSafety, ""},
		"other":     {`{"id":"r","status":"incomplete","incomplete_details":{"reason":"something_new"},"output":[]}`, genai.FinishReasonOther, "incomplete: something_new"},
	} {
		t.Run(name, func(t *testing.T) {
			r := complete(t, client(t, newServer(t, reply{body: tc.body}), nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}})
			if r.FinishReason != tc.want || (tc.raw != "" && r.CustomMetadata[openairesp.FinishReasonKey] != tc.raw) {
				t.Errorf("finish = %s (%v), want %s", r.FinishReason, r.CustomMetadata[openairesp.FinishReasonKey], tc.want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	s := newServer(t, reply{status: 404, body: `{"error":{"message":"The model ` + "`nope`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`})
	_, err := collect(t, client(t, s, nil).Model("nope"), &llm.Request{Contents: []*genai.Content{userText("x")}}, false)
	var apiErr *openairesp.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != 404 || !strings.Contains(apiErr.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}

	s = newServer(t, reply{body: `{"id":"r","status":"failed","error":{"code":"server_error","message":"boom"},"output":[]}`})
	_, err = collect(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, false)
	if !errors.As(err, &apiErr) || apiErr.Code != "server_error" || apiErr.HTTPStatus() != 0 {
		t.Errorf("failed response: err = %v", err)
	}
}

// streamBody is a Responses stream: reasoning and text deltas, then
// response.completed carrying the whole response.
const streamBody = `event: response.created
data: {"type":"response.created","response":{"id":"resp_s","status":"in_progress","output":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"think"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"1, 2, "}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"3"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_s","status":"completed","model":"gpt-5-2026","output":[{"type":"reasoning","id":"rs_s","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"enc"},{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"1, 2, 3"}]}],"usage":{"input_tokens":10,"output_tokens":6,"total_tokens":16}}}

`

// TestStreaming: partials for the text and reasoning deltas only, then
// exactly one TurnComplete — the final response decoded from
// response.completed, as a non-streamed call is (llm.Response).
func TestStreaming(t *testing.T) {
	s := newServer(t, reply{sse: true, body: streamBody})
	out, err := collect(t, client(t, s, nil).Model("gpt-5"), &llm.Request{Contents: []*genai.Content{userText("count")}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.body(0)["stream"] != true {
		t.Error("stream not requested")
	}
	var partials, completes int
	for _, r := range out[:len(out)-1] {
		if !r.Partial || r.TurnComplete {
			t.Errorf("chunk partial=%v turnComplete=%v", r.Partial, r.TurnComplete)
		}
		partials++
	}
	final := out[len(out)-1]
	for _, r := range out {
		if r.TurnComplete {
			completes++
		}
	}
	if partials != 3 || completes != 1 || final.Partial {
		t.Errorf("%d partials, %d TurnComplete, final partial=%v; want 3, 1, false", partials, completes, final.Partial)
	}
	if len(final.Content.Parts) != 2 || final.Content.Parts[1].Text != "1, 2, 3" || final.Content.Parts[0].ThoughtSignature == nil || final.UsageMetadata.PromptTokenCount != 10 {
		t.Errorf("final = %+v", final.Content.Parts)
	}
}

func TestStreamErrors(t *testing.T) {
	for name, body := range map[string]string{
		"error event":   "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}\n\n",
		"failed":        "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"},\"output\":[]}}\n\n",
		"no completion": "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, reply{sse: true, body: body})
			if _, err := collect(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, true); err == nil {
				t.Error("no error")
			}
		})
	}
}

// ExtraBody adds vendor fields and may not override the adapter's.
func TestExtraBody(t *testing.T) {
	s := newServer(t, reply{body: textReply})
	m := client(t, s, nil).ModelWith("m", openairesp.ModelOptions{ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}})
	complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}})
	if s.body(0)["chat_template_kwargs"] == nil {
		t.Error("extra_body field not sent")
	}
	bad := client(t, s, nil).ModelWith("m", openairesp.ModelOptions{ExtraBody: map[string]any{"store": true}})
	if _, err := collect(t, bad, &llm.Request{Contents: []*genai.Content{userText("x")}}, false); err == nil {
		t.Error("extra_body overrode store")
	}
}
