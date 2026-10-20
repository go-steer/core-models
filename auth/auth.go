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

// Package auth resolves the credential a provider profile declares.
//
// Resolution happens when the profile is opened, not on the first
// request: a profile whose key is missing or whose Google credentials
// cannot be found fails at construction, naming what is missing
// (requirement R1). A secret never appears in an error, a log line or
// a String().
//
// OAuth2ClientCredentials is the one kind whose material changes while
// a provider is open: it exchanges a client id and secret for a
// short-lived access token, caches it, and fetches a new one before it
// expires or after the server answers 401.
//
// How the credential is put on the wire is the dialect's business —
// OpenAI-shaped servers take a bearer token, Anthropic an x-api-key
// header, Gemini its own — so a Credential exposes the material and
// one helper for the common case, BearerTransport.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	cloudauth "cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

// Kind names how a profile authenticates.
type Kind string

const (
	// APIKey is a static key read from an environment variable.
	APIKey Kind = "api_key"
	// Bearer is a static bearer token read from an environment variable.
	// It differs from APIKey only in intent; both are sent the same way
	// by OpenAI-shaped dialects.
	Bearer Kind = "bearer"
	// GoogleADC is Google Application Default Credentials: the credential
	// Gemini-on-Vertex and Claude-on-Vertex already use, and the one
	// Vertex AI's partner-model endpoint takes.
	GoogleADC Kind = "google_adc"
	// None sends no credential: a self-hosted server on a trusted
	// network.
	None Kind = "none"
	// OAuth2ClientCredentials exchanges a client id and secret for an
	// access token at TokenURL (RFC 6749 §4.4), the credential a model
	// behind an authenticating gateway usually takes: Apigee, Azure API
	// Management with Entra ID, Kong or Okta OIDC, SAP AI Core. The token
	// is cached and refreshed before it expires, so a session may outlive
	// any one token.
	OAuth2ClientCredentials Kind = "oauth2_client_credentials" //nolint:gosec // a kind name, not a credential
)

// kindList is every Kind, for error messages.
const kindList = "api_key, bearer, google_adc, oauth2_client_credentials or none"

// CloudPlatformScope is the OAuth scope GoogleADC requests by default.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Config is the auth block of a profile.
type Config struct {
	Kind Kind `json:"kind" yaml:"kind"`
	// Env names the environment variable holding the key or token, for
	// APIKey and Bearer.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// AltEnv lists further variables tried in order when Env is unset,
	// for a vendor whose key goes by two names (Gemini's GOOGLE_API_KEY
	// and GEMINI_API_KEY).
	AltEnv []string `json:"alt_env,omitempty" yaml:"alt_env,omitempty"`
	// Scopes overrides the OAuth scopes GoogleADC requests, and lists
	// the scopes OAuth2ClientCredentials asks for (none by default).
	Scopes []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`

	// TokenURL is OAuth2ClientCredentials' token endpoint. In a profile
	// it may use ${VAR} and ${VAR:-default}, read at Resolve as params
	// are; by the time Config.Resolve sees it, it is a URL.
	TokenURL string `json:"token_url,omitempty" yaml:"token_url,omitempty"`
	// ClientIDEnv and ClientSecretEnv name the environment variables
	// holding OAuth2ClientCredentials' client id and secret. The values
	// are never written to a profile.
	ClientIDEnv     string `json:"client_id_env,omitempty" yaml:"client_id_env,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty" yaml:"client_secret_env,omitempty"`
	// Audience is sent as the token request's audience parameter, for an
	// identity provider that requires one (Auth0, some Okta setups).
	Audience string `json:"audience,omitempty" yaml:"audience,omitempty"`
	// ClientAuth is how the client authenticates to the token endpoint:
	// "basic" (HTTP Basic, the default RFC 6749 recommends) or "post"
	// (client_id and client_secret in the form body).
	ClientAuth string `json:"client_auth,omitempty" yaml:"client_auth,omitempty"`
}

// oauth2Set reports whether any OAuth2ClientCredentials-only field is set.
func (c Config) oauth2Set() bool {
	return c.TokenURL != "" || c.ClientIDEnv != "" || c.ClientSecretEnv != "" || c.Audience != "" || c.ClientAuth != ""
}

// Validate checks the block's shape without touching the environment.
func (c Config) Validate() error {
	if c.Kind != OAuth2ClientCredentials && c.Kind != "" && c.oauth2Set() {
		return fmt.Errorf("auth kind %q takes no token_url, client_id_env, client_secret_env, audience or client_auth: those are for oauth2_client_credentials", c.Kind)
	}
	switch c.Kind {
	case APIKey, Bearer:
		if c.Env == "" {
			return fmt.Errorf("auth kind %q needs env: the variable that holds the secret", c.Kind)
		}
		if len(c.Scopes) > 0 {
			return fmt.Errorf("auth kind %q takes no scopes", c.Kind)
		}
	case GoogleADC:
		if c.Env != "" || len(c.AltEnv) > 0 {
			return errors.New(`auth kind "google_adc" takes no env: credentials come from ADC`)
		}
	case None:
		if c.Env != "" || len(c.AltEnv) > 0 || len(c.Scopes) > 0 {
			return errors.New(`auth kind "none" takes no env or scopes`)
		}
	case OAuth2ClientCredentials:
		if c.Env != "" || len(c.AltEnv) > 0 {
			return errors.New(`auth kind "oauth2_client_credentials" takes no env or alt_env: name the variables with client_id_env and client_secret_env`)
		}
		var missing []string
		for _, f := range []struct{ name, v string }{
			{"token_url", c.TokenURL}, {"client_id_env", c.ClientIDEnv}, {"client_secret_env", c.ClientSecretEnv},
		} {
			if f.v == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf(`auth kind "oauth2_client_credentials" needs %s`, strings.Join(missing, ", "))
		}
		if c.ClientAuth != "" && c.ClientAuth != clientAuthBasic && c.ClientAuth != clientAuthPost {
			return fmt.Errorf(`auth client_auth %q is not "basic" or "post"`, c.ClientAuth)
		}
	case "":
		return errors.New("auth kind is required (" + kindList + ")")
	default:
		return fmt.Errorf("unknown auth kind %q (want %s)", c.Kind, kindList)
	}
	return nil
}

// TokenSource yields a current access token. *auth.Credentials from
// cloud.google.com/go/auth satisfies it through tokenSource below.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// QuotaProjecter is implemented by a TokenSource whose credentials name
// a quota project: the project Google bills and quota-checks requests
// against when it differs from the credentials' own (user ADC with
// `gcloud auth application-default set-quota-project`, or
// GOOGLE_CLOUD_QUOTA_PROJECT). Optional.
type QuotaProjecter interface {
	QuotaProject(ctx context.Context) (string, error)
}

// ErrNoADC is the error Resolve wraps when GoogleADC finds no
// Application Default Credentials, so a host can offer its own remedy
// (`gcloud auth application-default login`) with errors.Is rather than
// by matching text.
var ErrNoADC = errors.New("auth: no Google Application Default Credentials")

// Options adjusts resolution. The zero value reads the process
// environment and real Google credentials.
type Options struct {
	// Getenv replaces os.Getenv.
	Getenv func(string) string
	// DetectGoogle replaces ADC detection.
	DetectGoogle func(ctx context.Context, scopes []string) (TokenSource, error)
	// HTTPClient sends OAuth2ClientCredentials' token requests. Nil means
	// a client on http.DefaultTransport with a 30-second timeout.
	HTTPClient *http.Client
}

// Credential is a resolved Config.
type Credential struct {
	kind   Kind
	env    string
	secret string
	tokens TokenSource
	// quotaProject is sent as X-Goog-User-Project with every
	// GoogleADC request, as Google's own transport does.
	quotaProject string
	// describe is String()'s text for a kind that needs more than its
	// name; it never holds a secret.
	describe string
}

// invalidator is implemented by a TokenSource that can drop a token
// the server has refused, so the next Token fetches a new one.
type invalidator interface {
	invalidate(token string)
}

// Resolve reads the secret or finds the Google credentials c declares.
func (c Config) Resolve(ctx context.Context, opts Options) (*Credential, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cred := &Credential{kind: c.Kind, env: c.Env}
	switch c.Kind {
	case APIKey, Bearer:
		for _, name := range append([]string{c.Env}, c.AltEnv...) {
			if v := getenv(name); v != "" {
				cred.secret, cred.env = v, name
				break
			}
		}
		if cred.secret == "" {
			return nil, fmt.Errorf("auth: %s is not set", strings.Join(append([]string{c.Env}, c.AltEnv...), " or "))
		}
	case GoogleADC:
		scopes := c.Scopes
		if len(scopes) == 0 {
			scopes = []string{CloudPlatformScope}
		}
		detect := opts.DetectGoogle
		if detect == nil {
			detect = detectGoogle
		}
		ts, err := detect(ctx, scopes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoADC, err)
		}
		cred.tokens = ts
		// Read once, as cloud.google.com/go/auth's httptransport does
		// when it builds its transport.
		if q, ok := ts.(QuotaProjecter); ok {
			qp, err := q.QuotaProject(ctx)
			if err != nil {
				return nil, fmt.Errorf("auth: quota project: %w", err)
			}
			cred.quotaProject = qp
		}
	case OAuth2ClientCredentials:
		ts, err := newClientCredentials(c, getenv, opts.HTTPClient)
		if err != nil {
			return nil, err
		}
		cred.describe = ts.String()
		// The first token is fetched here, so a wrong secret or an
		// unreachable token endpoint fails at construction (R1).
		if _, err := ts.Token(ctx); err != nil {
			return nil, err
		}
		cred.tokens = ts
	}
	return cred, nil
}

func detectGoogle(_ context.Context, scopes []string) (TokenSource, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: scopes})
	if err != nil {
		return nil, err
	}
	return tokenSource{creds}, nil
}

type tokenSource struct{ creds *cloudauth.Credentials }

// QuotaProject implements QuotaProjecter. The credentials resolve
// GOOGLE_CLOUD_QUOTA_PROJECT ahead of the file's quota_project_id.
func (t tokenSource) QuotaProject(ctx context.Context) (string, error) {
	return t.creds.QuotaProjectID(ctx)
}

func (t tokenSource) Token(ctx context.Context) (string, error) {
	tok, err := t.creds.Token(ctx)
	if err != nil {
		return "", err
	}
	return tok.Value, nil
}

// Kind reports how the credential authenticates.
func (c *Credential) Kind() Kind { return c.kind }

// Secret returns the static key or token for APIKey and Bearer, and ""
// otherwise. For a dialect whose SDK takes the key directly.
func (c *Credential) Secret() string { return c.secret }

// Token returns a current token: the static secret for APIKey and
// Bearer, a fresh access token for GoogleADC and
// OAuth2ClientCredentials, "" for None.
func (c *Credential) Token(ctx context.Context) (string, error) {
	if c.tokens != nil {
		return c.tokens.Token(ctx)
	}
	return c.secret, nil
}

// String describes the credential without its secret.
func (c *Credential) String() string {
	switch c.kind {
	case APIKey, Bearer:
		return fmt.Sprintf("%s from $%s", c.kind, c.env)
	case OAuth2ClientCredentials:
		return c.describe
	default:
		return string(c.kind)
	}
}

// quotaProjectHeader names the quota project a Google request bills.
const quotaProjectHeader = "X-Goog-User-Project"

// BearerTransport returns a RoundTripper that sets
// "Authorization: Bearer <token>" on every request (and, for GoogleADC
// credentials that name one, the X-Goog-User-Project quota project), fetching the token
// per request so an access token is refreshed as it expires. For
// None it returns base unchanged. A nil base means
// http.DefaultTransport.
//
// For OAuth2ClientCredentials, a 401 answer drops the token that was
// refused and the request is sent once more with a new one: a token
// revoked or expired early is replaced without the caller seeing it.
// Only once, so a credential the server keeps refusing surfaces as the
// second 401. The retry layer sits outside this transport and does not
// retry a 401, so the request is never sent more than twice for it.
func (c *Credential) BearerTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if c.kind == None {
		return base
	}
	return bearer{cred: c, base: base}
}

type bearer struct {
	cred *Credential
	base http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := b.cred.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("auth: %s: %w", b.cred, err)
	}
	resp, err := b.base.RoundTrip(b.withToken(req, tok))
	inv, ok := b.cred.tokens.(invalidator)
	if err != nil || !ok || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return resp, err
	}
	inv.invalidate(tok)
	fresh, err := b.cred.Token(req.Context())
	if err != nil || fresh == tok {
		// No better token to try: the 401 stands.
		return resp, nil
	}
	retry := req
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil
		}
		retry = req.Clone(req.Context())
		retry.Body = body
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return b.base.RoundTrip(b.withToken(retry, fresh))
}

// withToken returns a copy of req carrying tok. A RoundTripper must not
// modify the request it was given.
func (b bearer) withToken(req *http.Request, tok string) *http.Request {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	// Never over a quota project the caller set.
	if qp := b.cred.quotaProject; qp != "" && req.Header.Get(quotaProjectHeader) == "" {
		req.Header.Set(quotaProjectHeader, qp)
	}
	return req
}
