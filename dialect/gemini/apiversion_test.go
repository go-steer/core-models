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
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/dialect/gemini"
	"github.com/go-steer/core-models/llm"
)

// TestAPIVersionPath: genai v1.78 moved the Developer API's default from
// v1beta to v1. The adapter keeps v1beta unless GOOGLE_GENAI_API_VERSION
// says otherwise, and leaves Vertex AI on genai's v1beta1.
func TestAPIVersionPath(t *testing.T) {
	for _, tc := range []struct {
		name, env, wantPrefix string
		vertex                bool
	}{
		{name: "developer API default", wantPrefix: "/v1beta/models/"},
		{name: "developer API opts in to v1", env: "v1", wantPrefix: "/v1/models/"},
		{name: "vertex AI unchanged", vertex: true, wantPrefix: "/v1beta1/projects/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_GENAI_API_VERSION", tc.env)
			s := &server{steps: []func(http.ResponseWriter, *http.Request){jsonReply(http.StatusOK, answer)}}
			var c *gemini.Client
			if tc.vertex {
				c = vertexClient(t, s, gemini.BuiltinTools{})
			} else {
				c = apiClient(t, s, gemini.Options{})
			}
			req := &llm.Request{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
			if _, err := collect(t, c.Model("gemini-3.6-flash"), context.Background(), req, false); err != nil {
				t.Fatal(err)
			}
			if got := s.paths[0]; !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("path = %q, want prefix %q", got, tc.wantPrefix)
			}
		})
	}
}
