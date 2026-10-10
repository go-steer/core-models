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

package coremodels

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/gemini"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
)

// openGemini opens a gemini-dialect profile. The credential picks the
// backend: an API key is the Developer API, Google credentials are
// Vertex AI at params.project and params.region.
//
// What Open cannot express — Vertex context caching, which needs a
// cache manager built from the client — is for a caller to construct
// with gemini.New directly.
func openGemini(ctx context.Context, r *profile.Resolved, opts Options) (Provider, error) {
	rp := r.Profile
	bt, err := gemini.BuiltinToolsFromNames(opts.BuiltinTools)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", rp.Name, err)
	}
	if len(opts.BuiltinTools) > 0 && rp.Capabilities.ServerTools != nil && !*rp.Capabilities.ServerTools {
		return nil, fmt.Errorf("profile %q declares no server-side tools, but %v were asked for", rp.Name, opts.BuiltinTools)
	}
	gopts := gemini.Options{
		// Empty is Google's endpoint for the backend; a profile sets it
		// for a proxy or a private endpoint.
		BaseURL:      r.BaseURL,
		HTTPClient:   opts.HTTPClient,
		Retry:        opts.Retry,
		BackendName:  rp.BackendName(),
		BuiltinTools: bt,
		Logf:         opts.Logf,
	}
	switch r.Credential.Kind() {
	case auth.APIKey:
		gopts.Backend, gopts.APIKey = genai.BackendGeminiAPI, r.Credential.Secret()
	case auth.GoogleADC:
		gopts.Backend, gopts.Credential = genai.BackendVertexAI, r.Credential
		gopts.Project, gopts.Location = r.Params["project"], r.Params["region"]
	default:
		return nil, fmt.Errorf("profile %q: dialect gemini authenticates with api_key (the Developer API) or google_adc (Vertex AI), not %s", rp.Name, r.Credential.Kind())
	}
	c, err := gemini.New(ctx, gopts)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", rp.Name, err)
	}
	return &provider{p: rp, model: func(id string) llm.LLM { return c.Model(id) }}, nil
}
