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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-models/profile"
)

// fakeXSUAA is SAP's token endpoint: it records the last form it got.
type fakeXSUAA struct {
	*httptest.Server
	mu   sync.Mutex
	form url.Values
	path string
}

func newFakeXSUAA(t *testing.T) *fakeXSUAA {
	f := &fakeXSUAA{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		f.mu.Lock()
		f.form, f.path = form, r.URL.Path
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"sap-token","token_type":"bearer","expires_in":43199}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func serviceKey(authURL string, nested bool) string {
	k := fmt.Sprintf(`{"clientid":"sb-client","clientsecret":"s3cr3t-value","url":%q,"serviceurls":{"AI_API_URL":"https://api.ai.example.com"}}`, authURL)
	if nested {
		return `{"credentials":` + k + `}`
	}
	return k
}

func sapProfile(t *testing.T) profile.Profile {
	t.Helper()
	p, ok := profile.Builtin("sap-ai-core")
	if !ok {
		t.Fatal("no sap-ai-core built-in")
	}
	return p
}

// The service key supplies the token endpoint (with SAP's /oauth/token
// path added), the client and the AI API root (with /v2 added), in the
// shape SAP issues it and nested under "credentials" as a Cloud Foundry
// binding holds it.
func TestSAPServiceKey(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%v", nested), func(t *testing.T) {
			idp := newFakeXSUAA(t)
			r, err := resolve(t, sapProfile(t), map[string]string{"AICORE_SERVICE_KEY": serviceKey(idp.URL+"/", nested)})
			if err != nil {
				t.Fatal(err)
			}
			if r.BaseURL != "https://api.ai.example.com/v2" {
				t.Errorf("BaseURL = %q", r.BaseURL)
			}
			idp.mu.Lock()
			defer idp.mu.Unlock()
			if idp.path != "/oauth/token" {
				t.Errorf("token request went to %q", idp.path)
			}
			if idp.form.Get("client_id") != "sb-client" || idp.form.Get("client_secret") != "s3cr3t-value" || idp.form.Get("grant_type") != "client_credentials" {
				t.Errorf("token form = %v", idp.form)
			}
			if got := r.HeaderNames(); len(got) != 1 || got[0] != "Ai-Resource-Group" {
				t.Errorf("headers = %v", got)
			}
			if r.Params["deployment_id"] != "" {
				t.Errorf("deployment_id = %q, want unset", r.Params["deployment_id"])
			}
			if r.Profile.BackendName() != "sap-ai-core" {
				t.Errorf("backend = %q", r.Profile.BackendName())
			}
			// Nothing printable carries the secret.
			for _, s := range []string{fmt.Sprintf("%+v", r), fmt.Sprint(r.Credential)} {
				if strings.Contains(s, "s3cr3t-value") {
					t.Errorf("secret printed: %s", s)
				}
			}
		})
	}
}

// The four discrete variables work as LiteLLM's sap provider reads
// them, with or without the paths SAP's URLs leave off.
func TestSAPDiscreteVariables(t *testing.T) {
	idp := newFakeXSUAA(t)
	r, err := resolve(t, sapProfile(t), map[string]string{
		"AICORE_AUTH_URL":       idp.URL + "/oauth/token",
		"AICORE_CLIENT_ID":      "sb-client",
		"AICORE_CLIENT_SECRET":  "s3cr3t-value",
		"AICORE_BASE_URL":       "https://api.ai.example.com/v2/",
		"AICORE_DEPLOYMENT_ID":  "d0123",
		"AICORE_RESOURCE_GROUP": "team-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseURL != "https://api.ai.example.com/v2" || r.Params["deployment_id"] != "d0123" {
		t.Errorf("BaseURL = %q, deployment_id = %q", r.BaseURL, r.Params["deployment_id"])
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.path != "/oauth/token" {
		t.Errorf("token path = %q, want it not doubled", idp.path)
	}
}

// Setting both shapes is refused, naming the variables, rather than one
// silently winning.
func TestSAPBothShapesRefused(t *testing.T) {
	_, err := resolve(t, sapProfile(t), map[string]string{
		"AICORE_SERVICE_KEY": serviceKey("https://idp.example.com", false),
		"AICORE_CLIENT_ID":   "stale",
	})
	if err == nil || !strings.Contains(err.Error(), "AICORE_SERVICE_KEY and AICORE_CLIENT_ID are both set") {
		t.Fatalf("Resolve = %v", err)
	}
}

// A bad service key says what is wrong without quoting it.
func TestSAPServiceKeyErrors(t *testing.T) {
	for _, tc := range []struct{ name, key, want string }{
		{"not json", `{"clientsecret":"s3cr3t-value"`, "AICORE_SERVICE_KEY is not valid JSON"},
		{"missing fields", `{"clientsecret":"s3cr3t-value","url":"https://idp.example.com"}`, "AICORE_SERVICE_KEY has no clientid, serviceurls.AI_API_URL"},
		{"x509", `{"clientid":"c","certurl":"https://idp.example.com","certificate":"PEM","serviceurls":{"AI_API_URL":"https://api"}}`, "X.509 certificate key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolve(t, sapProfile(t), map[string]string{"AICORE_SERVICE_KEY": tc.key})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cr3t-value") || strings.Contains(err.Error(), "PEM") {
				t.Fatalf("error quotes the key: %v", err)
			}
		})
	}
}

// With neither shape set, the error names a variable to set.
func TestSAPNothingSet(t *testing.T) {
	_, err := resolve(t, sapProfile(t), map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "AICORE_BASE_URL is not set") {
		t.Fatalf("Resolve = %v", err)
	}
}

// The platform belongs to openai-chat alone.
func TestSAPPlatformDialect(t *testing.T) {
	p := profile.Profile{Name: "x", Extends: "sap-ai-core", Dialect: profile.OpenAIResponses}
	p, err := profile.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `platform "sap-orchestration" applies to the openai-chat dialect`) {
		t.Fatalf("Validate = %v", err)
	}
}
