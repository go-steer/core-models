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
		"unbuilt dialect": {profile.Profile{Name: "claude", Dialect: profile.Anthropic, Auth: auth.Config{Kind: auth.None},
			OpenModels: new(true)}, nil, "not built yet"},
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
