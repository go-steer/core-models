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

package toolwire_test

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/toolwire"
)

func entry(t *testing.T, d *genai.FunctionDeclaration) toolwire.Entry {
	t.Helper()
	e, err := toolwire.EntryFor("test", d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEntryForBothSpellings(t *testing.T) {
	typed := entry(t, &genai.FunctionDeclaration{Name: "a", Parameters: &genai.Schema{
		Type: genai.TypeObject, Required: []string{"x"},
		Properties: map[string]*genai.Schema{"x": {Type: genai.TypeString}, "y": {Type: genai.TypeInteger}},
	}})
	raw := entry(t, &genai.FunctionDeclaration{Name: "b", ParametersJsonSchema: map[string]any{
		"type": "object", "required": []string{"y"},
		"properties": map[string]any{"y": map[string]any{"type": "string"}},
	}})
	none := entry(t, &genai.FunctionDeclaration{Name: "c"})
	if typed.Spelling != "Parameters" || strings.Join(typed.Props, ",") != "x,y" || strings.Join(typed.Required, ",") != "x" {
		t.Errorf("typed = %+v", typed)
	}
	if raw.Spelling != "ParametersJsonSchema" || strings.Join(raw.Props, ",") != "y" || strings.Join(raw.Required, ",") != "y" {
		t.Errorf("raw = %+v", raw)
	}
	if none.Spelling != "none" || none.HasParams() {
		t.Errorf("none = %+v", none)
	}
}

// Every defect class must be reported, or a converter that commits it
// would pass. This is Verify's own anti-vacuity floor.
func TestVerifyReportsEachDefect(t *testing.T) {
	e := entry(t, &genai.FunctionDeclaration{Name: "kubectl", ParametersJsonSchema: map[string]any{
		"type": "object", "required": []string{"verb"},
		"properties": map[string]any{"verb": map[string]any{"type": "string"}, "ns": map[string]any{"type": "string"}},
	}})
	noArgs := entry(t, &genai.FunctionDeclaration{Name: "now"})
	good := toolwire.Wire{
		"kubectl": {"type": "object", "required": []any{"verb"}, "properties": map[string]any{
			"verb": map[string]any{"type": "string"}, "ns": map[string]any{"type": "string"},
		}},
		"now": {"type": "object", "properties": map[string]any{}},
	}
	if p := toolwire.Verify([]toolwire.Entry{e, noArgs}, good); len(p) != 0 {
		t.Fatalf("a faithful wire was reported: %v", p)
	}
	for name, tc := range map[string]struct {
		wire toolwire.Wire
		want string
	}{
		"missing tool":      {toolwire.Wire{}, "not on the wire"},
		"empty properties":  {toolwire.Wire{"kubectl": {"type": "object"}}, "mast #154"},
		"dropped argument":  {toolwire.Wire{"kubectl": {"required": []any{"verb"}, "properties": map[string]any{"verb": map[string]any{"type": "string"}}}}, `"ns" is missing`},
		"emptied argument":  {toolwire.Wire{"kubectl": {"required": []any{"verb"}, "properties": map[string]any{"verb": map[string]any{}, "ns": map[string]any{"type": "string"}}}}, "its own schema was emptied"},
		"invented argument": {toolwire.Wire{"kubectl": {"required": []any{"verb"}, "properties": map[string]any{"verb": map[string]any{"type": "string"}, "ns": map[string]any{"type": "string"}, "extra": map[string]any{"type": "string"}}}}, "advertises [extra]"},
		"demoted required":  {toolwire.Wire{"kubectl": {"properties": map[string]any{"verb": map[string]any{"type": "string"}, "ns": map[string]any{"type": "string"}}}}, "are optional on the wire"},
	} {
		t.Run(name, func(t *testing.T) {
			p := toolwire.Verify([]toolwire.Entry{e}, tc.wire)
			if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Errorf("problems = %v, want one containing %q", p, tc.want)
			}
		})
	}
	invented := toolwire.Wire{"now": {"properties": map[string]any{"x": map[string]any{"type": "string"}}}}
	if p := toolwire.Verify([]toolwire.Entry{noArgs}, invented); len(p) == 0 {
		t.Error("properties invented for a no-argument tool were not reported")
	}
	if !strings.Contains(toolwire.Summary([]toolwire.Entry{e}), "kubectl") {
		t.Error("Summary does not name the tool")
	}
}
