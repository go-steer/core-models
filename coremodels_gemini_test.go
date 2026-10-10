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
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
	"github.com/go-steer/core-models/usage"
)

type geminiCall struct {
	path, key, auth string
	body            map[string]any
}

func geminiServer(t *testing.T) (*httptest.Server, *[]geminiCall) {
	t.Helper()
	var calls []geminiCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		calls = append(calls, geminiCall{r.URL.Path, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"), body})
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4},"modelVersion":"gemini-3.6-flash"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func callOnce(t *testing.T, p coremodels.Provider, id string) *llm.Response {
	t.Helper()
	m, err := p.Model(context.Background(), id)
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
	return final
}

// TestOpenTheGeminiBuiltins: the gemini profile is the Developer API by
// API key (either of its two names), the vertex profile is Vertex AI by
// ADC at the global location unless told otherwise, and both price
// under the backend names the products' catalogs use.
func TestOpenTheGeminiBuiltins(t *testing.T) {
	srv, calls := geminiServer(t)

	api := profile.Profile{Name: "g", Extends: "gemini", BaseURL: srv.URL}
	p, err := coremodels.Open(context.Background(), api, opts(map[string]string{"GEMINI_API_KEY": "k-2"}))
	if err != nil {
		t.Fatal(err)
	}
	r := callOnce(t, p, "gemini-3.6-flash")
	if c := (*calls)[0]; c.key != "k-2" || c.auth != "" || !strings.HasSuffix(c.path, "models/gemini-3.6-flash:generateContent") {
		t.Errorf("developer API call = %+v", c)
	}
	if d, _ := usage.FromMetadata(r.CustomMetadata); p.Backend() != "gemini" || d.Backend != "gemini" {
		t.Errorf("backend %q, detail %+v", p.Backend(), d)
	}

	vx := profile.Profile{Name: "vx", Extends: "vertex", BaseURL: srv.URL}
	p, err = coremodels.Open(context.Background(), vx, opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	r = callOnce(t, p, "gemini-3.6-flash")
	if c := (*calls)[1]; c.auth != "Bearer ya29.test" || c.key != "" || !strings.Contains(c.path, "projects/acme/locations/global/publishers/google/models/gemini-3.6-flash") {
		t.Errorf("vertex call = %+v", c)
	}
	if d, _ := usage.FromMetadata(r.CustomMetadata); p.Backend() != "vertex" || d.Backend != "vertex" || d.Region != "global" {
		t.Errorf("backend %q, detail %+v", p.Backend(), d)
	}
}

func TestOpenGeminiWithBuiltinTools(t *testing.T) {
	srv, calls := geminiServer(t)
	o := opts(map[string]string{"GOOGLE_API_KEY": "k"})
	o.BuiltinTools = []string{"web_search"}
	p, err := coremodels.Open(context.Background(), profile.Profile{Name: "g", Extends: "gemini", BaseURL: srv.URL}, o)
	if err != nil {
		t.Fatal(err)
	}
	callOnce(t, p, "gemini-3.6-flash")
	tools, _ := (*calls)[0].body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["googleSearch"] == nil {
		t.Errorf("tools sent = %v, want google_search", tools)
	}

	for name, tc := range map[string]struct {
		p     profile.Profile
		tools []string
		want  string
	}{
		"unknown built-in":         {profile.Profile{Name: "g", Extends: "gemini"}, []string{"google_maps"}, "unknown built-in"},
		"a dialect with none":      {profile.Profile{Name: "m", Extends: "vertex-maas"}, []string{"web_search"}, "no server-side tools"},
		"no key under either name": {profile.Profile{Name: "g", Extends: "gemini"}, nil, "GOOGLE_API_KEY or GEMINI_API_KEY is not set"},
	} {
		t.Run(name, func(t *testing.T) {
			o := opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme"})
			if name != "no key under either name" {
				o = opts(map[string]string{"GOOGLE_CLOUD_PROJECT": "acme", "GOOGLE_API_KEY": "k"})
			}
			o.BuiltinTools = tc.tools
			if _, err := coremodels.Open(context.Background(), tc.p, o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open = %v, want %q", err, tc.want)
			}
		})
	}
}
