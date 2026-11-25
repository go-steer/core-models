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

package llm

import "google.golang.org/genai"

// ThinkingLevelMax asks for the most thinking a model offers, one step
// above genai.ThinkingLevelXhigh, which genai has no name for. The
// Anthropic adapter sends it as effort "max"; the Gemini adapter as the
// model's highest thinking level.
//
// genai.ThinkingConfig.ThinkingLevel is core-models' provider-neutral
// thinking control: each adapter maps the level onto what the model
// accepts (docs/design.md, "Thinking level").
const ThinkingLevelMax genai.ThinkingLevel = "MAX"
