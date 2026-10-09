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

package openaichat

import (
	"strings"

	"google.golang.org/genai"
)

// schemaToJSON renders genai's typed Schema as the JSON Schema object
// Chat Completions takes. genai spells types in upper case ("OBJECT")
// and nullability as a flag; JSON Schema wants "object" and a type
// list. Everything else maps field to field.
func schemaToJSON(s *genai.Schema) map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{}
	if s.Type != "" && s.Type != genai.TypeUnspecified {
		t := strings.ToLower(string(s.Type))
		if s.Nullable != nil && *s.Nullable {
			out["type"] = []string{t, "null"}
		} else {
			out["type"] = t
		}
	}
	set := func(k string, v any, ok bool) {
		if ok {
			out[k] = v
		}
	}
	set("title", s.Title, s.Title != "")
	set("description", s.Description, s.Description != "")
	set("format", s.Format, s.Format != "")
	set("pattern", s.Pattern, s.Pattern != "")
	set("enum", s.Enum, len(s.Enum) > 0)
	set("default", s.Default, s.Default != nil)
	set("example", s.Example, s.Example != nil)
	setPtr(out, "minimum", s.Minimum)
	setPtr(out, "maximum", s.Maximum)
	setPtr(out, "minLength", s.MinLength)
	setPtr(out, "maxLength", s.MaxLength)
	setPtr(out, "minItems", s.MinItems)
	setPtr(out, "maxItems", s.MaxItems)
	setPtr(out, "minProperties", s.MinProperties)
	setPtr(out, "maxProperties", s.MaxProperties)
	if s.Items != nil {
		out["items"] = schemaToJSON(s.Items)
	}
	if len(s.Properties) > 0 {
		props := make(map[string]any, len(s.Properties))
		for k, v := range s.Properties {
			props[k] = schemaToJSON(v)
		}
		out["properties"] = props
	} else if s.Type == genai.TypeObject {
		out["properties"] = map[string]any{}
	}
	set("required", s.Required, len(s.Required) > 0)
	if len(s.AnyOf) > 0 {
		any := make([]any, len(s.AnyOf))
		for i, v := range s.AnyOf {
			any[i] = schemaToJSON(v)
		}
		out["anyOf"] = any
	}
	return out
}

// setPtr sets out[k] to *v when v is present.
func setPtr[T any](out map[string]any, k string, v *T) {
	if v != nil {
		out[k] = *v
	}
}
