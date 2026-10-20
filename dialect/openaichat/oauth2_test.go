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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/dialect/openaichat"
	"github.com/go-steer/core-models/llm"
)

// An OAuth2 client-credentials token reaches the server as a bearer, and
// a 401 on a token fetched moments ago is returned rather than answered
// with a new token per request (auth's 401 tests cover the re-fetch).
func TestAnOAuth2TokenIsSentAsABearer(t *testing.T) {
	var issued atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"bearer","expires_in":3600}`, issued.Add(1))
	}))
	t.Cleanup(idp.Close)
	cred, err := auth.Config{
		Kind: auth.OAuth2ClientCredentials, TokenURL: idp.URL + "/token",
		ClientIDEnv: "GW_ID", ClientSecretEnv: "GW_SECRET",
	}.Resolve(context.Background(), auth.Options{Getenv: func(k string) string { return "v-" + k }})
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t,
		reply{body: okReply},
		reply{status: http.StatusUnauthorized, body: `{"error":{"message":"not entitled","type":"invalid_request_error"}}`},
	)
	m := client(t, s, func(o *openaichat.Options) { o.Credential = cred }).Model("m")
	req := &llm.Request{Contents: []*genai.Content{userText("hi")}}
	complete(t, m, req)
	if got := s.headers[0].Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q", got)
	}
	if err := generateErr(m, req, false); err == nil {
		t.Fatal("a 401 succeeded")
	}
	if len(s.bodies) != 2 || issued.Load() != 1 {
		t.Fatalf("requests = %d, tokens = %d: want 2 and 1", len(s.bodies), issued.Load())
	}
}
