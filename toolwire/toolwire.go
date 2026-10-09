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

// Originally derived from go-steer/mast@c19d0c0:internal/toolcatalog/verify.go and catalog.go (entryFor).

// Package toolwire states the invariant every provider adapter is held
// to: each tool a model is offered arrives on the wire with every
// argument it declares, with the required ones still required, and with
// each argument's own schema intact.
//
// mast #154 shipped a release in which every tool reached Claude as a
// name with an empty schema; it passed two judged nightlies because
// nothing compared what the model was handed against what was declared.
// The comparison lives here, shared, so no adapter is held to a weaker
// check than its siblings. An adapter supplies one thing: a reader that
// pulls its own wire spelling out of a captured request body into a
// Wire.
//
// Products keep the other half — the catalog captured from their own
// agent rigs, which cannot move here — and run it through the library's
// adapters with this Verify.
package toolwire

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/genai"
)

// Wire is what an adapter actually sent, read back out of the captured
// request body: tool name -> the JSON schema object its arguments were
// described by.
type Wire map[string]map[string]any

// Entry is one declared tool and the facts derived from its
// declaration.
type Entry struct {
	// Name is the function name the model calls.
	Name string
	// Spelling records which declaration field carried the schema —
	// "Parameters" (typed *genai.Schema), "ParametersJsonSchema", or
	// "none". It names the converter branch in a failure.
	Spelling string
	// Rig names where the tool came from, so a failure says where to
	// look.
	Rig string
	// Declaration is the genai declaration, verbatim.
	Declaration *genai.FunctionDeclaration
	// Props are the argument names the declaration advertises, sorted.
	// Empty means the tool takes no arguments.
	Props []string
	// Required are the argument names marked required, sorted.
	Required []string
}

// HasParams reports whether the tool advertises any arguments.
func (e Entry) HasParams() bool { return len(e.Props) > 0 }

// EntryFor derives an Entry from a declaration, so expectations come
// from the declaration itself and cannot go stale beside it.
func EntryFor(rig string, d *genai.FunctionDeclaration) (Entry, error) {
	e := Entry{Name: d.Name, Rig: rig, Declaration: d}
	var src any
	switch {
	case d.Parameters != nil:
		e.Spelling = "Parameters"
		src = d.Parameters
	case d.ParametersJsonSchema != nil:
		e.Spelling = "ParametersJsonSchema"
		src = d.ParametersJsonSchema
	default:
		e.Spelling = "none"
		return e, nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return Entry{}, fmt.Errorf("toolwire: tool %q: marshal %s: %w", d.Name, e.Spelling, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return Entry{}, fmt.Errorf("toolwire: tool %q: unmarshal %s: %w", d.Name, e.Spelling, err)
	}
	if props, ok := generic["properties"].(map[string]any); ok {
		for k := range props {
			e.Props = append(e.Props, k)
		}
	}
	if req, ok := generic["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				e.Required = append(e.Required, s)
			}
		}
	}
	sort.Strings(e.Props)
	sort.Strings(e.Required)
	return e, nil
}

// Verify compares what each declaration promised against what the
// adapter emitted, and returns one line per violation — empty when the
// conversion was faithful. Each check is a real outage, not a cosmetic
// mismatch:
//
//   - Every tool is on the wire. A tool never shown cannot be called.
//   - A tool with arguments arrives with a non-empty properties map
//     (mast #154 exactly).
//   - Every argument name survives, and none is invented.
//   - Every required argument is still required; demoted, the model may
//     omit it and the tool fails at dispatch.
//   - Each argument's own schema survives as a non-empty object.
func Verify(catalog []Entry, wire Wire) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	for _, e := range catalog {
		schema, ok := wire[e.Name]
		if !ok {
			report("%s (%s, %s): not on the wire at all — the model is never shown this tool", e.Name, e.Rig, e.Spelling)
			continue
		}

		props, _ := schema["properties"].(map[string]any)
		if e.HasParams() && len(props) == 0 {
			report("%s (%s, %s): declares %v but the emitted schema has no properties — the model can only call it with {} (the mast #154 shape)",
				e.Name, e.Rig, e.Spelling, e.Props)
			continue
		}
		if !e.HasParams() && len(props) > 0 {
			report("%s (%s, %s): declares no arguments but the emitted schema invented %v",
				e.Name, e.Rig, e.Spelling, sortedKeys(props))
			continue
		}

		for _, want := range e.Props {
			sub, present := props[want]
			if !present {
				report("%s (%s, %s): argument %q is missing from the emitted schema — the model cannot send it (emitted %v)",
					e.Name, e.Rig, e.Spelling, want, sortedKeys(props))
				continue
			}
			if obj, ok := sub.(map[string]any); !ok || len(obj) == 0 {
				report("%s (%s, %s): argument %q survived as %v — its own schema was emptied, so the model is guessing at its type",
					e.Name, e.Rig, e.Spelling, want, sub)
			}
		}
		if extra := difference(sortedKeys(props), e.Props); len(extra) > 0 {
			report("%s (%s, %s): the emitted schema advertises %v, which the declaration does not",
				e.Name, e.Rig, e.Spelling, extra)
		}

		if missing := difference(e.Required, wireRequired(schema)); len(missing) > 0 {
			report("%s (%s, %s): required argument(s) %v are optional on the wire — the model may omit them and the tool fails at dispatch",
				e.Name, e.Rig, e.Spelling, missing)
		}
	}

	sort.Strings(problems)
	return problems
}

// Summary renders a catalog as one line per tool, so a failure says
// what was actually checked.
func Summary(catalog []Entry) string {
	var b strings.Builder
	for _, e := range catalog {
		fmt.Fprintf(&b, "  %-24s rig=%-16s spelling=%-21s args=%v required=%v\n",
			e.Name, e.Rig, e.Spelling, e.Props, e.Required)
	}
	return b.String()
}

func wireRequired(schema map[string]any) []string {
	var out []string
	switch req := schema["required"].(type) {
	case []any:
		for _, v := range req {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, req...)
	}
	sort.Strings(out)
	return out
}

func difference(want, have []string) []string {
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := set[w]; !ok {
			out = append(out, w)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
