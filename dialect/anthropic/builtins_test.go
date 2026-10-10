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

// Originally derived from go-steer/core-agent@615ea69dab42733ee4aed0cae80139b00a3803b5:pkg/models/anthropic/builtins_test.go

package anthropic

import (
	"context"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/llm"
)

func TestBuiltinTools_ZeroValueIsAllOff(t *testing.T) {
	t.Parallel()
	if (BuiltinTools{}).WebSearch {
		t.Errorf("WebSearch should be OFF in the zero value — opt-in due to per-search billing")
	}
}

func TestBuiltinTools_AsAnthropicTools_Empty(t *testing.T) {
	t.Parallel()
	if got := (BuiltinTools{}).asAnthropicTools(); len(got) != 0 {
		t.Errorf("zero-value should produce no tools, got %d", len(got))
	}
}

func TestBuiltinTools_AsAnthropicTools_WebSearchOn(t *testing.T) {
	t.Parallel()
	got := BuiltinTools{WebSearch: true}.asAnthropicTools()
	if len(got) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got))
	}
	if got[0].OfWebSearchTool20260209 == nil {
		t.Errorf("expected OfWebSearchTool20260209 to be set, got %+v", got[0])
	}
}

// TestModel_ReportsItsBuiltins: the model a product holds reports what
// it will send, under the neutral names (mast #340).
func TestModel_ReportsItsBuiltins(t *testing.T) {
	t.Parallel()
	off, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := off.Model("claude-test").(interface{ BuiltinToolNames() []string })
	if !ok {
		t.Fatal("model does not report BuiltinToolNames")
	}
	if got := r.BuiltinToolNames(); len(got) != 0 {
		t.Errorf("default model reports %v, want nothing", got)
	}
	on, err := New(Options{BuiltinTools: BuiltinTools{WebSearch: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := on.Model("claude-test").(interface{ BuiltinToolNames() []string }).BuiltinToolNames()
	if len(got) != 1 || got[0] != "web_search" {
		t.Errorf("BuiltinToolNames = %v, want [web_search]", got)
	}
}

func TestBuildParams_AppendsWebSearchToTools(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name: "search", Description: "user-defined",
			}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, CacheOptions{}, BuiltinTools{WebSearch: true})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Tools) != 2 {
		t.Fatalf("expected 2 tools (1 function decl + 1 web_search), got %d", len(p.Tools))
	}
	// Function declarations come first; web_search is appended.
	if p.Tools[0].OfTool == nil || p.Tools[0].OfTool.Name != "search" {
		t.Errorf("first tool should be the function decl, got %+v", p.Tools[0])
	}
	if p.Tools[1].OfWebSearchTool20260209 == nil {
		t.Errorf("second tool should be web_search, got %+v", p.Tools[1])
	}
}

func TestBuildParams_NoBuiltinsWhenAllOff(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "search"}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, CacheOptions{}, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Tools) != 1 {
		t.Errorf("expected 1 tool (function decl only), got %d", len(p.Tools))
	}
}

// TestGenerateContent_WithoutBuiltinsDropsWebSearch: a one-shot side
// call on the shared model (core-agent's auto-mode approver, #1175) must go out
// with no server-side tools, even when the deployment turned
// web_search on for the agent loop. Same llm, same request; only the
// context differs.
func TestGenerateContent_WithoutBuiltinsDropsWebSearch(t *testing.T) {
	t.Parallel()
	req := &llm.Request{
		Contents: []*genai.Content{genai.NewContentFromText("judge this", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{},
	}

	l, captured := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	l.builtins = BuiltinTools{WebSearch: true}
	drain(t, l, context.Background(), req)
	if tools, _ := captured.body["tools"].([]any); len(tools) == 0 {
		t.Fatal("baseline request carried no tools; the opt-out test proves nothing")
	}

	l2, captured2 := newOfflineLLM(t, "claude-test", cacheWarmingSSEFixture)
	l2.builtins = BuiltinTools{WebSearch: true}
	drain(t, l2, callctx.WithoutBuiltins(context.Background()), req)
	if tools, ok := captured2.body["tools"]; ok {
		t.Errorf("suppressed request carried tools %v, want none", tools)
	}
}
