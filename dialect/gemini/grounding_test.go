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

package gemini

import (
	"slices"
	"testing"

	"google.golang.org/genai"
)

func TestGroundingEvidence(t *testing.T) {
	t.Parallel()
	if got := GroundingEvidence(nil); got != nil {
		t.Errorf("nil metadata = %v", got)
	}
	if got := GroundingEvidence(&genai.GroundingMetadata{}); got != nil {
		t.Errorf("empty metadata = %v", got)
	}
	gm := &genai.GroundingMetadata{
		WebSearchQueries: []string{"kubernetes crashloop", "", "kubernetes crashloop"},
		GroundingChunks: []*genai.GroundingChunk{
			{Web: &genai.GroundingChunkWeb{Title: "Debug Pods", URI: "https://k8s.io/debug"}},
			{Web: &genai.GroundingChunkWeb{URI: "https://example.com/a"}},
			{Web: &genai.GroundingChunkWeb{Title: "Debug Pods", URI: "https://k8s.io/debug"}},
			{Web: &genai.GroundingChunkWeb{Title: "no uri"}},
			nil,
		},
	}
	want := []string{"query: kubernetes crashloop", "Debug Pods — https://k8s.io/debug", "https://example.com/a"}
	if got := GroundingEvidence(gm); !slices.Equal(got, want) {
		t.Errorf("evidence = %q, want %q", got, want)
	}
}
