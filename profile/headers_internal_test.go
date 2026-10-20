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

package profile

import (
	"maps"
	"strings"
	"testing"

	"github.com/go-steer/core-models/auth"
)

// TestHeadersExtendMergeByKey: a derived profile's headers lay over the
// built-in's key by key and case-insensitively, keep the ones it does
// not name, and drop one it sets empty. No built-in sets headers yet, so
// the test registers one for its own run.
func TestHeadersExtendMergeByKey(t *testing.T) {
	builtins["test-gateway"] = Profile{Name: "test-gateway", Dialect: OpenAIChat, BaseURL: "https://gw.example/v1",
		Auth: auth.Config{Kind: auth.None}, OpenModels: new(true),
		Headers: map[string]string{"ai-resource-group": "default", "X-Keep": "base", "X-Drop": "base"}}
	t.Cleanup(func() { delete(builtins, "test-gateway") })

	p, err := Expand(Profile{Name: "mine", Extends: "test-gateway", Headers: map[string]string{"AI-Resource-Group": "team-a", "x-drop": "", "X-New": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"AI-Resource-Group": "team-a", "X-Keep": "base", "x-drop": "", "X-New": "1"}
	if !maps.Equal(p.Headers, want) {
		t.Errorf("Expand headers = %v, want %v", p.Headers, want)
	}
	r, err := Resolve(t.Context(), Profile{Name: "mine", Extends: "test-gateway", Headers: map[string]string{"x-drop": ""}}, Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.HeaderNames(); len(got) != 2 || got[0] != "Ai-Resource-Group" || got[1] != "X-Keep" {
		t.Errorf("HeaderNames = %v, want the inherited headers less X-Drop", got)
	}
	if b, _ := Builtin("test-gateway"); b.Headers["ai-resource-group"] != "default" {
		t.Error("Expand modified the built-in's headers")
	}

	// Two spellings of one header in the derived profile are refused,
	// not collapsed in map order.
	_, err = Resolve(t.Context(), Profile{Name: "mine", Extends: "test-gateway", Headers: map[string]string{"X-Tenant": "a", "x-tenant": "b"}}, Options{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "are the same header") {
		t.Errorf("two spellings through extends: err = %v", err)
	}
}
