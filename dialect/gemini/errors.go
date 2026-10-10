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

// Originally derived from go-steer/core-agent@9d3eba89:pkg/models/gemini/transient.go (#935, #1247)

package gemini

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// bareInvalidArgumentMessage is the whole message of the 400 that
// core-agent #898 and #1247 recorded: Vertex naming nothing it could
// not parse.
const bareInvalidArgumentMessage = "Request contains an invalid argument."

// IsBareInvalidArgumentBody reports whether an HTTP response is
// Vertex's 400 INVALID_ARGUMENT that names nothing — the generic
// message and no details. It is the retry.Policy.AfterSuccess New
// installs: twice now it arrived on a session whose previous call under
// the same config had succeeded, and the session worked afterwards. On
// its own it is indistinguishable from a request malformed from the
// start, which is why the transport consults it only for a session
// that has been served (callctx.PriorCallSucceeded), and only once.
//
// All three must hold, each a narrowing: status 400 with
// INVALID_ARGUMENT; no details, since a 400 with field violations has
// said what is wrong; the generic message exactly, since one naming a
// parameter is saying what is wrong too, and an expired cache ("Cache
// content <id> is expired.") is vertexcache.Gone's. Google sends the
// error object bare or, on some Vertex paths, inside a one-element
// array; both are read.
func IsBareInvalidArgumentBody(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	type apiError struct {
		Error *struct {
			Code    int               `json:"code"`
			Message string            `json:"message"`
			Status  string            `json:"status"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	var one apiError
	if err := json.Unmarshal(body, &one); err != nil || one.Error == nil {
		var many []apiError
		if err := json.Unmarshal(body, &many); err != nil || len(many) != 1 || many[0].Error == nil {
			return false
		}
		one = many[0]
	}
	e := one.Error
	return e.Code == http.StatusBadRequest && e.Status == "INVALID_ARGUMENT" &&
		len(e.Details) == 0 && strings.TrimSpace(e.Message) == bareInvalidArgumentMessage
}

// IsTransient reports whether err is Gemini declining to serve this
// request right now — 429 RESOURCE_EXHAUSTED or 503 UNAVAILABLE — as
// opposed to declining it at all. The adapter does not need it: package
// retry already retries those at the HTTP layer, with Retry-After
// visible. It is exported for a product's own model-level policy, which
// sees what the transport gave up on (core-agent's RetryPolicy).
//
// The typed genai.APIError check is exact and tried first; a typed
// error that is neither code is a definite no. The text fallback pairs
// the code with its status word, so a 429 in the model's own prose or
// a pod named "unavailable" cannot match.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code == http.StatusServiceUnavailable
	}
	s := err.Error()
	return (strings.Contains(s, "429") && strings.Contains(s, "RESOURCE_EXHAUSTED")) ||
		(strings.Contains(s, "503") && strings.Contains(s, "UNAVAILABLE"))
}

// IsBareInvalidArgument is IsBareInvalidArgumentBody for an error that
// has already become a genai.APIError, for a product policy above the
// adapter that judges errors rather than responses.
func IsBareInvalidArgument(err error) bool {
	if err == nil {
		return false
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusBadRequest && apiErr.Status == "INVALID_ARGUMENT" &&
			len(apiErr.Details) == 0 && strings.TrimSpace(apiErr.Message) == bareInvalidArgumentMessage
	}
	return strings.Contains(err.Error(),
		"Error 400, Message: "+bareInvalidArgumentMessage+", Status: INVALID_ARGUMENT, Details: []")
}
