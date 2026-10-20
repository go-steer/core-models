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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/dialect/openaichat"
	"github.com/go-steer/core-models/llm"
	"google.golang.org/genai"
)

// oauthCredential resolves an OAuth2 client-credentials credential
// against a fake token endpoint that issues tok-1, tok-2, ….
func oauthCredential(t *testing.T) (*auth.Credential, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"bearer","expires_in":3600}`, n.Add(1))
	}))
	t.Cleanup(idp.Close)
	cred, err := auth.Config{
		Kind: auth.OAuth2ClientCredentials, TokenURL: idp.URL + "/token",
		ClientIDEnv: "GW_ID", ClientSecretEnv: "GW_SECRET",
	}.Resolve(context.Background(), auth.Options{Getenv: func(k string) string { return "v-" + k }})
	if err != nil {
		t.Fatal(err)
	}
	return cred, &n
}

func TestA401FetchesANewTokenAndRetriesOnce(t *testing.T) {
	cred, issued := oauthCredential(t)
	// The gateway has revoked tok-1 early: it refuses it and accepts tok-2.
	s := newServer(t,
		reply{status: http.StatusUnauthorized, body: `{"error":{"message":"token revoked","type":"invalid_request_error"}}`},
		reply{body: okReply},
	)
	m := client(t, s, func(o *openaichat.Options) { o.Credential = cred }).Model("m")
	r := complete(t, m, &llm.Request{Contents: []*genai.Content{userText("hi")}})
	if got := r.Content.Parts[0].Text; got != "ok" {
		t.Fatalf("answer = %q", got)
	}
	if got := []string{s.headers[0].Get("Authorization"), s.headers[1].Get("Authorization")}; got[0] != "Bearer tok-1" || got[1] != "Bearer tok-2" {
		t.Fatalf("Authorization = %q, want tok-1 then tok-2", got)
	}
	if s.body(1)["model"] != "m" {
		t.Fatalf("the retried request lost its body: %v", s.body(1))
	}
	if n := issued.Load(); n != 2 {
		t.Fatalf("tokens issued = %d, want 2", n)
	}
}

func TestA401ThatPersistsIsReturnedAfterOneRetry(t *testing.T) {
	cred, issued := oauthCredential(t)
	refuse := reply{status: http.StatusUnauthorized, body: `{"error":{"message":"not allowed","type":"invalid_request_error"}}`}
	s := newServer(t, refuse, refuse, refuse, refuse)
	m := client(t, s, func(o *openaichat.Options) { o.Credential = cred }).Model("m")
	err := generateErr(m, &llm.Request{Contents: []*genai.Content{userText("hi")}}, false)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401", err)
	}
	if len(s.bodies) != 2 || issued.Load() != 2 {
		t.Fatalf("requests = %d, tokens = %d: want exactly one re-fetch and one retry", len(s.bodies), issued.Load())
	}
}
