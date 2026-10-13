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

package adkv2_test

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/adkv2"
	"github.com/go-steer/core-models/dialect/gemini"
)

// TestGroundingProjectionAppendsTheEvidence: an event carrying Google
// Search grounding is stored, then followed by one synthetic event per
// query and per web source — deduplicated, skipping a URI-less source —
// under the parent's invocation and branch, with no role so ADK keeps
// them out of the model's history. An event without grounding gets
// nothing extra.
func TestGroundingProjectionAppendsTheEvidence(t *testing.T) {
	ctx := context.Background()
	svc := adkv2.GroundingProjection(session.InMemoryService())
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	sess := created.Session

	plain := session.NewEvent(ctx, "inv-0")
	plain.Author = "agent"
	plain.Content = genai.NewContentFromText("no grounding here", genai.RoleModel)
	if err := svc.AppendEvent(ctx, sess, plain); err != nil {
		t.Fatal(err)
	}

	grounded := session.NewEvent(ctx, "inv-1")
	grounded.Author = "agent"
	grounded.Branch = "root.analyst"
	grounded.Content = genai.NewContentFromText("Go 1.27.2 is current.", genai.RoleModel)
	grounded.GroundingMetadata = &genai.GroundingMetadata{
		WebSearchQueries: []string{"latest go release", "latest go release", ""},
		GroundingChunks: []*genai.GroundingChunk{
			{Web: &genai.GroundingChunkWeb{Title: "Release History", URI: "https://go.dev/doc/devel/release"}},
			{Web: &genai.GroundingChunkWeb{Title: "Release History", URI: "https://go.dev/doc/devel/release"}},
			{Web: &genai.GroundingChunkWeb{Title: "no uri"}},
			{Web: &genai.GroundingChunkWeb{URI: "https://go.dev/dl/"}},
		},
	}
	if err := svc.AppendEvent(ctx, sess, grounded); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, &session.GetRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var events []*session.Event
	for ev := range got.Session.Events().All() {
		events = append(events, ev)
	}
	want := []string{
		"query: latest go release",
		"Release History — https://go.dev/doc/devel/release",
		"https://go.dev/dl/",
	}
	if len(events) != 2+len(want) {
		t.Fatalf("%d events stored, want the 2 originals plus %d synthetic", len(events), len(want))
	}
	for i, line := range want {
		syn := events[2+i]
		if syn.Author != gemini.AuthorGoogleSearch || syn.InvocationID != "inv-1" || syn.Branch != "root.analyst" {
			t.Errorf("synthetic %d: author %q invocation %q branch %q", i, syn.Author, syn.InvocationID, syn.Branch)
		}
		if syn.Content == nil || syn.Content.Role != "" || len(syn.Content.Parts) != 1 || syn.Content.Parts[0].Text != line {
			t.Errorf("synthetic %d content = %+v, want role-less %q", i, syn.Content, line)
		}
		if syn.ID == "" || syn.ID == grounded.ID {
			t.Errorf("synthetic %d has ID %q; each needs its own", i, syn.ID)
		}
	}
}
