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

package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-steer/core-models/auth"
)

// The variables SAP's SDKs and LiteLLM's sap provider read, so a user
// who already has them set needs nothing else.
const (
	// sapBaseURL is the built-in's base_url: AICORE_BASE_URL, which the
	// service key can stand in for.
	sapBaseURL         = "${" + sapBaseURLEnv + "}"
	sapServiceKeyEnv   = "AICORE_SERVICE_KEY"
	sapAuthURLEnv      = "AICORE_AUTH_URL"
	sapClientIDEnv     = "AICORE_CLIENT_ID"
	sapClientSecretEnv = "AICORE_CLIENT_SECRET" //nolint:gosec // a variable name, not a credential
	sapBaseURLEnv      = "AICORE_BASE_URL"
)

// sapAICore is SAP AI Core's orchestration service (Generative AI Hub):
// one deployment serves every model SAP offers, named in the request
// body (anthropic--claude-4.5-sonnet, gpt-5-mini, gemini-2.5-pro).
//
// Credentials come in either shape SAP issues: the service key JSON in
// AICORE_SERVICE_KEY, or its four values in AICORE_AUTH_URL,
// AICORE_CLIENT_ID, AICORE_CLIENT_SECRET and AICORE_BASE_URL. Setting
// both is refused (sapEnv). AICORE_RESOURCE_GROUP picks the resource
// group, "default" when unset; AICORE_DEPLOYMENT_ID pins a deployment
// instead of finding the newest running one.
//
// Backend is its own: SAP bills in its capacity units, not per-token
// list prices, and core-models ships no sap-ai-core rows. Capabilities
// are the conservative floor, since they depend on the model family
// behind orchestration; an operator declares more per model.
var sapAICore = Profile{
	Name:     "sap-ai-core",
	Dialect:  OpenAIChat,
	Platform: PlatformSAPOrchestration,
	Backend:  "sap-ai-core",
	BaseURL:  sapBaseURL,
	Params:   map[string]string{"deployment_id": "${AICORE_DEPLOYMENT_ID:-}"},
	Auth: auth.Config{
		Kind:            auth.OAuth2ClientCredentials,
		TokenURL:        "${" + sapAuthURLEnv + "}",
		ClientIDEnv:     sapClientIDEnv,
		ClientSecretEnv: sapClientSecretEnv,
		// SAP's token endpoint (XSUAA) takes either; SAP's SDKs and
		// LiteLLM send the client in the form body.
		ClientAuth: "post",
	},
	Headers:    map[string]string{"AI-Resource-Group": "${AICORE_RESOURCE_GROUP:-default}"},
	OpenModels: yes(),
	Capabilities: Capabilities{
		ResponseSchema: no(), ReasoningEcho: no(), ParallelToolCalls: no(),
		ServerTools: no(), Streaming: yes(),
	},
	env: sapEnv,
}

// sapEnv serves the AICORE_* values sapAICore reads from whichever
// shape the user holds. A service key in AICORE_SERVICE_KEY supplies
// all four; otherwise the discrete variables are read as they stand.
// Either way the token URL gets SAP's /oauth/token path and the API
// URL its /v2 root when they lack them, as SAP's SDKs and LiteLLM
// accept both spellings.
//
// A run that sets the service key and any discrete variable is refused,
// naming them: LiteLLM silently takes the first source that has any
// value, and a stale discrete variable shadowing a fresh key (or the
// reverse) is the kind of mix-up that costs an afternoon.
func sapEnv(p Profile, getenv func(string) string) (func(string) string, error) {
	discrete := []string{sapAuthURLEnv, sapClientIDEnv, sapClientSecretEnv, sapBaseURLEnv}
	var set []string
	for _, name := range discrete {
		if getenv(name) != "" {
			set = append(set, name)
		}
	}
	raw := getenv(sapServiceKeyEnv)
	// With nothing set, name both shapes; Resolve alone would name only
	// AICORE_BASE_URL. Not when a derived profile has its own base_url,
	// which may need none of these.
	if raw == "" && len(set) == 0 && p.BaseURL == sapBaseURL {
		return nil, fmt.Errorf("set %s (SAP's service key JSON), or %s", sapServiceKeyEnv, strings.Join(discrete, ", "))
	}
	values := map[string]string{}
	if raw != "" {
		if len(set) > 0 {
			return nil, fmt.Errorf("%s and %s are both set: use the service key or the discrete variables, not both", sapServiceKeyEnv, strings.Join(set, ", "))
		}
		k, err := parseSAPServiceKey(raw)
		if err != nil {
			return nil, err
		}
		values[sapAuthURLEnv] = sapTokenURL(k.URL)
		values[sapClientIDEnv] = k.ClientID
		values[sapClientSecretEnv] = k.ClientSecret
		values[sapBaseURLEnv] = sapAPIURL(k.ServiceURLs.AIAPIURL)
	} else {
		if v := getenv(sapAuthURLEnv); v != "" {
			values[sapAuthURLEnv] = sapTokenURL(v)
		}
		if v := getenv(sapBaseURLEnv); v != "" {
			values[sapBaseURLEnv] = sapAPIURL(v)
		}
	}
	return func(name string) string {
		if v, ok := values[name]; ok {
			return v
		}
		return getenv(name)
	}, nil
}

// sapServiceKey is the part of SAP's service key the profile reads.
type sapServiceKey struct {
	ClientID     string `json:"clientid"`
	ClientSecret string `json:"clientsecret"`
	URL          string `json:"url"`
	CertURL      string `json:"certurl"`
	ServiceURLs  struct {
		AIAPIURL string `json:"AI_API_URL"`
	} `json:"serviceurls"`
}

// parseSAPServiceKey reads a service key as SAP issues it, or nested
// under "credentials" as a Cloud Foundry binding (VCAP_SERVICES) holds
// it. An error names what is missing and never quotes the key: it
// carries the client secret.
func parseSAPServiceKey(raw string) (sapServiceKey, error) {
	var outer struct {
		Credentials *sapServiceKey `json:"credentials"`
		sapServiceKey
	}
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		return sapServiceKey{}, fmt.Errorf("%s is not valid JSON", sapServiceKeyEnv)
	}
	k := outer.sapServiceKey
	if outer.Credentials != nil {
		k = *outer.Credentials
	}
	if k.ClientSecret == "" && k.CertURL != "" {
		return sapServiceKey{}, fmt.Errorf("%s is an X.509 certificate key, which this profile does not support; create a service key with a client secret", sapServiceKeyEnv)
	}
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"clientid", k.ClientID}, {"clientsecret", k.ClientSecret}, {"url", k.URL}, {"serviceurls.AI_API_URL", k.ServiceURLs.AIAPIURL},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return sapServiceKey{}, errors.New(sapServiceKeyEnv + " has no " + strings.Join(missing, ", "))
	}
	return k, nil
}

// sapTokenURL is u with SAP's token path, which a service key's url
// leaves off.
func sapTokenURL(u string) string {
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/oauth/token") {
		return u
	}
	return u + "/oauth/token"
}

// sapAPIURL is u with the AI API's /v2 root, which a service key's
// AI_API_URL leaves off.
func sapAPIURL(u string) string {
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/v2") {
		return u
	}
	return u + "/v2"
}
