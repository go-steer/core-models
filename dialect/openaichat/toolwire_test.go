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

package openaichat_test

import (
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/toolwire"
)

// catalog is the declaration shapes a real agent hands a model: both
// schema spellings, nested objects and arrays, enums, required and
// optional arguments, and a tool with none. Products also run their own
// captured catalogs through this adapter; this one keeps the library
// honest on its own.
func catalog() []*genai.FunctionDeclaration {
	return []*genai.FunctionDeclaration{
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
}

func TestEveryToolArrivesWhole(t *testing.T) {
	decls := catalog()
	var entries []toolwire.Entry
	for _, d := range decls {
		e, err := toolwire.EntryFor("openaichat", d)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}

	s := newServer(t, reply{body: okReply})
	complete(t, client(t, s, nil).Model("m"), &llm.Request{
		Contents: []*genai.Content{userText("hi")},
		Config:   &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: decls}}},
	})

	// The wire spelling for this dialect: tools[].function.parameters.
	wire := toolwire.Wire{}
	for _, raw := range s.body(0)["tools"].([]any) {
		fn := raw.(map[string]any)["function"].(map[string]any)
		schema, _ := fn["parameters"].(map[string]any)
		wire[fn["name"].(string)] = schema
	}
	if len(wire) != len(entries) {
		t.Fatalf("captured %d tools, declared %d — the capture is not measuring the catalog", len(wire), len(entries))
	}
	if problems := toolwire.Verify(entries, wire); len(problems) > 0 {
		t.Errorf("tools did not arrive whole:\n%s\ncatalog:\n%s", problems, toolwire.Summary(entries))
	}
}
