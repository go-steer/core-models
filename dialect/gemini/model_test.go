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

package gemini_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/dialect/gemini"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"
	"github.com/go-steer/core-models/toolwire"
	"github.com/go-steer/core-models/usage"
)

// server is a fake Gemini endpoint: it answers each request with the
// next step (the last repeats) and records what it was sent.
type server struct {
	mu      sync.Mutex
	steps   []func(w http.ResponseWriter, r *http.Request)
	paths   []string
	bodies  []map[string]any
	headers []http.Header
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	s.mu.Lock()
	n := len(s.paths)
	s.paths = append(s.paths, r.URL.Path)
	s.bodies = append(s.bodies, body)
	s.headers = append(s.headers, r.Header.Clone())
	step := s.steps[min(n, len(s.steps)-1)]
	s.mu.Unlock()
	step(w, r)
}

func (s *server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

func jsonReply(status int, v string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, v)
	}
}

func sse(chunks ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	}
}

const answer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"the pod is crashlooping"}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":7,"cachedContentTokenCount":100,"thoughtsTokenCount":30,"toolUsePromptTokenCount":4,"totalTokenCount":161},
"modelVersion":"gemini-3.6-flash-001","responseId":"resp-1"}`

const bare400 = `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`

func fastRetry() *retry.Policy {
	p := retry.Default()
	p.Jitter = 0
	p.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	p.AfterSuccess = gemini.IsBareInvalidArgumentBody
	return &p
}

func apiClient(t *testing.T, s *server, opts gemini.Options) *gemini.Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	opts.Backend = genai.BackendGeminiAPI
	opts.APIKey = "test-key"
	opts.BaseURL = srv.URL
	if opts.Retry == nil {
		opts.Retry = fastRetry()
	}
	c, err := gemini.New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func collect(t *testing.T, m llm.LLM, ctx context.Context, req *llm.Request, stream bool) ([]*llm.Response, error) {
	t.Helper()
	var out []*llm.Response
	var last error
	for r, err := range m.GenerateContent(ctx, req, stream) {
		if err != nil {
			last = err
			continue
		}
		out = append(out, r)
	}
	return out, last
}

func userReq(s string) *llm.Request {
	return &llm.Request{Contents: []*genai.Content{genai.NewContentFromText(s, genai.RoleUser)}}
}

func TestACompleteAnswerCarriesItsUsageAndIdentity(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, answer)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	out, err := collect(t, m, context.Background(), userReq("why?"), false)
	if err != nil || len(out) != 1 {
		t.Fatalf("got %d responses, %v", len(out), err)
	}
	r := out[0]
	if r.Content.Parts[0].Text != "the pod is crashlooping" || r.FinishReason != genai.FinishReasonStop || !r.TurnComplete {
		t.Errorf("response = %+v", r)
	}
	d, ok := usage.FromMetadata(r.CustomMetadata)
	if !ok {
		t.Fatal("no usage sidecar")
	}
	if *d.CacheReadTokens != 100 || *d.ReasoningTokens != 30 || *d.ToolUseTokens != 4 || d.CacheWriteTokens != nil {
		t.Errorf("detail buckets = read %d, reasoning %d, tool %d, write %v", *d.CacheReadTokens, *d.ReasoningTokens, *d.ToolUseTokens, d.CacheWriteTokens)
	}
	if d.ServedModel != "gemini-3.6-flash-001" || d.ProviderRequestID != "resp-1" || d.Backend != "gemini" {
		t.Errorf("detail identity = %+v", d)
	}
	h := s.headers[0]
	if h.Get("x-goog-api-key") != "test-key" {
		t.Error("the API key header is missing")
	}
	if got := h.Get("x-goog-api-client"); !strings.Contains(got, "go-steer-core-models") || !strings.Contains(got, "google-genai-sdk") {
		t.Errorf("x-goog-api-client = %q, want genai's value and core-models' joined", got)
	}
	if !strings.HasSuffix(s.paths[0], "models/gemini-3.6-flash:generateContent") {
		t.Errorf("path = %q", s.paths[0])
	}
}

// TestAStreamIsAggregated: heartbeats, a thought, answer text in two
// pieces, and a function call whose arguments stream through
// PartialArgs, folded into one complete response.
func TestAStreamIsAggregated(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){sse(
		`{"usageMetadata":{"trafficType":"ON_DEMAND"},"modelVersion":"gemini-3.6-flash-001","responseId":"r"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking…","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Let me "}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"check."}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"kubectl","id":"c1","willContinue":true,"partialArgs":[{"jsonPath":"$.verb","stringValue":"get"}]}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"willContinue":true,"partialArgs":[{"jsonPath":"$.labels[1]","stringValue":"b"},{"jsonPath":"$.labels[0]","stringValue":"a"}]}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$['resource']","stringValue":"pods"}]}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`,
	)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	out, err := collect(t, m, context.Background(), userReq("why?"), true)
	if err != nil {
		t.Fatal(err)
	}
	final := out[len(out)-1]
	for _, r := range out[:len(out)-1] {
		if !r.Partial {
			t.Error("a streamed chunk is not marked partial")
		}
	}
	if final.Partial || !final.TurnComplete {
		t.Errorf("final partial=%v turnComplete=%v", final.Partial, final.TurnComplete)
	}
	parts := final.Content.Parts
	if len(parts) != 3 || !parts[0].Thought || parts[1].Text != "Let me check." || parts[2].FunctionCall == nil {
		t.Fatalf("aggregated parts = %+v", parts)
	}
	fc := parts[2].FunctionCall
	labels, _ := fc.Args["labels"].([]any)
	if fc.Name != "kubectl" || fc.ID != "c1" || fc.Args["verb"] != "get" || fc.Args["resource"] != "pods" || len(labels) != 2 || labels[0] != "a" {
		t.Errorf("function call = %+v", fc)
	}
	if final.ModelVersion != "gemini-3.6-flash-001" {
		t.Errorf("final ModelVersion = %q", final.ModelVersion)
	}
	if d, ok := usage.FromMetadata(final.CustomMetadata); !ok || d.ProviderRequestID != "r" {
		t.Errorf("final usage detail = %+v, %v", d, ok)
	}
	if !strings.HasSuffix(s.paths[0], ":streamGenerateContent") {
		t.Errorf("path = %q", s.paths[0])
	}
}

func TestATransientRejectionIsRetriedAndRecorded(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){
		jsonReply(429, `{"error":{"code":429,"message":"Resource exhausted. Please try again later.","status":"RESOURCE_EXHAUSTED"}}`),
		jsonReply(200, answer),
	}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	out, err := collect(t, m, context.Background(), userReq("why?"), false)
	if err != nil || len(out) != 1 || s.count() != 2 {
		t.Fatalf("got %d responses, %v, %d requests", len(out), err, s.count())
	}
	snap, ok := out[0].CustomMetadata[retry.MetadataKey].(retry.Snapshot)
	if !ok || snap.Retries != 1 || snap.LastStatus != 429 {
		t.Errorf("retry record = %+v", out[0].CustomMetadata[retry.MetadataKey])
	}
}

// TestTheBare400IsRetriedOnlyForAServedSession is core-agent #1247
// through the real stack, with New's default policy rule.
func TestTheBare400IsRetriedOnlyForAServedSession(t *testing.T) {
	served := callctx.NewPriorSuccess()
	served.Mark()
	for name, tc := range map[string]struct {
		ctx      context.Context
		requests int
		ok       bool
	}{
		"served":     {callctx.WithPriorSuccess(context.Background(), served), 2, true},
		"first call": {context.Background(), 1, false},
		"side call":  {callctx.AsSideCall(callctx.WithPriorSuccess(context.Background(), served), "btw"), 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(400, bare400), jsonReply(200, answer)}}
			m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
			_, err := collect(t, m, tc.ctx, userReq("why?"), false)
			if (err == nil) != tc.ok || s.count() != tc.requests {
				t.Fatalf("err %v after %d requests; want ok=%v after %d", err, s.count(), tc.ok, tc.requests)
			}
			if err != nil && !gemini.IsBareInvalidArgument(err) {
				t.Errorf("the 400 handed back is not recognisable: %v", err)
			}
		})
	}
}

func TestNoCandidatesTwiceIsAnEmptyResponse(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, `{"usageMetadata":{"promptTokenCount":3}}`)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	_, err := collect(t, m, context.Background(), userReq("why?"), false)
	if !errors.Is(err, llm.ErrEmptyResponse) || s.count() != 2 {
		t.Errorf("err %v after %d requests; want ErrEmptyResponse after one retry", err, s.count())
	}
}

func TestABlockedPromptIsUsableAndCarriesTheReason(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked"}}`)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	out, err := collect(t, m, context.Background(), userReq("why?"), false)
	if err != nil || len(out) != 1 || out[0].ErrorCode != "SAFETY" || s.count() != 1 {
		t.Errorf("got %+v, %v after %d requests", out, err, s.count())
	}
}

func TestAHistoryEndingOnTheModelGetsAUserTurn(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, answer)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	req := &llm.Request{Contents: []*genai.Content{
		genai.NewContentFromText("hi", genai.RoleUser),
		genai.NewContentFromText("hello", genai.RoleModel),
	}}
	if _, err := collect(t, m, context.Background(), req, false); err != nil {
		t.Fatal(err)
	}
	contents, _ := s.bodies[0]["contents"].([]any)
	if len(contents) != 3 || len(req.Contents) != 2 {
		t.Errorf("sent %d turns, caller's history now %d; want 3 sent and the caller's untouched", len(contents), len(req.Contents))
	}
}

func vertexClient(t *testing.T, s *server, bt gemini.BuiltinTools) *gemini.Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{
		DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) { return fakeToken("adc-token"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := gemini.New(context.Background(), gemini.Options{
		Backend: genai.BackendVertexAI, Project: "p", Location: "us-central1", Credential: cred,
		BaseURL: srv.URL, Retry: fastRetry(), BuiltinTools: bt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeToken string

func (f fakeToken) Token(context.Context) (string, error) { return string(f), nil }

// TestTheBackendDecidesTheServerSideFlag is mast #505 on the wire: the
// Developer API gets include_server_side_tool_invocations beside
// built-ins and function tools; Vertex AI, which rejects it, never does.
func TestTheBackendDecidesTheServerSideFlag(t *testing.T) {
	req := func() *llm.Request {
		r := userReq("search for it")
		r.Config = &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "finish_task"}}}}}
		return r
	}
	bt := gemini.BuiltinTools{GoogleSearch: true}

	api := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, answer)}}
	if _, err := collect(t, apiClient(t, api, gemini.Options{BuiltinTools: bt}).Model("gemini-3.6-flash"), context.Background(), req(), false); err != nil {
		t.Fatal(err)
	}
	tc, _ := api.bodies[0]["toolConfig"].(map[string]any)
	if tc["includeServerSideToolInvocations"] != true {
		t.Errorf("Developer API toolConfig = %v, want the flag set", tc)
	}

	vx := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, answer)}}
	out, err := collect(t, vertexClient(t, vx, bt).Model("gemini-3.6-flash"), context.Background(), req(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := vx.bodies[0]["toolConfig"]; set {
		t.Errorf("Vertex was sent a toolConfig: %v", vx.bodies[0]["toolConfig"])
	}
	tools, _ := vx.bodies[0]["tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("Vertex tools = %v, want the function tool and google_search", tools)
	}
	if got := vx.headers[0].Get("Authorization"); got != "Bearer adc-token" {
		t.Errorf("Authorization = %q", got)
	}
	if !strings.Contains(vx.paths[0], "projects/p/locations/us-central1/publishers/google/models/gemini-3.6-flash") {
		t.Errorf("Vertex path = %q", vx.paths[0])
	}
	if d, _ := usage.FromMetadata(out[0].CustomMetadata); d.Backend != "vertex" || d.Region != "us-central1" {
		t.Errorf("Vertex detail = %+v", d)
	}
}

// TestEveryToolArrivesWhole holds the adapter to toolwire's invariant:
// each declared tool reaches Gemini with every argument, the required
// ones still required.
func TestEveryToolArrivesWhole(t *testing.T) {
	decls := []*genai.FunctionDeclaration{
		{Name: "kubectl", Description: "run kubectl", Parameters: &genai.Schema{
			Type: genai.TypeObject, Required: []string{"verb", "resource"},
			Properties: map[string]*genai.Schema{
				"verb":      {Type: genai.TypeString, Enum: []string{"get", "describe"}},
				"resource":  {Type: genai.TypeString},
				"namespace": {Type: genai.TypeString},
				"labels":    {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
			},
		}},
		{Name: "read_file", ParametersJsonSchema: map[string]any{
			"type": "object", "required": []any{"path"},
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "minimum": 1},
			},
		}},
		{Name: "finish_task", Description: "no arguments"},
	}
	var entries []toolwire.Entry
	for _, d := range decls {
		e, err := toolwire.EntryFor("gemini", d)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200, answer)}}
	m := apiClient(t, s, gemini.Options{BuiltinTools: gemini.BuiltinTools{GoogleSearch: true}}).Model("gemini-3.6-flash")
	req := userReq("go")
	req.Config = &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: decls}}}
	if _, err := collect(t, m, context.Background(), req, false); err != nil {
		t.Fatal(err)
	}
	wire := toolwire.Wire{}
	tools, _ := s.bodies[0]["tools"].([]any)
	for _, tl := range tools {
		fds, _ := tl.(map[string]any)["functionDeclarations"].([]any)
		for _, fd := range fds {
			f := fd.(map[string]any)
			schema, ok := f["parameters"].(map[string]any)
			if !ok {
				schema, _ = f["parametersJsonSchema"].(map[string]any)
			}
			if schema == nil {
				schema = map[string]any{}
			}
			wire[f["name"].(string)] = schema
		}
	}
	if problems := toolwire.Verify(entries, wire); len(problems) > 0 {
		t.Errorf("tools did not arrive whole:\n%s", strings.Join(problems, "\n"))
	}
}

// TestACallersPolicyStillGetsTheBare400Rule is fix 9: a Retry policy
// supplied without AfterSuccess is merged with the rule, not used
// instead of it.
func TestACallersPolicyStillGetsTheBare400Rule(t *testing.T) {
	served := callctx.NewPriorSuccess()
	served.Mark()
	p := retry.Default()
	p.Jitter = 0
	p.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(400, bare400), jsonReply(200, answer)}}
	m := apiClient(t, s, gemini.Options{Retry: &p}).Model("gemini-3.6-flash")
	if _, err := collect(t, m, callctx.WithPriorSuccess(context.Background(), served), userReq("why?"), false); err != nil || s.count() != 2 {
		t.Errorf("err %v after %d requests; want the bare 400 retried under the caller's policy", err, s.count())
	}
	if p.AfterSuccess != nil {
		t.Error("New modified the caller's policy")
	}
}

// TestUnreportedBucketsStayNil: a usage block without the optional
// buckets leaves them nil, never 0 (AGENTS.md rule 11).
func TestUnreportedBucketsStayNil(t *testing.T) {
	s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(200,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`)}}
	m := apiClient(t, s, gemini.Options{}).Model("gemini-3.6-flash")
	out, err := collect(t, m, context.Background(), userReq("hi"), false)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := usage.FromMetadata(out[0].CustomMetadata)
	if !ok || d.CacheReadTokens != nil || d.ReasoningTokens != nil || d.ToolUseTokens != nil || d.CacheWriteTokens != nil {
		t.Errorf("detail = %+v; want every unreported bucket nil", d)
	}
}
