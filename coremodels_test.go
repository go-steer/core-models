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

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genai"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/dialect/anthropic"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
	"github.com/go-steer/core-models/usage"
)

func opts(env map[string]string) coremodels.Options {
	return coremodels.Options{Resolve: profile.Options{
		Getenv: func(k string) string { return env[k] },
		Auth: auth.Options{DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) {
			return staticToken("ya29.test"), nil
		}},
	}}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func TestOpenAProfileAndCallIt(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"id":"c","model":"openai/gpt-oss-20b-maas","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer srv.Close()

	maas := profile.Profile{Name: "maas-test", Extends: "vertex-maas", BaseURL: srv.URL + "/v1/projects/{project}/locations/{region}/endpoints/openapi"}
	p, err := coremodels.Open(context.Background(), maas, opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "maas-test" || p.Backend() != "vertex-maas" {
		t.Errorf("name %q backend %q", p.Name(), p.Backend())
	}
	m, err := p.Model(context.Background(), "openai/gpt-oss-20b-maas")
	if err != nil {
		t.Fatal(err)
	}
	var final *llm.Response
	for r, err := range m.GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}, false) {
		if err != nil {
			t.Fatal(err)
		}
		final = r
	}
	if gotPath != "/v1/projects/acme/locations/global/endpoints/openapi/chat/completions" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer ya29.test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	d, _ := usage.FromMetadata(final.CustomMetadata)
	if d.Backend != "vertex-maas" || d.Region != "global" || final.ModelVersion != "openai/gpt-oss-20b-maas" {
		t.Errorf("detail %+v, model version %q", d, final.ModelVersion)
	}
}

func TestOpenRefusesBeforeAnyRequest(t *testing.T) {
	closed := profile.Profile{
		Name: "lab", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1",
		OpenModels: new(false), Models: []profile.Model{{ID: "served"}},
	}
	p, err := coremodels.Open(context.Background(), closed, opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Model(context.Background(), "other"); err == nil || !strings.Contains(err.Error(), `does not serve model "other"`) {
		t.Errorf("Model(other) = %v", err)
	}

	for name, tc := range map[string]struct {
		p    profile.Profile
		env  map[string]string
		want string
	}{
		"missing project":  {profile.Profile{Name: "m", Extends: "vertex-maas"}, nil, "GOOGLE_CLOUD_PROJECT is not set"},
		"template, no url": {profile.Profile{Name: "v", Extends: "vllm"}, nil, "base_url is required"},
		"unbuilt dialect": {profile.Profile{Name: "resp", Dialect: profile.OpenAIResponses, BaseURL: "http://127.0.0.1:1/v1",
			Auth: auth.Config{Kind: auth.None}, OpenModels: new(true)}, nil, "not built yet"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := coremodels.Open(context.Background(), tc.p, opts(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open = %v, want %q", err, tc.want)
			}
		})
	}
}

// The built-in vertex-maas profile carries gpt-oss's documented
// limitation; Open must hand it to the adapter per model.
func TestOpenAppliesPerModelCapabilities(t *testing.T) {
	var choices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		c, _ := json.Marshal(b["tool_choice"])
		choices = append(choices, string(c))
		_, _ = io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p, err := coremodels.Open(context.Background(),
		profile.Profile{Name: "maas", Extends: "vertex-maas", BaseURL: srv.URL + "/v1"},
		opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	req := &llm.Request{
		Contents: []*genai.Content{genai.NewContentFromText("report", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "finish_task"}}}},
			ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"finish_task"},
			}},
		},
	}
	for _, id := range []string{"openai/gpt-oss-20b-maas", "zai-org/glm-5.2-maas"} {
		m, err := p.Model(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range m.GenerateContent(context.Background(), req, false) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(choices) != 2 || choices[0] != `"auto"` || !strings.Contains(choices[1], "finish_task") {
		t.Errorf("tool_choice per model = %v, want gpt-oss downgraded to auto and GLM forced", choices)
	}
}

func TestOpenAppliesPerModelExtraBody(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		_, _ = io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p, err := coremodels.Open(context.Background(), profile.Profile{
		Name: "lab", Extends: "vllm", BaseURL: srv.URL + "/v1",
		Models: []profile.Model{{ID: "gemma", ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}}},
	}, opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gemma", "qwen"} {
		m, _ := p.Model(context.Background(), id)
		for _, err := range m.GenerateContent(context.Background(), &llm.Request{Contents: []*genai.Content{genai.NewContentFromText("x", genai.RoleUser)}}, false) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := bodies[0]["chat_template_kwargs"]; !ok {
		t.Error("gemma's extra_body was not sent")
	}
	if _, ok := bodies[1]["chat_template_kwargs"]; ok {
		t.Error("another model received gemma's extra_body")
	}
}

// claudeSSE is the smallest complete Messages stream.
const claudeSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// claudeServer records each request's path, auth headers and body.
type claudeServer struct {
	*httptest.Server
	path, key, bearer string
	body              map[string]any
}

func newClaudeServer(t *testing.T) *claudeServer {
	t.Helper()
	c := &claudeServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path, c.key, c.bearer = r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		c.body = nil
		_ = json.Unmarshal(raw, &c.body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, claudeSSE)
	}))
	t.Cleanup(c.Close)
	return c
}

func callClaude(t *testing.T, p coremodels.Provider) *llm.Response {
	t.Helper()
	m, err := p.Model(context.Background(), "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	var final *llm.Response
	req := &llm.Request{
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("be brief", genai.RoleUser)},
	}
	for r, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatal(err)
		}
		final = r
	}
	return final
}

// The two Claude built-ins, opened and called: the first-party API with
// its key, Vertex AI with ADC and the model in the path.
func TestOpenAnAnthropicProfile(t *testing.T) {
	t.Run("first party", func(t *testing.T) {
		s := newClaudeServer(t)
		p, err := coremodels.Open(context.Background(),
			profile.Profile{Name: "claude", Extends: "anthropic", BaseURL: s.URL},
			opts(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}))
		if err != nil {
			t.Fatal(err)
		}
		final := callClaude(t, p)
		if s.path != "/v1/messages" || s.key != "sk-ant-test" || s.bearer != "" {
			t.Errorf("path %q, x-api-key %q, Authorization %q", s.path, s.key, s.bearer)
		}
		d, _ := usage.FromMetadata(final.CustomMetadata)
		if d == nil || d.Backend != "claude" || final.ModelVersion != "claude-haiku-4-5" {
			t.Errorf("detail %+v, model version %q", d, final.ModelVersion)
		}
	})
	t.Run("vertex", func(t *testing.T) {
		s := newClaudeServer(t)
		p, err := coremodels.Open(context.Background(),
			profile.Profile{Name: "claude-vertex", Extends: "anthropic-vertex",
				BaseURL: s.URL + "/v1/projects/{project}/locations/{region}/publishers/anthropic/models"},
			opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"}))
		if err != nil {
			t.Fatal(err)
		}
		final := callClaude(t, p)
		if want := "/v1/projects/acme/locations/us-east5/publishers/anthropic/models/claude-haiku-4-5:streamRawPredict"; s.path != want {
			t.Errorf("path = %s, want %s", s.path, want)
		}
		if s.bearer != "Bearer ya29.test" || s.key != "" {
			t.Errorf("Authorization %q, x-api-key %q", s.bearer, s.key)
		}
		d, _ := usage.FromMetadata(final.CustomMetadata)
		if d == nil || d.Backend != "anthropic-vertex" || d.Region != "us-east5" {
			t.Errorf("detail %+v", d)
		}
	})
}

// Prompt caching is on by default through Open, off with a zero policy,
// and server-side tools are off unless named.
func TestOpenAnthropicCachingAndBuiltins(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "k"}
	open := func(t *testing.T, s *claudeServer, o coremodels.Options) coremodels.Provider {
		t.Helper()
		p, err := coremodels.Open(context.Background(), profile.Profile{Name: "claude", Extends: "anthropic", BaseURL: s.URL}, o)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	systemMarked := func(body map[string]any) bool {
		sys, _ := body["system"].([]any)
		if len(sys) == 0 {
			return false
		}
		_, ok := sys[len(sys)-1].(map[string]any)["cache_control"]
		return ok
	}

	s := newClaudeServer(t)
	callClaude(t, open(t, s, opts(env)))
	if !systemMarked(s.body) {
		t.Error("default Open placed no cache breakpoint on the system block")
	}
	if _, ok := s.body["tools"]; ok {
		t.Errorf("default Open sent tools %v, want none", s.body["tools"])
	}

	o := opts(env)
	o.PromptCache = &anthropic.CacheOptions{}
	o.BuiltinTools = []string{"web_search"}
	callClaude(t, open(t, s, o))
	if systemMarked(s.body) {
		t.Error("a zero PromptCache still placed a breakpoint")
	}
	tools, _ := s.body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "web_search" {
		t.Errorf("tools = %v, want web_search", tools)
	}

	// A tool Claude does not have is refused at Open, as for gemini.
	o.BuiltinTools = []string{"web_search", "url_context"}
	if _, err := coremodels.Open(context.Background(), profile.Profile{Name: "claude", Extends: "anthropic", BaseURL: s.URL}, o); err == nil || !strings.Contains(err.Error(), "url_context") {
		t.Errorf("Open with url_context = %v, want it refused by name", err)
	}
}

// Vertex routing follows the profile's platform, not its credential. A
// proxy in front of Vertex that takes a bearer token is still Vertex,
// and a first-party gateway that takes Google tokens is not.
func TestOpenRoutesClaudeByPlatform(t *testing.T) {
	t.Run("vertex behind a bearer-token proxy", func(t *testing.T) {
		s := newClaudeServer(t)
		p, err := coremodels.Open(context.Background(), profile.Profile{
			Name: "claude-proxy", Extends: "anthropic-vertex",
			BaseURL: s.URL + "/v1/projects/{project}/locations/{region}/publishers/anthropic/models",
			Auth:    auth.Config{Kind: auth.Bearer, Env: "PROXY_TOKEN"},
		}, opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme", "PROXY_TOKEN": "tok"}))
		if err != nil {
			t.Fatal(err)
		}
		callClaude(t, p)
		if want := "/v1/projects/acme/locations/us-east5/publishers/anthropic/models/claude-haiku-4-5:streamRawPredict"; s.path != want {
			t.Errorf("path = %s, want the Vertex route %s", s.path, want)
		}
		if s.bearer != "Bearer tok" {
			t.Errorf("Authorization = %q", s.bearer)
		}
	})
	t.Run("first party behind a Google-token gateway", func(t *testing.T) {
		s := newClaudeServer(t)
		p, err := coremodels.Open(context.Background(), profile.Profile{
			Name: "claude-gw", Extends: "anthropic", BaseURL: s.URL,
			Auth: auth.Config{Kind: auth.GoogleADC},
		}, opts(nil))
		if err != nil {
			t.Fatal(err)
		}
		callClaude(t, p)
		if s.path != "/v1/messages" {
			t.Errorf("path = %s, want the first-party /v1/messages", s.path)
		}
		if s.bearer != "Bearer ya29.test" {
			t.Errorf("Authorization = %q", s.bearer)
		}
	})
}
