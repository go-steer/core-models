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

// Originally derived from go-steer/core-agent@9d3eba89:pkg/models/gemini/{transient,bare_invalid_argument}_test.go

package gemini

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genai"
)

// recorded1247 is the turn error from #1247 verbatim, which is #898's
// too. If the predicate does not match this exact string it does not do
// the job it was written for.
const recorded1247 = "Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"

func TestIsBareInvalidArgument(t *testing.T) {
	typedBare := genai.APIError{Code: 400, Message: "Request contains an invalid argument.", Status: "INVALID_ARGUMENT"}
	withDetails := typedBare
	withDetails.Details = []map[string]any{{
		"@type":           "type.googleapis.com/google.rpc.BadRequest",
		"fieldViolations": []any{map[string]any{"field": "contents[3].parts[0]"}},
	}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The shape this exists for.
		{"#1247 verbatim", errors.New(recorded1247), true},
		{"#1247 wrapped", fmt.Errorf("generate content: %w", errors.New(recorded1247)), true},
		{"typed", typedBare, true},
		{"typed, wrapped", fmt.Errorf("gemini: %w", typedBare), true},
		{"typed, empty non-nil details", genai.APIError{Code: 400, Message: typedBare.Message, Status: "INVALID_ARGUMENT", Details: []map[string]any{}}, true},
		// What genai renders that typed error as is what the string
		// fallback matches: the two paths agree.
		{"typed error's own rendering, untyped", errors.New(typedBare.Error()), true},

		// A 400 that says what is wrong has said it; a retry is told
		// the same thing again.
		{"typed, with details", withDetails, false},
		{"untyped, with details", errors.New(withDetails.Error()), false},
		{"typed, names a parameter", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT",
			Message: "Unable to submit request because it has an empty text parameter. Add a value to the parameter and try again."}, false},
		{"untyped, names a parameter", errors.New("Error 400, Message: Unable to submit request because it has an empty text parameter., Status: INVALID_ARGUMENT, Details: []"), false},
		{"cache expired is vertexcache.Gone's", errors.New("Error 400, Message: Cache content 123 is expired., Status: INVALID_ARGUMENT, Details: []"), false},

		// The status word is required, not just the code.
		{"typed 400, FAILED_PRECONDITION", genai.APIError{Code: 400, Message: typedBare.Message, Status: "FAILED_PRECONDITION"}, false},
		{"typed 400, HTTP status line", genai.APIError{Code: 400, Message: typedBare.Message, Status: "400 Bad Request"}, false},
		{"untyped 400, FAILED_PRECONDITION", errors.New("Error 400, Message: Request contains an invalid argument., Status: FAILED_PRECONDITION, Details: []"), false},
		{"typed 500 INVALID_ARGUMENT", genai.APIError{Code: 500, Message: typedBare.Message, Status: "INVALID_ARGUMENT"}, false},

		// Not a provider rejection at all.
		{"model prose quoting the message", errors.New("the API said: Request contains an invalid argument."), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBareInvalidArgument(tc.err); got != tc.want {
				t.Errorf("IsBareInvalidArgument(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The predicate is never a transient one on its own: IsTransient must
// keep refusing the bare 400, or it would be retried on a session's
// very first call.
func TestIsTransient_StillRefusesTheBare400(t *testing.T) {
	if IsTransient(errors.New(recorded1247)) {
		t.Error("IsTransient accepted the bare 400 — it would be retried with no prior success")
	}
}

// archived429 is the error text recorded verbatim in all four GKE drill
// runs that lost a subagent delegation (#935). If the predicate does
// not match this exact string it does not do the job it was written
// for.
const archived429 = "Error 429, Message: Resource exhausted. Please try again later. " +
	"Please refer to https://cloud.google.com/vertex-ai/generative-ai/docs/error-code-429 " +
	"for more details., Status: RESOURCE_EXHAUSTED, Details: []"

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The case this exists for, in both the shape the SDK
		// produces and the shape that survives re-wrapping.
		{"archived 429, verbatim", errors.New(archived429), true},
		{"archived 429, wrapped by ADK", fmt.Errorf("generate content: %w", errors.New(archived429)), true},
		{"typed 429", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, true},
		{"typed 429, wrapped", fmt.Errorf("gemini: %w", genai.APIError{Code: 429}), true},
		{"typed 503", genai.APIError{Code: 503, Status: "UNAVAILABLE"}, true},
		{"untyped 503", errors.New("Error 503, Status: UNAVAILABLE, Details: []"), true},

		// Permanent. Retrying these spends a request to be told the
		// same thing.
		{"typed 400", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}, false},
		{"typed 404", genai.APIError{Code: 404, Status: "NOT_FOUND"}, false},
		{"typed 500", genai.APIError{Code: 500, Status: "INTERNAL"}, false},

		// #898's empty-Details 400 stays out of THIS predicate: on its
		// own it is indistinguishable from a malformed request. It is
		// retried only after the session has been served, through
		// IsBareInvalidArgument (#1247).
		{"the #898 400", errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"), false},

		// A status code on its own is not a discriminator. The drill
		// reads live cluster state, so provider-shaped numbers and
		// words turn up in model prose and in resource names.
		{"model prose quoting a 429", errors.New("the deployment reported 429 failed probes"), false},
		{"a pod named unavailable", errors.New("pods/svc-unavailable-7d9: CrashLoopBackOff"), false},
		{"UNAVAILABLE without a 503", errors.New("Status: UNAVAILABLE on a cached handle"), false},

		// vertexcache.Gone owns these; reading one as transient would retry
		// the identical cached request instead of dropping the handle.
		{"cache reaped", errors.New("Error 404, Message: Cached content x is not found., Status: NOT_FOUND"), false},
		{"cache expired", errors.New("Error 400, Message: Cache content x is expired., Status: INVALID_ARGUMENT"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A typed error that is not transient must not fall through to the
// substring path, where its message body could re-open the door.
func TestIsTransient_TypedNonTransientIsFinal(t *testing.T) {
	err := genai.APIError{
		Code:    400,
		Status:  "INVALID_ARGUMENT",
		Message: "your previous request returned 429 RESOURCE_EXHAUSTED",
	}
	if IsTransient(err) {
		t.Error("a typed 400 quoting a 429 in its message was read as transient")
	}
}

// TestIsBareInvalidArgumentBody is the same verdict on the wire, which
// is where the retry transport judges it.
func TestIsBareInvalidArgumentBody(t *testing.T) {
	bare := `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`
	for name, tc := range map[string]struct {
		status int
		body   string
		want   bool
	}{
		"bare":                {400, bare, true},
		"bare, in an array":   {400, "[" + bare + "]", true},
		"bare, empty details": {400, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT","details":[]}}`, true},
		"with details":        {400, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT","details":[{"@type":"x"}]}}`, false},
		"names a parameter":   {400, `{"error":{"code":400,"message":"Unable to submit request because it has an empty text parameter.","status":"INVALID_ARGUMENT"}}`, false},
		"cache expired":       {400, `{"error":{"code":400,"message":"Cache content 1 is expired.","status":"INVALID_ARGUMENT"}}`, false},
		"other status word":   {400, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"FAILED_PRECONDITION"}}`, false},
		"not a 400":           {500, bare, false},
		"not JSON":            {400, "Bad Request", false},
		"two errors":          {400, "[" + bare + "," + bare + "]", false},
	} {
		if got := IsBareInvalidArgumentBody(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
