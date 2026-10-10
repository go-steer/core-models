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

// Originally derived from go-steer/mast@6568618f531368b8fa4894645df292b1e2aca09f:internal/providers/anthropic/toolwire_test.go

package anthropic

import (
	"context"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/toolwire"
)

// TestEveryToolArrivesWhole is the toolwire invariant over this
// dialect: whatever a product declares, Claude is shown the arguments
// it accepts. mast #154 shipped a release in which every ADK- and
// MCP-built tool reached Claude as a bare name with an empty input
// schema, because only the typed genai.Schema spelling was handled; this
// states the invariant over both spellings rather than for the one tool
// that was noticed (mast #168).
func TestEveryToolArrivesWhole(t *testing.T) {
	t.Parallel()
	decls := []*genai.FunctionDeclaration{
		{Name: "kubectl", Description: "run kubectl", Parameters: &genai.Schema{
			Type: genai.TypeObject, Required: []string{"verb", "resource"},
			Properties: map[string]*genai.Schema{
				"verb":      {Type: genai.TypeString, Enum: []string{"get", "describe"}},
				"resource":  {Type: genai.TypeString},
				"namespace": {Type: genai.TypeString},
				"labels":    {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
			},
		}},
		{Name: "read_file", ParametersJsonSchema: map[string]any{
			"type": "object", "required": []any{"path"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "file to read"},
				"limit":  map[string]any{"type": "integer", "minimum": 1},
				"filter": map[string]any{"type": "object", "properties": map[string]any{"glob": map[string]any{"type": "string"}}},
			},
		}},
		{Name: "finish_task", Description: "no arguments"},
	}
	var entries []toolwire.Entry
	for _, d := range decls {
		e, err := toolwire.EntryFor("anthropic", d)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}

	// Through GenerateContent, reading the request body: what Anthropic
	// receives is the thing that was wrong in #154, not toolsParam.
	l, captured := newOfflineLLM(t, "claude-test", messagesSSEFixture)
	for _, err := range l.GenerateContent(context.Background(), &llm.Request{
		Contents: userText("enumerate the tools"),
		Config:   &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: decls}}},
	}, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}

	// The wire spelling for this dialect: tools[].input_schema, which
	// Anthropic requires to be an object schema.
	wire := toolwire.Wire{}
	for i, raw := range captured.body["tools"].([]any) {
		entry := raw.(map[string]any)
		name, _ := entry["name"].(string)
		schema, ok := entry["input_schema"].(map[string]any)
		if !ok {
			t.Errorf("tools[%d] %s: input_schema is %T, want an object", i, name, entry["input_schema"])
			continue
		}
		if got, _ := schema["type"].(string); got != "object" {
			t.Errorf("%s: input_schema type is %q, want \"object\"", name, got)
		}
		wire[name] = schema
	}
	if len(wire) != len(entries) {
		t.Fatalf("captured %d tools, declared %d — the capture is not measuring the catalog", len(wire), len(entries))
	}
	if problems := toolwire.Verify(entries, wire); len(problems) > 0 {
		t.Errorf("tools did not arrive whole:\n%s\ncatalog:\n%s", problems, toolwire.Summary(entries))
	}
}
