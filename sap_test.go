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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/profile"
)

// fakeSAPTenant is one SAP AI Core tenant end to end: the token endpoint
// (XSUAA), the AI API's deployment list and an orchestration deployment
// that answers like a model would. It stands in for a recorded corpus
// until one from a real tenant exists (core-models #83): the envelope
// it checks and the answers it gives follow SAP's Orchestration v2 spec
// and the fixtures in SAP's Cloud SDK for AI.
type fakeSAPTenant struct {
	srv *httptest.Server
	ttl int // expires_in the token endpoint grants

	mu      sync.Mutex
	issued  int
	seen    []string // "METHOD path token resource-group" per API request
	problem string   // the first envelope check that failed
}

func newFakeSAPTenant(t *testing.T, ttl int) *fakeSAPTenant {
	f := &fakeSAPTenant{ttl: ttl}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSAPTenant) serviceKey() string {
	return fmt.Sprintf(`{"credentials":{"clientid":"sb-x","clientsecret":"shh","url":%q,"serviceurls":{"AI_API_URL":%q}}}`, f.srv.URL, f.srv.URL)
}

func (f *fakeSAPTenant) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/oauth/token" {
		f.issued++
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"bearer","expires_in":%d}`, f.issued, f.ttl)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.seen = append(f.seen, fmt.Sprintf("%s %s %s %s", r.Method, r.URL.Path, tok, r.Header.Get("AI-Resource-Group")))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/lm/deployments":
		_, _ = fmt.Fprintf(w, `{"count":1,"resources":[{"id":"d1","deploymentUrl":"%s/v2/inference/deployments/d1","scenarioId":"orchestration","status":"RUNNING","createdAt":"2026-10-01T00:00:00Z"}]}`, f.srv.URL)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/inference/deployments/d1/v2/completion":
		f.complete(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// complete answers as a model would for the shared scenarios: a tool
// call when tools are offered and none has run, an answer from the tool
// result once one has, pong when asked, a count otherwise.
func (f *fakeSAPTenant) complete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config struct {
			Modules struct {
				PT struct {
					Prompt struct {
						Template []map[string]any `json:"template"`
						Tools    []any            `json:"tools"`
					} `json:"prompt"`
					Model struct {
						Name string `json:"name"`
					} `json:"model"`
				} `json:"prompt_templating"`
			} `json:"modules"`
			Stream *struct {
				Enabled bool `json:"enabled"`
			} `json:"stream"`
		} `json:"config"`
		History []map[string]any `json:"messages_history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail("body is not JSON: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	pt := body.Config.Modules.PT
	if len(pt.Prompt.Template) != 1 || pt.Model.Name == "" {
		f.fail("template %v, model %q: want one template message and a model", pt.Prompt.Template, pt.Model.Name)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	msgs := slices.Concat(body.History, pt.Prompt.Template)
	var answer, call string
	last, _ := pt.Prompt.Template[0]["content"].(string)
	switch {
	case hasRole(msgs, "tool"):
		answer = "Pod web-7 in shop is in CrashLoopBackOff with 14 restarts."
	case len(pt.Prompt.Tools) > 0:
		call = `{"index":0,"id":"call_sap1","type":"function","function":{"name":"get_pod_status","arguments":"{\"pod\":\"web-7\",\"namespace\":\"shop\"}"}}`
	case strings.Contains(last, "pong"):
		answer = "pong"
	default:
		answer = "1, 2, 3, 4, 5"
	}
	usage := `{"completion_tokens":12,"prompt_tokens":40,"total_tokens":52}`
	if body.Config.Stream != nil && body.Config.Stream.Enabled {
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(delta, finish, usage string) {
			u := ""
			if usage != "" {
				u = `,"usage":` + usage
			}
			_, _ = fmt.Fprintf(w, `data: {"request_id":"sap-req","intermediate_results":{},"final_result":{"id":"c1","object":"chat.completion.chunk","created":1,"model":"%s-2026","choices":[{"index":0,"delta":%s,"finish_reason":%q}]%s}}`+"\n\n", pt.Model.Name, delta, finish, u)
		}
		ev(`{"role":"","content":""}`, "", "")
		half := len(answer) / 2
		ev(fmt.Sprintf(`{"role":"assistant","content":%q}`, answer[:half]), "", "")
		ev(fmt.Sprintf(`{"role":"assistant","content":%q}`, answer[half:]), "stop", usage)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	msg := fmt.Sprintf(`{"role":"assistant","content":%q}`, answer)
	finish := "stop"
	if call != "" {
		msg = `{"role":"assistant","tool_calls":[` + call + `]}`
		finish = "tool_calls"
	}
	_, _ = fmt.Fprintf(w, `{"request_id":"sap-req","intermediate_results":{},"final_result":{"id":"c1","object":"chat.completion","created":1,"model":"%s-2026","choices":[{"index":0,"message":%s,"finish_reason":%q}],"usage":%s}}`, pt.Model.Name, msg, finish, usage)
}

func hasRole(msgs []map[string]any, role string) bool {
	for _, m := range msgs {
		if m["role"] == role {
			return true
		}
	}
	return false
}

func (f *fakeSAPTenant) fail(format string, args ...any) {
	if f.problem == "" {
		f.problem = fmt.Sprintf(format, args...)
	}
}

func openSAP(t *testing.T, f *fakeSAPTenant, env map[string]string) coremodels.Provider {
	t.Helper()
	p, err := profile.Find("sap-ai-core", nil)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := coremodels.Open(context.Background(), p, coremodels.Options{Resolve: profile.Options{
		Getenv: func(k string) string { return env[k] },
	}})
	if err != nil {
		t.Fatal(err)
	}
	return prov
}

// The shared scenarios pass through the sap-ai-core built-in, opened
// from a service key alone, against the fake tenant.
func TestSAPAICoreScenarios(t *testing.T) {
	f := newFakeSAPTenant(t, 3600)
	prov := openSAP(t, f, map[string]string{"AICORE_SERVICE_KEY": f.serviceKey(), "AICORE_RESOURCE_GROUP": "team-a"})
	if prov.Backend() != "sap-ai-core" {
		t.Errorf("backend = %q", prov.Backend())
	}
	m, err := prov.Model(context.Background(), "anthropic--claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, m) })
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.problem != "" {
		t.Error(f.problem)
	}
	// The deployment was found once, at Open, and every request carried
	// the token and the resource group.
	if len(f.seen) == 0 || !strings.HasPrefix(f.seen[0], "GET /v2/lm/deployments tok-1 team-a") {
		t.Errorf("first request = %v", f.seen)
	}
	for _, s := range f.seen[1:] {
		if !strings.HasPrefix(s, "POST /v2/inference/deployments/d1/v2/completion tok-1 team-a") {
			t.Errorf("request %q", s)
		}
	}
}

// A session outlives its token: once the first expires, the next request
// carries a new one.
func TestSAPAICoreTokenRefresh(t *testing.T) {
	f := newFakeSAPTenant(t, 1)
	prov := openSAP(t, f, map[string]string{"AICORE_SERVICE_KEY": f.serviceKey()})
	m, err := prov.Model(context.Background(), "gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	scenarios[0].run(t, m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.seen[len(f.seen)-1]; !strings.Contains(last, " tok-2 default") {
		t.Errorf("request after expiry = %q, want the second token and the default resource group", last)
	}
}

// A tenant with no running orchestration deployment fails at Open.
func TestSAPAICoreNoDeploymentFailsAtOpen(t *testing.T) {
	f := newFakeSAPTenant(t, 3600)
	p, _ := profile.Find("sap-ai-core", nil)
	_, err := coremodels.Open(context.Background(), p, coremodels.Options{Resolve: profile.Options{
		Getenv: func(k string) string {
			return map[string]string{"AICORE_SERVICE_KEY": f.serviceKey(), "AICORE_DEPLOYMENT_ID": "d-gone"}[k]
		},
	}})
	if err == nil || !strings.Contains(err.Error(), `profile "sap-ai-core"`) {
		t.Fatalf("Open = %v", err)
	}
}
