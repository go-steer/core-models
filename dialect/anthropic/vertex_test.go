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

package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/llm"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// vertexCapture is what a fake Vertex endpoint saw.
type vertexCapture struct {
	mu      sync.Mutex
	path    string
	auth    string
	apiKey  string
	version string
	body    map[string]any
}

// newVertexModel stands up a fake Vertex endpoint and returns a model
// routed to it the way coremodels.Open routes anthropic-vertex: a
// Google credential, Vertex set, and the publisher prefix as BaseURL.
func newVertexModel(t *testing.T, modelID string) (*model, *vertexCapture) {
	t.Helper()
	c := &vertexCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path = r.URL.Path
		c.auth = r.Header.Get("Authorization")
		c.apiKey = r.Header.Get("X-Api-Key")
		c.version = r.Header.Get("Anthropic-Version")
		_ = json.Unmarshal(raw, &c.body)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesSSEFixture)
	}))
	t.Cleanup(srv.Close)
	cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{
		DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) { return staticToken("adc-token"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := New(Options{
		BaseURL:    srv.URL + "/v1/projects/p1/locations/us-east5/publishers/anthropic/models",
		Credential: cred,
		Vertex:     true,
		Backend:    "anthropic-vertex",
		Region:     "us-east5",
	})
	if err != nil {
		t.Fatal(err)
	}
	return cl.Model(modelID).(*model), c
}

// TestVertex_RewritesTheRequest pins the wire contract Vertex AI serves
// Claude under: the model in the path, streamRawPredict for a stream,
// anthropic_version in the body, and a bearer token rather than an API
// key.
func TestVertex_RewritesTheRequest(t *testing.T) {
	t.Parallel()
	l, c := newVertexModel(t, "claude-opus-4-5@20251101")
	resp := terminalOf(t, l)

	if want := "/v1/projects/p1/locations/us-east5/publishers/anthropic/models/claude-opus-4-5@20251101:streamRawPredict"; c.path != want {
		t.Errorf("path = %q, want %q", c.path, want)
	}
	if _, ok := c.body["model"]; ok {
		t.Error("body still names the model; Vertex takes it from the path")
	}
	if got := c.body["anthropic_version"]; got != VertexVersion {
		t.Errorf("anthropic_version = %v, want %q", got, VertexVersion)
	}
	if c.auth != "Bearer adc-token" {
		t.Errorf("Authorization = %q, want the ADC bearer token", c.auth)
	}
	if c.apiKey != "" {
		t.Errorf("X-Api-Key = %q, want none on Vertex", c.apiKey)
	}
	if resp.ModelVersion != "claude-test" {
		t.Errorf("ModelVersion = %q, want the server's echo", resp.ModelVersion)
	}
	d := detailOf(t, resp)
	if d.Backend != "anthropic-vertex" || d.Region != "us-east5" {
		t.Errorf("Backend, Region = %q, %q; want anthropic-vertex, us-east5", d.Backend, d.Region)
	}
}

// TestVertex_NeedsABaseURL: there is no default Vertex endpoint to fall
// back on — it is per project and region.
func TestVertex_NeedsABaseURL(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{Vertex: true}); err == nil {
		t.Error("New accepted Vertex with no BaseURL")
	}
}

// TestFirstParty_SendsTheAPIKey: the first-party API authenticates with
// x-api-key, not a bearer token.
func TestFirstParty_SendsTheAPIKey(t *testing.T) {
	t.Parallel()
	var gotKey, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuth, gotPath = r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesSSEFixture)
	}))
	t.Cleanup(srv.Close)
	l := offlineModel(t, srv.URL, nil, "claude-test", BuiltinTools{})
	for _, err := range l.GenerateContent(context.Background(), &llm.Request{Contents: userText("hi")}, false) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if gotKey != "test-key-not-real" || gotAuth != "" || gotPath != "/v1/messages" {
		t.Errorf("x-api-key %q, Authorization %q, path %q; want the key, no bearer, /v1/messages", gotKey, gotAuth, gotPath)
	}
}

// TestTheEnvironmentDoesNotReachIn: ANTHROPIC_BASE_URL and
// ANTHROPIC_API_KEY are the SDK's defaults, and a profile is the whole
// configuration. A stray variable must not redirect a request.
func TestTheEnvironmentDoesNotReachIn(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("ANTHROPIC_API_KEY", "from-the-environment")
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesSSEFixture)
	}))
	t.Cleanup(srv.Close)
	l := offlineModel(t, srv.URL, nil, "claude-test", BuiltinTools{})
	for _, err := range l.GenerateContent(context.Background(), &llm.Request{Contents: userText("hi")}, false) {
		if err != nil {
			t.Fatalf("request went somewhere else: %v", err)
		}
	}
	if gotKey != "test-key-not-real" {
		t.Errorf("x-api-key = %q, want the credential's", gotKey)
	}
}
