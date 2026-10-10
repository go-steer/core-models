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

package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// VertexVersion is the anthropic_version Vertex AI requires in the
// body of every Claude request, in place of the first-party
// anthropic-version header.
const VertexVersion = "vertex-2023-10-16"

// vertexMiddleware rewrites a first-party Messages request into the
// form Vertex AI serves Claude under:
//
//	POST {base}/v1/messages {"model": m, "stream": true, …}
//	→ POST {base}/{m}:streamRawPredict {"anthropic_version": …, "stream": true, …}
//
// base is the publisher prefix the profile resolved
// (https://{host}/v1/projects/{p}/locations/{r}/publishers/anthropic/models).
// rawPredict replaces streamRawPredict for a non-streaming body. Any
// other request passes through untouched: the adapter sends nothing
// else, and a request it does not recognize is better refused by the
// server than guessed at here.
//
// It does what the SDK's vertex.WithCredentials middleware does, minus
// the credential, which arrives on the transport (package auth).
func vertexMiddleware(base string) option.Middleware {
	prefix, err := url.Parse(base)
	return func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if err != nil {
			return nil, fmt.Errorf("anthropic: vertex base URL %q: %w", base, err)
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") || r.Body == nil {
			return next(r)
		}
		raw, rerr := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("anthropic: vertex: request body is not a JSON object: %w", err)
		}
		var modelID string
		if err := json.Unmarshal(body["model"], &modelID); err != nil || modelID == "" {
			return nil, fmt.Errorf("anthropic: vertex: request names no model")
		}
		var stream bool
		_ = json.Unmarshal(body["stream"], &stream)
		delete(body, "model")
		if _, ok := body["anthropic_version"]; !ok {
			body["anthropic_version"] = json.RawMessage(`"` + VertexVersion + `"`)
		}
		out, merr := json.Marshal(body)
		if merr != nil {
			return nil, merr
		}
		method := "rawPredict"
		if stream {
			method = "streamRawPredict"
		}
		u := *prefix
		u.Path = strings.TrimRight(prefix.Path, "/") + "/" + modelID + ":" + method
		u.RawPath = ""
		r.URL = &u
		r.Host = u.Host
		r.Body = io.NopCloser(bytes.NewReader(out))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }
		r.ContentLength = int64(len(out))
		return next(r)
	}
}
