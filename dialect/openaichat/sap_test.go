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

	"google.golang.org/genai"

	"github.com/go-steer/core-models/dialect/openaichat"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/usage"
)

// fakeSAP is SAP AI Core's AI API (deployments) and orchestration
// service (completion), shaped after SAP's Orchestration v2 spec and
// the fixtures in SAP's Cloud SDK for AI.
type fakeSAP struct {
	srv *httptest.Server

	mu          sync.Mutex
	deployments []map[string]any          // the list endpoint's resources
	byID        map[string]map[string]any // the get-one endpoint
	gone        map[string]bool           // deployments whose completion answers 404
	replies     []sapReply
	bodies      []map[string]any
	paths       []string
	listQueries []string
}

type sapReply struct {
	status int
	sse    bool
	body   string
}

func newFakeSAP(t *testing.T, replies ...sapReply) *fakeSAP {
	f := &fakeSAP{byID: map[string]map[string]any{}, gone: map[string]bool{}, replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSAP) api() string { return f.srv.URL + "/v2" }

// deployment is a RUNNING orchestration deployment record at this
// server.
func (f *fakeSAP) deployment(id, createdAt string) map[string]any {
	return map[string]any{
		"id": id, "deploymentUrl": f.api() + "/inference/deployments/" + id,
		"scenarioId": "orchestration", "status": "RUNNING", "createdAt": createdAt,
	}
}

func (f *fakeSAP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/lm/deployments":
		f.listQueries = append(f.listQueries, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(f.deployments), "resources": f.deployments})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/lm/deployments/"):
		rec, ok := f.byID[strings.TrimPrefix(r.URL.Path, "/v2/lm/deployments/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"03021001","message":"Deployment not found"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(rec)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/completion"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/inference/deployments/"), "/v2/completion")
		f.paths = append(f.paths, r.URL.Path)
		if f.gone[id] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"request_id":"r-404","code":404,"message":"deployment not found","location":"Request"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		if len(f.replies) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rp := f.replies[0]
		f.replies = f.replies[1:]
		if rp.sse {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		if rp.status != 0 {
			w.WriteHeader(rp.status)
		}
		_, _ = io.WriteString(w, rp.body)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func (f *fakeSAP) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func sapClient(t *testing.T, f *fakeSAP, mod func(*openaichat.Options)) *openaichat.Client {
	t.Helper()
	o := openaichat.Options{BaseURL: f.api(), Platform: openaichat.PlatformSAPOrchestration, Backend: "sap-ai-core"}
	if mod != nil {
		mod(&o)
	}
	c, err := openaichat.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const sapHello = `{"request_id":"req-1","intermediate_results":{},"final_result":{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"completion_tokens":10,"prompt_tokens":9,"total_tokens":19}}}`

// dig walks a decoded JSON object.
func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// The newest RUNNING orchestration deployment is used: a stopped one,
// a newer one running another scenario and an older one are passed over.
func TestSAPFindsTheNewestRunningDeployment(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	stopped := f.deployment("d-stopped", "2026-10-19T00:00:00Z")
	stopped["status"] = "STOPPED"
	other := f.deployment("d-other", "2026-10-19T00:00:00Z")
	other["scenarioId"] = "foundation-models"
	f.deployments = []map[string]any{
		f.deployment("d-old", "2026-01-01T00:00:00Z"), stopped, other,
		f.deployment("d-new", "2026-10-01T00:00:00Z"),
	}
	c := sapClient(t, f, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := complete(t, c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	if text := r.Content.Parts[0].Text; text != "Hello!" {
		t.Errorf("text = %q", text)
	}
	if f.paths[0] != "/v2/inference/deployments/d-new/v2/completion" {
		t.Errorf("posted to %q", f.paths[0])
	}
	if len(f.listQueries) != 1 || !strings.Contains(f.listQueries[0], "scenarioId=orchestration") || !strings.Contains(f.listQueries[0], "status=RUNNING") {
		t.Errorf("list queries = %v", f.listQueries)
	}
}

func TestSAPPinnedDeployment(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	f.byID["d-pin"] = f.deployment("d-pin", "2026-01-01T00:00:00Z")
	stopped := f.deployment("d-stopped", "2026-01-01T00:00:00Z")
	stopped["status"] = "STOPPED"
	f.byID["d-stopped"] = stopped
	other := f.deployment("d-fm", "2026-01-01T00:00:00Z")
	other["scenarioId"] = "foundation-models"
	f.byID["d-fm"] = other

	c := sapClient(t, f, func(o *openaichat.Options) { o.DeploymentID = "d-pin" })
	complete(t, c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	if len(f.listQueries) != 0 || f.paths[0] != "/v2/inference/deployments/d-pin/v2/completion" {
		t.Errorf("list queries %v, paths %v", f.listQueries, f.paths)
	}
	for id, want := range map[string]string{
		"d-stopped": "deployment d-stopped is STOPPED, not RUNNING",
		"d-fm":      `runs scenario "foundation-models"`,
		"d-missing": "Deployment not found",
	} {
		err := sapClient(t, f, func(o *openaichat.Options) { o.DeploymentID = id }).Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Connect(%s) = %v, want %q", id, err, want)
		}
	}
}

func TestSAPNoRunningDeployment(t *testing.T) {
	f := newFakeSAP(t)
	err := sapClient(t, f, nil).Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no RUNNING orchestration deployment") {
		t.Fatalf("Connect = %v", err)
	}
}

// The bearer token goes wherever the deployment URL points, so one on
// another host is refused.
func TestSAPDeploymentOnAnotherHost(t *testing.T) {
	f := newFakeSAP(t)
	d := f.deployment("d1", "2026-01-01T00:00:00Z")
	d["deploymentUrl"] = "https://elsewhere.example.com/v2/inference/deployments/d1"
	f.deployments = []map[string]any{d}
	err := sapClient(t, f, nil).Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("Connect = %v", err)
	}
}

// A replaced deployment answers 404; the current one is found and the
// request sent once more.
func TestSAPFindsTheDeploymentAgainAfter404(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	c := sapClient(t, f, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.gone["d1"] = true
	f.deployments = []map[string]any{f.deployment("d2", "2026-02-01T00:00:00Z")}
	f.mu.Unlock()
	complete(t, c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	if len(f.paths) != 2 || !strings.Contains(f.paths[1], "/d2/") || len(f.listQueries) != 2 {
		t.Errorf("paths %v, list queries %v", f.paths, f.listQueries)
	}

	// When the deployment found again is the same one, the 404 is the
	// answer.
	f.mu.Lock()
	f.gone["d2"] = true
	f.mu.Unlock()
	err := generateErr(c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}}, false)
	var ae *openaichat.APIError
	if !errors.As(err, &ae) || ae.StatusCode != 404 {
		t.Fatalf("err = %v", err)
	}
}

// The conversation goes to messages_history and only the newest
// message to the template; tools, the response format and the
// sampling settings go where orchestration takes them, and ExtraBody's
// modules reach config.modules.
func TestSAPEnvelope(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	c := sapClient(t, f, func(o *openaichat.Options) {
		o.ReasoningEcho = true
		o.ResponseSchema = true
		o.ExtraBody = map[string]any{
			"reasoning_effort": "low",
			"modules":          map[string]any{"masking": map[string]any{"providers": []any{}}},
		}
	})
	temp := float32(0.2)
	req := &llm.Request{
		Contents: []*genai.Content{
			userText("what is up with pod web-7?"),
			{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "I should look it up.", Thought: true},
				{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "get_pod_status", Args: map[string]any{"pod": "web-7"}}},
			}},
			{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "get_pod_status", Response: map[string]any{"output": "CrashLoopBackOff"}}}}},
			userText("summarise that"),
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("You are an SRE.", genai.RoleUser),
			Temperature:       &temp,
			MaxOutputTokens:   256,
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name: "get_pod_status", ParametersJsonSchema: map[string]any{"type": "object"},
			}}}},
			ToolConfig:         &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}},
			ResponseMIMEType:   "application/json",
			ResponseJsonSchema: map[string]any{"type": "object"},
		},
	}
	complete(t, c.Model("anthropic--claude-4.5-sonnet"), req)
	b := f.body(0)
	pt := dig(b, "config", "modules", "prompt_templating")
	tmpl, _ := dig(pt, "prompt", "template").([]any)
	if len(tmpl) != 1 || dig(tmpl[0], "content") != "summarise that" || dig(tmpl[0], "role") != "user" {
		t.Errorf("template = %v", tmpl)
	}
	if tools, _ := dig(pt, "prompt", "tools").([]any); len(tools) != 1 || dig(tools[0], "function", "name") != "get_pod_status" {
		t.Errorf("tools = %v", dig(pt, "prompt", "tools"))
	}
	if dig(pt, "prompt", "response_format", "type") != "json_schema" {
		t.Errorf("response_format = %v", dig(pt, "prompt", "response_format"))
	}
	if dig(pt, "model", "name") != "anthropic--claude-4.5-sonnet" {
		t.Errorf("model = %v", dig(pt, "model"))
	}
	params, _ := dig(pt, "model", "params").(map[string]any)
	if params["max_tokens"] != float64(256) || params["reasoning_effort"] != "low" || params["tool_choice"] != "required" {
		t.Errorf("params = %v", params)
	}
	if v, ok := params["temperature"].(float64); !ok || v < 0.19 || v > 0.21 {
		t.Errorf("temperature = %v", params["temperature"])
	}
	if dig(b, "config", "modules", "masking") == nil {
		t.Errorf("modules = %v", dig(b, "config", "modules"))
	}
	if dig(b, "config", "stream") != nil {
		t.Errorf("a unary request asks to stream: %v", dig(b, "config", "stream"))
	}
	hist, _ := b["messages_history"].([]any)
	if len(hist) != 4 {
		t.Fatalf("messages_history = %v", hist)
	}
	if dig(hist[0], "role") != "system" || dig(hist[1], "role") != "user" || dig(hist[3], "tool_call_id") != "call-1" {
		t.Errorf("messages_history = %v", hist)
	}
	reasoning, _ := dig(hist[2], "reasoning_content").([]any)
	if len(reasoning) != 1 || dig(reasoning[0], "content") != "I should look it up." {
		t.Errorf("reasoning_content = %v", dig(hist[2], "reasoning_content"))
	}
	if calls, _ := dig(hist[2], "tool_calls").([]any); len(calls) != 1 || dig(calls[0], "id") != "call-1" {
		t.Errorf("tool_calls = %v", dig(hist[2], "tool_calls"))
	}
	if _, set := hist[2].(map[string]any)["content"]; set {
		t.Errorf("a tool-call-only assistant turn sends content: %v", hist[2])
	}
}

func TestSAPStreamingRequest(t *testing.T) {
	f := newFakeSAP(t, sapReply{sse: true, body: "data: " + `{"request_id":"r","final_result":{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"completion_tokens":1,"prompt_tokens":1,"total_tokens":2}}}` + "\n\ndata: [DONE]\n\n"})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	c := sapClient(t, f, nil)
	for _, err := range c.Model("gpt-4o").GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{userText("hi")}}, true) {
		if err != nil {
			t.Fatal(err)
		}
	}
	b := f.body(0)
	if dig(b, "config", "stream", "enabled") != true || dig(b, "config", "modules", "prompt_templating", "model", "params", "stream_options", "include_usage") != true {
		t.Errorf("body = %v", b)
	}
}

// The template is rendered for {{?name}} placeholders, so a newest
// message holding one is refused before anything is sent; in history it
// passes through untouched.
func TestSAPPlaceholderInTheNewestMessage(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	c := sapClient(t, f, nil)
	err := generateErr(c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("render {{?name}} please")}}, false)
	if err == nil || !strings.Contains(err.Error(), "placeholder") || len(f.paths) != 0 {
		t.Fatalf("err = %v, sent %d", err, len(f.paths))
	}
	complete(t, c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{
		userText("render {{?name}} please"), {Role: genai.RoleModel, Parts: []*genai.Part{{Text: "sure"}}}, userText("go on"),
	}})
	if hist, _ := f.body(0)["messages_history"].([]any); dig(hist[0], "content") != "render {{?name}} please" {
		t.Errorf("history = %v", hist)
	}
}

func TestSAPModulesCannotReplacePromptTemplating(t *testing.T) {
	f := newFakeSAP(t)
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	for _, extra := range []map[string]any{
		{"modules": map[string]any{"prompt_templating": map[string]any{}}},
		{"modules": "masking"},
		{"temperature": 1},
	} {
		c := sapClient(t, f, func(o *openaichat.Options) { o.ExtraBody = extra })
		temp := float32(0.5)
		err := generateErr(c.Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}, Config: &genai.GenerateContentConfig{Temperature: &temp}}, false)
		if err == nil {
			t.Errorf("extra_body %v accepted", extra)
		}
	}
}

// final_result is read as a Chat Completion: reasoning blocks become a
// thought part, tool calls calls, and every usage count SAP reports
// lands, cache writes included. The id is orchestration's request_id.
func TestSAPCompleteResponse(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: `{"request_id":"req-9","intermediate_results":{},"final_result":{
		"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"anthropic--claude-4.5-sonnet",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Looking.",
			"reasoning_content":[{"content":"The user wants ","signature":"sig"},{"content":"pod status."}],
			"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"get_pod_status","arguments":"{\"pod\":\"web-7\"}"}}]},
			"finish_reason":"tool_calls"}],
		"usage":{"completion_tokens":40,"prompt_tokens":1200,"total_tokens":1240,
			"prompt_tokens_details":{"cached_tokens":1000,"cache_creation_tokens":150,"cache_creation_token_details":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":100}},
			"completion_tokens_details":{"reasoning_tokens":12}}}}`})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	r := complete(t, sapClient(t, f, nil).Model("anthropic--claude-4.5-sonnet"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	parts := r.Content.Parts
	if len(parts) != 3 || !parts[0].Thought || parts[0].Text != "The user wants pod status." || parts[1].Text != "Looking." || parts[2].FunctionCall == nil || parts[2].FunctionCall.ID != "toolu_1" || parts[2].FunctionCall.Args["pod"] != "web-7" {
		t.Fatalf("parts = %s", asJSON(t, parts))
	}
	if r.FinishReason != genai.FinishReasonStop {
		t.Errorf("finish = %v", r.FinishReason)
	}
	d, _ := usage.FromMetadata(r.CustomMetadata)
	if d.ProviderRequestID != "req-9" || d.ServedModel != "anthropic--claude-4.5-sonnet" || d.Backend != "sap-ai-core" {
		t.Errorf("detail = %+v", d)
	}
	for name, got := range map[string]*int64{"cache_read": d.CacheReadTokens, "cache_write": d.CacheWriteTokens, "cache_write_1h": d.CacheWrite1hTokens, "reasoning": d.ReasoningTokens} {
		want := map[string]int64{"cache_read": 1000, "cache_write": 150, "cache_write_1h": 100, "reasoning": 12}[name]
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %d", name, got, want)
		}
	}
	if u := r.UsageMetadata; u.PromptTokenCount != 1200 || u.CandidatesTokenCount != 28 || u.ThoughtsTokenCount != 12 || u.CachedContentTokenCount != 1000 {
		t.Errorf("usage = %+v", u)
	}
}

// A count SAP did not send stays unreported (rule #11).
func TestSAPUnreportedUsageStaysNil(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: sapHello})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	r := complete(t, sapClient(t, f, nil).Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	d, _ := usage.FromMetadata(r.CustomMetadata)
	if d.CacheReadTokens != nil || d.CacheWriteTokens != nil || d.CacheWrite1hTokens != nil || d.ReasoningTokens != nil || d.ToolUseTokens != nil {
		t.Errorf("detail = %+v: unreported counts must stay nil", d)
	}
	if u := r.UsageMetadata; u.PromptTokenCount != 9 || u.CandidatesTokenCount != 10 || u.TotalTokenCount != 19 {
		t.Errorf("usage = %+v", u)
	}
}

// Orchestration v1 named the result orchestration_result.
func TestSAPOrchestrationResultV1(t *testing.T) {
	f := newFakeSAP(t, sapReply{body: `{"request_id":"r1","module_results":{},"orchestration_result":{"id":"c","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"v1 says hi"},"finish_reason":"stop"}],"usage":{"completion_tokens":3,"prompt_tokens":2,"total_tokens":5}}}`})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	r := complete(t, sapClient(t, f, nil).Model("gpt-4o"), &llm.Request{Contents: []*genai.Content{userText("hi")}})
	if r.Content.Parts[0].Text != "v1 says hi" {
		t.Errorf("parts = %s", asJSON(t, r.Content.Parts))
	}
}

// A stream as SAP's SDK fixtures show it: an empty first event, deltas
// under final_result, a tool call split across events, usage and the
// finish reason last, then [DONE].
func TestSAPStream(t *testing.T) {
	ev := func(final string) string {
		return `data: {"request_id":"req-s","intermediate_results":{},"final_result":` + final + "}\n\n"
	}
	stream := ev(`{"id":"","object":"","created":0,"model":"","system_fingerprint":"","choices":[{"index":0,"delta":{"role":"","content":""},"finish_reason":""}]}`) +
		ev(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":[{"content":"think"}]},"finish_reason":""}]}`) +
		ev(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":"1, 2, "},"finish_reason":""}]}`) +
		ev(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":"3","tool_calls":[{"index":1,"id":"call_9","type":"function","function":{"name":"get_pod_status","arguments":"{\"pod\":"}}]},"finish_reason":""}]}`) +
		ev(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"index":1,"function":{"arguments":"\"web-7\"}"}}]},"finish_reason":"stop"}],"usage":{"completion_tokens":7,"prompt_tokens":17,"total_tokens":24}}`) +
		"data: [DONE]\n\n"
	f := newFakeSAP(t, sapReply{sse: true, body: stream})
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	var partials []string
	var final *llm.Response
	for r, err := range sapClient(t, f, nil).Model("gpt-4o").GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{userText("count")}}, true) {
		if err != nil {
			t.Fatal(err)
		}
		if r.Partial {
			partials = append(partials, r.Content.Parts[0].Text)
			continue
		}
		final = r
	}
	if strings.Join(partials, "|") != "think|1, 2, |3" {
		t.Errorf("partials = %q", partials)
	}
	if final == nil || !final.TurnComplete {
		t.Fatal("no complete final response")
	}
	parts := final.Content.Parts
	if len(parts) != 3 || parts[1].Text != "1, 2, 3" || parts[2].FunctionCall == nil || parts[2].FunctionCall.Args["pod"] != "web-7" || parts[2].FunctionCall.ID != "call_9" {
		t.Errorf("final parts = %s", asJSON(t, parts))
	}
	d, _ := usage.FromMetadata(final.CustomMetadata)
	if d.ProviderRequestID != "req-s" || d.ServedModel != "gpt-4o-2024-08-06" || final.UsageMetadata.PromptTokenCount != 17 || final.UsageMetadata.CandidatesTokenCount != 7 {
		t.Errorf("detail = %+v usage = %+v", d, final.UsageMetadata)
	}
	if d.CacheReadTokens != nil || d.ReasoningTokens != nil {
		t.Errorf("unreported counts set: %+v", d)
	}
}

// Orchestration's errors, over HTTP and in a stream, keep the code, the
// failing module and every message.
func TestSAPErrors(t *testing.T) {
	f := newFakeSAP(t,
		sapReply{status: 400, body: `{"error":{"request_id":"r","code":400,"message":"400 - LLM Module: Model gpt-5 in version wrong-version not found.","location":"LLM Module"}}`},
		sapReply{status: 400, body: `{"error":[{"request_id":"r","code":400,"message":"first config failed","location":"LLM Module"},{"request_id":"r","code":429,"message":"fallback throttled","location":"LLM Module"}]}`},
		sapReply{status: 403, body: `upstream connect error`},
		sapReply{sse: true, body: `data: {"request_id":"r","intermediate_results":{"templating":[]},"final_result":{"id":"","object":"","created":0,"model":"","choices":[{"index":0,"delta":{"content":""}}]}}` + "\n\n" +
			`data: {"error": {"request_id": "r2", "code": 400, "message": "Input Filter: content blocked", "location": "Input Filter"}}` + "\n\n"},
	)
	f.deployments = []map[string]any{f.deployment("d1", "2026-01-01T00:00:00Z")}
	m := sapClient(t, f, nil).Model("gpt-5")
	req := &llm.Request{Contents: []*genai.Content{userText("hi")}}
	for i, want := range []openaichat.APIError{
		{StatusCode: 400, Type: "LLM Module", Code: "400", Message: "400 - LLM Module: Model gpt-5 in version wrong-version not found."},
		{StatusCode: 400, Type: "LLM Module", Code: "400", Message: "first config failed; fallback throttled"},
		{StatusCode: 403, Message: "upstream connect error"},
		{StatusCode: 0, Type: "Input Filter", Code: "400", Message: "Input Filter: content blocked"},
	} {
		err := generateErr(m, req, i == 3)
		var ae *openaichat.APIError
		if !errors.As(err, &ae) || *ae != want {
			t.Errorf("error %d = %#v, want %#v", i, err, want)
		}
	}
}

// DeploymentID belongs to the SAP platform.
func TestDeploymentIDNeedsTheSAPPlatform(t *testing.T) {
	if _, err := openaichat.New(openaichat.Options{BaseURL: "http://x/v1", DeploymentID: "d1"}); err == nil {
		t.Error("New accepted a DeploymentID without the sap-orchestration platform")
	}
	if _, err := openaichat.New(openaichat.Options{BaseURL: "http://x/v1", Platform: "azure"}); err == nil {
		t.Error("New accepted an unknown platform")
	}
}
