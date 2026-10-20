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

package profile_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/core-models/profile"
)

// A profile behind an OAuth2 gateway: token_url expands ${VAR} as params
// do, and the first token is fetched at Resolve.
func TestOAuth2ClientCredentialsProfile(t *testing.T) {
	var form string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"gw-token","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(idp.Close)

	ps, err := profile.DecodeJSON([]byte(`[{
		"name": "gateway",
		"extends": "openai-compatible",
		"base_url": "https://gw.example.com/v1",
		"auth": {
			"kind": "oauth2_client_credentials",
			"token_url": "${GW_TOKEN_URL}",
			"client_id_env": "GW_CLIENT_ID",
			"client_secret_env": "GW_CLIENT_SECRET",
			"scopes": ["models.invoke"]
		}
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	p := ps[0]
	env := map[string]string{"GW_CLIENT_ID": "id", "GW_CLIENT_SECRET": "secret"}
	if _, err := resolve(t, p, env); err == nil || !strings.Contains(err.Error(), "auth.token_url: GW_TOKEN_URL is not set") {
		t.Fatalf("Resolve without GW_TOKEN_URL = %v", err)
	}
	env["GW_TOKEN_URL"] = idp.URL + "/oauth/token"
	r, err := resolve(t, p, env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(form, "scope=models.invoke") {
		t.Fatalf("token request form = %q", form)
	}
	if tok, err := r.Credential.Token(t.Context()); err != nil || tok != "gw-token" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
}

func TestOAuth2TokenURLMustBeAURL(t *testing.T) {
	p := profile.Profile{Name: "gateway", Extends: "openai-compatible", BaseURL: "https://gw.example.com/v1"}
	p.Auth.Kind = "oauth2_client_credentials"
	p.Auth.TokenURL = "login.example.com/token"
	p.Auth.ClientIDEnv, p.Auth.ClientSecretEnv = "I", "S"
	p, err := profile.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "auth.token_url") {
		t.Fatalf("Validate = %v, want auth.token_url refused", err)
	}
}
