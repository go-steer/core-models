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

package openaichat_test

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

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/dialect/openaichat"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/usage"
)

// server is a fake chat-completions endpoint: it records each request
// body and answers with the next scripted reply.
type server struct {
	t       *testing.T
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	replies []reply
	srv     *httptest.Server
}

type reply struct {
	status int
	sse    bool
	body   string
	header map[string]string
}

func newServer(t *testing.T, replies ...reply) *server {
	t.Helper()
	s := &server{t: t, replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if n >= len(s.replies) {
			http.Error(w, "script exhausted", http.StatusTeapot)
			return
		}
		rp := s.replies[n]
		for k, v := range rp.header {
			w.Header().Set(k, v)
		}
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

func (s *server) url() string { return s.srv.URL + "/v1" }

func (s *server) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		s.t.Fatalf("request %d was never sent (%d were)", i, len(s.bodies))
	}
	return s.bodies[i]
}

func client(t *testing.T, s *server, mod func(*openaichat.Options)) *openaichat.Client {
	t.Helper()
	p := retry.Default()
	p.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	opts := openaichat.Options{BaseURL: s.url(), Backend: "house-vllm", Region: "lab", Retry: &p}
	if mod != nil {
		mod(&opts)
	}
	c, err := openaichat.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func complete(t *testing.T, m llm.LLM, req *llm.Request) *llm.Response {
	t.Helper()
	var out *llm.Response
	for r, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		out = r
	}
	return out
}

func generateErr(m llm.LLM, req *llm.Request, stream bool) error {
	for _, err := range m.GenerateContent(context.Background(), req, stream) {
		if err != nil {
			return err
		}
	}
	return nil
}

const okReply = `{"id":"chatcmpl-1","model":"served-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

func userText(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleUser) }

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- requests ----

func TestConversationBecomesMessages(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	m := client(t, s, nil).Model("Qwen/Qwen3-Coder-Next")
	complete(t, m, &llm.Request{
		Config: &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("be terse", genai.RoleUser)},
		Contents: []*genai.Content{
			userText("list the pods"),
			{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "I should list them.", Thought: true},
				{Text: "Listing."},
				{FunctionCall: &genai.FunctionCall{ID: "call_a", Name: "kubectl", Args: map[string]any{"verb": "get"}}},
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "x"}}}, // no id: Gemini-era history
			}},
			{Role: genai.RoleUser, Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "call_a", Name: "kubectl", Response: map[string]any{"output": "pod-1"}}},
				{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{"output": "contents"}}},
				{Text: "and now?"},
			}},
		},
	})
	b := s.body(0)
	if b["model"] != "Qwen/Qwen3-Coder-Next" {
		t.Errorf("model = %v", b["model"])
	}
	msgs := b["messages"].([]any)
	roles := make([]string, len(msgs))
	for i, mm := range msgs {
		roles[i] = mm.(map[string]any)["role"].(string)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,tool,user" {
		t.Fatalf("roles = %s", got)
	}
	asst := msgs[2].(map[string]any)
	if asst["content"] != "Listing." {
		t.Errorf("assistant content = %v", asst["content"])
	}
	if _, echoed := asst["reasoning_content"]; echoed {
		t.Error("reasoning was echoed without capabilities.reasoning_echo")
	}
	calls := asst["tool_calls"].([]any)
	first, second := calls[0].(map[string]any), calls[1].(map[string]any)
	if first["id"] != "call_a" {
		t.Errorf("first call id = %v, want the one in history", first["id"])
	}
	genID, _ := second["id"].(string)
	if !strings.HasPrefix(genID, "call_") || genID == "call_a" {
		t.Errorf("second call id = %q, want a generated one", genID)
	}
	if fn := first["function"].(map[string]any); fn["arguments"] != `{"verb":"get"}` || fn["name"] != "kubectl" {
		t.Errorf("first call function = %v", fn)
	}
	t1, t2 := msgs[3].(map[string]any), msgs[4].(map[string]any)
	if t1["tool_call_id"] != "call_a" || t2["tool_call_id"] != genID {
		t.Errorf("tool_call_ids = %v, %v; want call_a and the generated %s", t1["tool_call_id"], t2["tool_call_id"], genID)
	}
	if t1["content"] != `{"output":"pod-1"}` {
		t.Errorf("tool content = %v", t1["content"])
	}
	if msgs[5].(map[string]any)["content"] != "and now?" {
		t.Errorf("trailing user text = %v", msgs[5])
	}
}

func TestReasoningIsEchoedWhenTheProfileSaysSo(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	m := client(t, s, func(o *openaichat.Options) { o.ReasoningEcho = true }).Model("deepseek")
	complete(t, m, &llm.Request{Contents: []*genai.Content{
		userText("q"),
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Text: "thinking", Thought: true},
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "f"}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "f", Response: map[string]any{}}}}},
	}})
	asst := s.body(0)["messages"].([]any)[1].(map[string]any)
	if asst["reasoning_content"] != "thinking" {
		t.Errorf("reasoning_content = %v", asst["reasoning_content"])
	}
	if c, present := asst["content"]; !present || c != nil {
		t.Errorf("content = %v (present %v), want an explicit null on a tool-call-only turn", c, present)
	}
	if fn := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any); fn["arguments"] != "{}" {
		t.Errorf("nil args sent as %v, want {}", fn["arguments"])
	}
}

func TestConfigMapping(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	m := client(t, s, nil).Model("m")
	temp, topP, pres, freq := float32(0.25), float32(0.5), float32(0.1), float32(0.2)
	seed := int32(7)
	complete(t, m, &llm.Request{
		Contents: []*genai.Content{userText("hi")},
		Config: &genai.GenerateContentConfig{
			Temperature: &temp, TopP: &topP, PresencePenalty: &pres, FrequencyPenalty: &freq,
			Seed: &seed, MaxOutputTokens: 512, StopSequences: []string{"END"},
			ResponseMIMEType: "application/json",
			ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"kubectl"},
			}},
		},
	})
	b := s.body(0)
	for k, want := range map[string]any{
		"temperature": 0.25, "top_p": 0.5, "seed": 7.0, "max_tokens": 512.0,
	} {
		if b[k] != want {
			t.Errorf("%s = %v, want %v", k, b[k], want)
		}
	}
	if asJSON(t, b["stop"]) != `["END"]` {
		t.Errorf("stop = %v", b["stop"])
	}
	if asJSON(t, b["tool_choice"]) != `{"function":{"name":"kubectl"},"type":"function"}` {
		t.Errorf("tool_choice = %v", asJSON(t, b["tool_choice"]))
	}
	if asJSON(t, b["response_format"]) != `{"type":"json_object"}` {
		t.Errorf("response_format = %v", asJSON(t, b["response_format"]))
	}
	if _, set := b["stream"]; set {
		t.Error("stream set on a non-streaming request")
	}
}

func TestToolChoiceModes(t *testing.T) {
	for mode, want := range map[genai.FunctionCallingConfigMode]string{
		genai.FunctionCallingConfigModeAuto: `"auto"`,
		genai.FunctionCallingConfigModeNone: `"none"`,
		genai.FunctionCallingConfigModeAny:  `"required"`,
	} {
		s := newServer(t, reply{body: okReply})
		complete(t, client(t, s, nil).Model("m"), &llm.Request{
			Contents: []*genai.Content{userText("hi")},
			Config: &genai.GenerateContentConfig{ToolConfig: &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: mode},
			}},
		})
		if got := asJSON(t, s.body(0)["tool_choice"]); got != want {
			t.Errorf("%s: tool_choice = %s, want %s", mode, got, want)
		}
	}
}

func TestResponseSchemaNeedsTheCapability(t *testing.T) {
	schema := &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"verdict": {Type: genai.TypeString}}}
	req := &llm.Request{
		Contents: []*genai.Content{userText("judge")},
		Config:   &genai.GenerateContentConfig{ResponseMIMEType: "application/json", ResponseSchema: schema},
	}
	s := newServer(t, reply{body: okReply})
	err := generateErr(client(t, s, nil).Model("m"), req, false)
	if err == nil || !strings.Contains(err.Error(), "capabilities.response_schema") {
		t.Fatalf("err = %v, want a refusal naming the capability", err)
	}
	if len(s.bodies) != 0 {
		t.Error("a refused request reached the server")
	}

	s = newServer(t, reply{body: okReply})
	complete(t, client(t, s, func(o *openaichat.Options) { o.ResponseSchema = true }).Model("m"), req)
	want := `{"json_schema":{"name":"response","schema":{"properties":{"verdict":{"type":"string"}},"type":"object"}},"type":"json_schema"}`
	if got := asJSON(t, s.body(0)["response_format"]); got != want {
		t.Errorf("response_format = %s\nwant %s", got, want)
	}
}

func TestWhatTheDialectRefuses(t *testing.T) {
	for name, req := range map[string]*llm.Request{
		"a provider built-in tool": {
			Contents: []*genai.Content{userText("hi")},
			Config:   &genai.GenerateContentConfig{Tools: []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}}},
		},
		"two candidates": {
			Contents: []*genai.Content{userText("hi")},
			Config:   &genai.GenerateContentConfig{CandidateCount: 2},
		},
		"audio":                  {Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "audio/wav", Data: []byte{1}}}}}}},
		"code execution history": {Contents: []*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{{ExecutableCode: &genai.ExecutableCode{Code: "1"}}}}}},
		"unknown role":           {Contents: []*genai.Content{{Role: "system", Parts: []*genai.Part{{Text: "x"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, reply{body: okReply})
			if err := generateErr(client(t, s, nil).Model("m"), req, false); err == nil {
				t.Fatal("sent; want a refusal")
			}
			if len(s.bodies) != 0 {
				t.Error("a refused request reached the server")
			}
		})
	}
}

func TestImagesBecomeImageParts(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	complete(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{
		{Text: "what is this?"},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("png")}},
	}}}})
	got := asJSON(t, s.body(0)["messages"].([]any)[0].(map[string]any)["content"])
	want := `[{"text":"what is this?","type":"text"},{"image_url":{"url":"data:image/png;base64,cG5n"},"type":"image_url"}]`
	if got != want {
		t.Errorf("content = %s\nwant %s", got, want)
	}
}

func TestTypedSchemaBecomesJSONSchema(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	nullable := true
	minItems := int64(1)
	complete(t, client(t, s, nil).Model("m"), &llm.Request{
		Contents: []*genai.Content{userText("hi")},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name: "search",
			Parameters: &genai.Schema{
				Type:     genai.TypeObject,
				Required: []string{"query"},
				Properties: map[string]*genai.Schema{
					"query": {Type: genai.TypeString, Description: "what to find"},
					"tags":  {Type: genai.TypeArray, MinItems: &minItems, Items: &genai.Schema{Type: genai.TypeString, Enum: []string{"a", "b"}}},
					"since": {Type: genai.TypeString, Format: "date-time", Nullable: &nullable},
				},
			},
		}, {Name: "now"}}}}},
	})
	tools := s.body(0)["tools"].([]any)
	got := asJSON(t, tools[0].(map[string]any)["function"].(map[string]any)["parameters"])
	want := `{"properties":{"query":{"description":"what to find","type":"string"},"since":{"format":"date-time","type":["string","null"]},"tags":{"items":{"enum":["a","b"],"type":"string"},"minItems":1,"type":"array"}},"required":["query"],"type":"object"}`
	if got != want {
		t.Errorf("parameters = %s\nwant %s", got, want)
	}
	if got := asJSON(t, tools[1].(map[string]any)["function"].(map[string]any)["parameters"]); got != `{"properties":{},"type":"object"}` {
		t.Errorf("a no-argument tool sent %s", got)
	}
}

// ---- responses ----

func TestACompleteResponse(t *testing.T) {
	s := newServer(t, reply{body: `{
		"id": "chatcmpl-9", "model": "/models/qwen3-coder",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
			"role": "assistant", "content": "Checking.", "reasoning_content": "they want pods",
			"tool_calls": [{"id": "call_x", "type": "function", "function": {"name": "kubectl", "arguments": "{\"verb\":\"get\",\"n\":2}"}}]
		}}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130,
		          "prompt_tokens_details": {"cached_tokens": 64}, "completion_tokens_details": {"reasoning_tokens": 12}}
	}`})
	r := complete(t, client(t, s, nil).Model("Qwen/Qwen3-Coder-Next"), &llm.Request{Contents: []*genai.Content{userText("pods?")}})

	parts := r.Content.Parts
	if len(parts) != 3 || !parts[0].Thought || parts[0].Text != "they want pods" || parts[1].Text != "Checking." {
		t.Fatalf("parts = %+v", parts)
	}
	fc := parts[2].FunctionCall
	if fc == nil || fc.ID != "call_x" || fc.Name != "kubectl" || fc.Args["verb"] != "get" || fc.Args["n"] != 2.0 {
		t.Errorf("function call = %+v", fc)
	}
	if r.ModelVersion != "Qwen/Qwen3-Coder-Next" {
		t.Errorf("ModelVersion = %q, want the requested id, never the served weights path", r.ModelVersion)
	}
	if r.FinishReason != genai.FinishReasonStop {
		t.Errorf("FinishReason = %v", r.FinishReason)
	}
	u := r.UsageMetadata
	if u.PromptTokenCount != 100 || u.CandidatesTokenCount != 18 || u.ThoughtsTokenCount != 12 || u.TotalTokenCount != 130 || u.CachedContentTokenCount != 64 {
		t.Errorf("usage = %+v (candidates must exclude the 12 reasoning tokens)", u)
	}
	d, ok := usage.FromMetadata(r.CustomMetadata)
	if !ok {
		t.Fatal("no usage.Detail")
	}
	if *d.CacheReadTokens != 64 || *d.ReasoningTokens != 12 || d.CacheWriteTokens != nil {
		t.Errorf("detail counts = read %v, reasoning %v, write %v", d.CacheReadTokens, d.ReasoningTokens, d.CacheWriteTokens)
	}
	if d.ServedModel != "/models/qwen3-coder" || d.ProviderRequestID != "chatcmpl-9" || d.Backend != "house-vllm" || d.Region != "lab" {
		t.Errorf("detail identity = %+v", d)
	}
}

func TestUsageThatIsNotReportedStaysUnreported(t *testing.T) {
	for name, tc := range map[string]struct {
		usage      string
		unreliable bool
		wantMeta   bool
	}{
		"no usage at all":           {"", false, false},
		"no details":                {`,"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`, false, true},
		"details null":              {`,"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":null}`, false, true},
		"cached flagged unreliable": {`,"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}`, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"x"},"finish_reason":"stop"}]` + tc.usage + `}`})
			r := complete(t, client(t, s, func(o *openaichat.Options) { o.CachedTokensUnreliable = tc.unreliable }).Model("m"),
				&llm.Request{Contents: []*genai.Content{userText("hi")}})
			if (r.UsageMetadata != nil) != tc.wantMeta {
				t.Errorf("UsageMetadata = %+v", r.UsageMetadata)
			}
			if tc.wantMeta && r.UsageMetadata.TotalTokenCount != 7 {
				t.Errorf("total = %d, want prompt+completion when total is missing", r.UsageMetadata.TotalTokenCount)
			}
			d, _ := usage.FromMetadata(r.CustomMetadata)
			if d.CacheReadTokens != nil || d.ReasoningTokens != nil {
				t.Errorf("an unreported count became %v / %v", d.CacheReadTokens, d.ReasoningTokens)
			}
		})
	}
}

func TestAReportedZeroIsKept(t *testing.T) {
	s := newServer(t, reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}`})
	r := complete(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	d, _ := usage.FromMetadata(r.CustomMetadata)
	if d.CacheReadTokens == nil || *d.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %v, want a reported zero", d.CacheReadTokens)
	}
}

func TestFinishReasons(t *testing.T) {
	for in, want := range map[string]genai.FinishReason{
		"stop": genai.FinishReasonStop, "tool_calls": genai.FinishReasonStop, "length": genai.FinishReasonMaxTokens,
		"content_filter": genai.FinishReasonSafety, "eos_token": genai.FinishReasonOther,
	} {
		s := newServer(t, reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"x"},"finish_reason":"` + in + `"}]}`})
		r := complete(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
		if r.FinishReason != want {
			t.Errorf("%s -> %v, want %v", in, r.FinishReason, want)
		}
		if want == genai.FinishReasonOther && r.CustomMetadata[openaichat.FinishReasonKey] != in {
			t.Errorf("%s: the raw reason was not kept", in)
		}
	}
}

func TestRefusalAndMissingIDs(t *testing.T) {
	s := newServer(t,
		reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`},
		reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"tool_calls":[{"type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":"tool_calls"}]}`},
	)
	m := client(t, s, nil).Model("m")
	r := complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}})
	if r.Content.Parts[0].Text != "I can't help with that." || r.CustomMetadata[openaichat.RefusalKey] != true {
		t.Errorf("refusal = %+v, %v", r.Content.Parts, r.CustomMetadata)
	}
	r = complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}})
	fc := r.Content.Parts[0].FunctionCall
	if fc == nil || !strings.HasPrefix(fc.ID, "call_") || len(fc.Args) != 0 {
		t.Errorf("a call with no id and empty arguments became %+v", fc)
	}
}

func TestMalformedToolArgumentsAreAnError(t *testing.T) {
	s := newServer(t, reply{body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"tool_calls":[{"id":"1","type":"function","function":{"name":"kubectl","arguments":"{\"verb\": get}"}}]},"finish_reason":"tool_calls"}]}`})
	err := generateErr(client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, false)
	if err == nil || !strings.Contains(err.Error(), `"kubectl"`) {
		t.Errorf("err = %v, want the tool named", err)
	}
}

func TestHTTPErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		rp   reply
		want openaichat.APIError
	}{
		"openai envelope": {reply{status: 400, body: `{"error":{"message":"bad tool","type":"invalid_request_error","code":"bad_tool"}}`},
			openaichat.APIError{StatusCode: 400, Type: "invalid_request_error", Code: "bad_tool", Message: "bad tool"}},
		"top-level message": {reply{status: 404, body: `{"object":"error","message":"The model x does not exist."}`},
			openaichat.APIError{StatusCode: 404, Message: "The model x does not exist."}},
		"plain text": {reply{status: 401, body: "unauthorized\n"},
			openaichat.APIError{StatusCode: 401, Message: "unauthorized"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, tc.rp)
			err := generateErr(client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, false)
			var ae *openaichat.APIError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if *ae != tc.want {
				t.Errorf("APIError = %+v\nwant       %+v", *ae, tc.want)
			}
			if ae.HTTPStatus() != tc.want.StatusCode {
				t.Errorf("HTTPStatus = %d", ae.HTTPStatus())
			}
		})
	}
}

func TestARetryIsStampedOnTheResponse(t *testing.T) {
	s := newServer(t,
		reply{status: 429, body: `{"error":{"message":"slow down"}}`, header: map[string]string{"retry-after-ms": "10"}},
		reply{body: okReply},
	)
	r := complete(t, client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}})
	snap, ok := r.CustomMetadata[retry.MetadataKey].(retry.Snapshot)
	if !ok || snap.Retries != 1 || snap.LastStatus != 429 {
		t.Errorf("retry stamp = %+v", r.CustomMetadata[retry.MetadataKey])
	}
}

func TestTheCredentialIsSentAsABearerToken(t *testing.T) {
	s := newServer(t, reply{body: okReply})
	cred, err := auth.Config{Kind: auth.APIKey, Env: "K"}.Resolve(context.Background(), auth.Options{Getenv: func(string) string { return "sk-test" }})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, client(t, s, func(o *openaichat.Options) { o.Credential = cred }).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}})
	if got := s.headers[0].Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
}

// ---- streaming ----

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestAStreamAccumulatesIntoOneFinalResponse(t *testing.T) {
	s := newServer(t, reply{sse: true, body: ": keep-alive\n\n" + sse(
		`{"id":"s1","model":"served","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"reasoning":"hmm "}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"reasoning":"ok"}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"content":"Let me "}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"content":"check.","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"kubectl","arguments":""}}]}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"verb\":"}}]}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"get\"}"}}]}}]}`,
		`{"id":"s1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"s1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29}}`,
	)})
	m := client(t, s, nil).Model("m")
	var partials []string
	var final *llm.Response
	for r, err := range m.GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{userText("x")}}, true) {
		if err != nil {
			t.Fatal(err)
		}
		if r.Partial {
			p := r.Content.Parts[0]
			if p.Thought {
				partials = append(partials, "~"+p.Text)
			} else {
				partials = append(partials, p.Text)
			}
			continue
		}
		final = r
	}
	if got := strings.Join(partials, "|"); got != "~hmm |~ok|Let me |check." {
		t.Errorf("partials = %q", got)
	}
	if b := s.body(0); b["stream"] != true || asJSON(t, b["stream_options"]) != `{"include_usage":true}` {
		t.Errorf("stream request = %v / %v", b["stream"], b["stream_options"])
	}
	if final == nil || !final.TurnComplete {
		t.Fatalf("final = %+v", final)
	}
	parts := final.Content.Parts
	if len(parts) != 4 || parts[0].Text != "hmm ok" || !parts[0].Thought || parts[1].Text != "Let me check." {
		t.Fatalf("final parts = %+v", parts)
	}
	c1, c2 := parts[2].FunctionCall, parts[3].FunctionCall
	if c1.ID != "call_1" || c1.Args["verb"] != "get" || c2.ID != "call_2" || c2.Name != "read_file" {
		t.Errorf("tool calls = %+v, %+v", c1, c2)
	}
	if final.UsageMetadata == nil || final.UsageMetadata.TotalTokenCount != 29 || final.FinishReason != genai.FinishReasonStop {
		t.Errorf("usage %+v, finish %v", final.UsageMetadata, final.FinishReason)
	}
	if d, _ := usage.FromMetadata(final.CustomMetadata); d.ServedModel != "served" || d.ProviderRequestID != "s1" {
		t.Errorf("detail = %+v", d)
	}
}

func TestAnErrorInsideAStream(t *testing.T) {
	s := newServer(t, reply{sse: true, body: sse(
		`{"id":"s","choices":[{"index":0,"delta":{"content":"par"}}]}`,
		`{"error":{"message":"out of memory","type":"server_error"}}`,
	)})
	err := generateErr(client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, true)
	var ae *openaichat.APIError
	if !errors.As(err, &ae) || ae.Message != "out of memory" || ae.StatusCode != 0 {
		t.Errorf("err = %v", err)
	}
}

func TestAStreamStopsWhenTheConsumerDoes(t *testing.T) {
	s := newServer(t, reply{sse: true, body: sse(
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"b"}}]}`,
	)})
	n := 0
	for range client(t, s, nil).Model("m").GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{userText("x")}}, true) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("yielded %d after the consumer stopped", n)
	}
}

func TestThinkTagsAreSplitWhenTheProfileSaysSo(t *testing.T) {
	body := `{"id":"c","model":"qwen3:1.7b","choices":[{"index":0,"message":{"content":"<think>\nthey want pong\n</think>\n\npong"},"finish_reason":"stop"}]}`
	req := &llm.Request{Contents: []*genai.Content{userText("ping")}}

	s := newServer(t, reply{body: body})
	r := complete(t, client(t, s, func(o *openaichat.Options) { o.ThinkTags = true }).Model("qwen3:1.7b"), req)
	if p := r.Content.Parts; len(p) != 2 || !p[0].Thought || p[0].Text != "\nthey want pong\n" || p[1].Text != "pong" {
		t.Errorf("parts = %+v", p)
	}

	s = newServer(t, reply{body: body})
	r = complete(t, client(t, s, nil).Model("qwen3:1.7b"), req)
	if p := r.Content.Parts; len(p) != 1 || p[0].Thought {
		t.Errorf("without think_tags the content must pass through untouched: %+v", p)
	}
}

func TestThinkTagsInAStream(t *testing.T) {
	s := newServer(t, reply{sse: true, body: sse(
		`{"id":"s","choices":[{"index":0,"delta":{"content":"<think>"}}]}`,
		`{"id":"s","choices":[{"index":0,"delta":{"content":"\nhmm"}}]}`,
		`{"id":"s","choices":[{"index":0,"delta":{"content":"</thi"}}]}`,
		`{"id":"s","choices":[{"index":0,"delta":{"content":"nk>\n\n"}}]}`,
		`{"id":"s","choices":[{"index":0,"delta":{"content":"1, 2"}}]}`,
		`{"id":"s","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)})
	var thoughts, answer string
	var final *llm.Response
	for r, err := range client(t, s, func(o *openaichat.Options) { o.ThinkTags = true }).Model("m").GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{userText("x")}}, true) {
		if err != nil {
			t.Fatal(err)
		}
		if !r.Partial {
			final = r
			continue
		}
		for _, p := range r.Content.Parts {
			if p.Thought {
				thoughts += p.Text
			} else {
				answer += p.Text
			}
		}
	}
	if thoughts != "\nhmm" || answer != "1, 2" {
		t.Errorf("partials: thoughts %q answer %q", thoughts, answer)
	}
	if p := final.Content.Parts; len(p) != 2 || p[0].Text != "\nhmm" || p[1].Text != "1, 2" {
		t.Errorf("final parts = %+v", p)
	}
}

func TestToolChoiceIsAutoWhenToolsAreOfferedAndNothingIsAsked(t *testing.T) {
	s := newServer(t, reply{body: okReply}, reply{body: okReply})
	m := client(t, s, nil).Model("qwen")
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "f"}}}}
	complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}, Config: &genai.GenerateContentConfig{Tools: tools}})
	if got := s.body(0)["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %v, want auto with tools offered and no choice expressed", got)
	}
	complete(t, m, &llm.Request{Contents: []*genai.Content{userText("x")}})
	if _, set := s.body(1)["tool_choice"]; set {
		t.Error("tool_choice sent on a request with no tools")
	}
}

// mast's final-report path forces a named finish_task call. A model
// that rejects a forced choice (gpt-oss on Vertex AI) gets "auto" with
// the same tools, and the response says it was not forced.
func TestAForcedChoiceIsDowngradedForAModelThatRejectsIt(t *testing.T) {
	force := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "finish_task"}}}},
		ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"finish_task"},
		}},
	}
	req := &llm.Request{Contents: []*genai.Content{userText("report")}, Config: force}

	s := newServer(t, reply{body: okReply}, reply{body: okReply}, reply{sse: true, body: sse(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)})
	c := client(t, s, nil)
	r := complete(t, c.ModelWith("gpt-oss", openaichat.ModelOptions{NoForcedToolChoice: new(true)}), req)
	if got := s.body(0)["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %v, want auto", got)
	}
	if r.CustomMetadata[openaichat.ToolChoiceKey] != true {
		t.Errorf("the downgrade was not marked: %v", r.CustomMetadata)
	}

	r = complete(t, c.Model("qwen"), req)
	if got := asJSON(t, s.body(1)["tool_choice"]); got != `{"function":{"name":"finish_task"},"type":"function"}` {
		t.Errorf("a model that takes a forced choice got %s", got)
	}
	if _, marked := r.CustomMetadata[openaichat.ToolChoiceKey]; marked {
		t.Error("an undowngraded request was marked")
	}

	var final *llm.Response
	for rr, err := range c.ModelWith("gpt-oss", openaichat.ModelOptions{NoForcedToolChoice: new(true)}).GenerateContent(context.Background(), req, true) {
		if err != nil {
			t.Fatal(err)
		}
		if !rr.Partial {
			final = rr
		}
	}
	if final.CustomMetadata[openaichat.ToolChoiceKey] != true {
		t.Error("a streamed downgrade was not marked on the final response")
	}
}

func TestAVertexErrorArrayIsUnwrapped(t *testing.T) {
	s := newServer(t, reply{status: 404, body: `[{"error": {"code": 404, "message": "Publisher model x was not found or your project does not have access to it.", "status": "NOT_FOUND"}}]`})
	err := generateErr(client(t, s, nil).Model("m"), &llm.Request{Contents: []*genai.Content{userText("x")}}, false)
	var ae *openaichat.APIError
	if !errors.As(err, &ae) || ae.StatusCode != 404 || !strings.HasPrefix(ae.Message, "Publisher model x was not found") {
		t.Errorf("err = %v", err)
	}
}
