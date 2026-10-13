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

// Originally derived from go-steer/mast@4dde466b:internal/providers/gemini/projection.go (itself from go-steer/core-agent@b8dd225e)

package adkv2

import (
	"context"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/dialect/gemini"
)

// GroundingProjection wraps a session.Service so that every event
// carrying Gemini Google Search grounding is followed by synthetic
// events surfacing that evidence: one per search query the model
// issued, then one per grounded web source, each authored
// gemini.AuthorGoogleSearch. The lines come from
// gemini.GroundingEvidence, the ADK-free half of this projection.
//
// The synthetic events share the parent's InvocationID and Branch, so
// event-log queries by invocation, author or branch find them. Their
// Content.Role is empty, so ADK's content processor leaves them out of
// the conversation it sends the model: they are for audit and display,
// not context.
//
// Wrap the service before handing it to the runner:
//
//	svc = adkv2.GroundingProjection(svc)
func GroundingProjection(inner session.Service) session.Service {
	return &groundingProjection{inner: inner}
}

type groundingProjection struct {
	inner session.Service
}

func (g *groundingProjection) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return g.inner.Create(ctx, req)
}

func (g *groundingProjection) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return g.inner.Get(ctx, req)
}

func (g *groundingProjection) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return g.inner.List(ctx, req)
}

func (g *groundingProjection) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return g.inner.Delete(ctx, req)
}

func (g *groundingProjection) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if err := g.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	if ev == nil {
		return nil
	}
	for _, line := range gemini.GroundingEvidence(ev.GroundingMetadata) {
		// ctx, not context.Background: ADK v2's NewEvent draws the
		// event's ID and timestamp from providers installed on the
		// context, which is what deterministic replay relies on.
		syn := session.NewEvent(ctx, ev.InvocationID)
		syn.Author = gemini.AuthorGoogleSearch
		syn.Branch = ev.Branch
		syn.Content = &genai.Content{Parts: []*genai.Part{{Text: line}}}
		if err := g.inner.AppendEvent(ctx, sess, syn); err != nil {
			return err
		}
	}
	return nil
}
