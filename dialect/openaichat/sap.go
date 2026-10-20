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

package openaichat

// SAP AI Core's orchestration service (Generative AI Hub) speaks Chat
// Completions inside an envelope. The wire format here follows SAP's
// Orchestration v2 OpenAPI spec and SAP's Cloud SDK for AI
// (github.com/SAP/ai-sdk-js, packages/orchestration), the source of
// truth; it has not yet been recorded against a real tenant.
//
//	POST {deploymentUrl}/v2/completion
//	{"config": {"modules": {"prompt_templating": {
//	     "prompt": {"template": [...], "tools": [...], "response_format": {...}},
//	     "model":  {"name": "...", "params": {...}}}},
//	   "stream": {"enabled": true}},
//	 "messages_history": [...]}
//
// The answer's final_result is a Chat Completion (or, streamed, one
// chunk per event); errors carry {"error": {code, message, location}}.
// One orchestration deployment serves every model, found at startup
// from the AI API's deployment list.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PlatformSAPOrchestration is Options.Platform for SAP AI Core's
// orchestration service.
const PlatformSAPOrchestration = "sap-orchestration"

// sapModulesKey is the ExtraBody key whose object is merged into
// config.modules: orchestration's own modules (filtering, masking,
// grounding, translation), which are the product's to configure.
// Every other ExtraBody key goes to the model's params.
const sapModulesKey = "modules"

// sapScenario is the scenario every orchestration deployment runs.
const sapScenario = "orchestration"

// sapDeployment finds and remembers the orchestration deployment's
// completion URL.
type sapDeployment struct {
	api    *url.URL // the AI API root, ".../v2"
	pinned string   // a deployment id to use rather than the newest

	mu  sync.Mutex
	url string // POST target; "" until found
}

func newSAPDeployment(base, pinned string) (*sapDeployment, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("openai-chat: sap-orchestration: BaseURL is not a URL with a host")
	}
	return &sapDeployment{api: u, pinned: pinned}, nil
}

// endpoint returns the completion URL, finding the deployment on first
// use. stale, when not "", is a URL the server answered 404 at: it is
// forgotten and the deployment found again, unless another request has
// already done so.
func (d *sapDeployment) endpoint(ctx context.Context, hc *http.Client, stale string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.url != "" && (stale == "" || d.url != stale) {
		return d.url, nil
	}
	d.url = ""
	u, err := d.find(ctx, hc)
	if err != nil {
		return "", err
	}
	d.url = u
	return u, nil
}

// sapDeploymentRecord is the part of an AI API deployment the dialect
// reads.
type sapDeploymentRecord struct {
	ID            string `json:"id"`
	DeploymentURL string `json:"deploymentUrl"`
	ScenarioID    string `json:"scenarioId"`
	Status        string `json:"status"`
	CreatedAt     string `json:"createdAt"`
}

// find asks the AI API for the deployment: the pinned one, which must
// be running, or the newest running orchestration deployment in the
// resource group (the AI-Resource-Group header the profile sends).
func (d *sapDeployment) find(ctx context.Context, hc *http.Client) (string, error) {
	if d.pinned != "" {
		var rec sapDeploymentRecord
		if err := d.get(ctx, hc, "/lm/deployments/"+url.PathEscape(d.pinned), &rec); err != nil {
			return "", err
		}
		switch {
		case rec.ScenarioID != "" && rec.ScenarioID != sapScenario:
			return "", fmt.Errorf("openai-chat: sap-orchestration: deployment %s runs scenario %q, not %s", d.pinned, rec.ScenarioID, sapScenario)
		case rec.Status != "RUNNING":
			return "", fmt.Errorf("openai-chat: sap-orchestration: deployment %s is %s, not RUNNING", d.pinned, orUnknown(rec.Status))
		}
		if rec.ID == "" {
			rec.ID = d.pinned
		}
		return d.completionURL(rec)
	}
	q := url.Values{"scenarioId": {sapScenario}, "status": {"RUNNING"}}
	var list struct {
		Resources []sapDeploymentRecord `json:"resources"`
	}
	if err := d.get(ctx, hc, "/lm/deployments?"+q.Encode(), &list); err != nil {
		return "", err
	}
	var best *sapDeploymentRecord
	var bestAt time.Time
	for i := range list.Resources {
		r := &list.Resources[i]
		if r.Status != "RUNNING" || r.ScenarioID != sapScenario || (r.ID == "" && r.DeploymentURL == "") {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.CreatedAt)
		if best == nil || at.After(bestAt) {
			best, bestAt = r, at
		}
	}
	if best == nil {
		return "", errors.New("openai-chat: sap-orchestration: the resource group has no RUNNING orchestration deployment; create one in SAP AI Launchpad, or check AI-Resource-Group")
	}
	return d.completionURL(*best)
}

// completionURL is rec's /v2/completion. The deployment URL must be on
// the AI API's own scheme and host: the bearer token goes wherever it
// points.
func (d *sapDeployment) completionURL(rec sapDeploymentRecord) (string, error) {
	base := strings.TrimRight(rec.DeploymentURL, "/")
	if base == "" {
		base = d.api.String() + "/inference/deployments/" + url.PathEscape(rec.ID)
	}
	u, err := url.Parse(base)
	if err != nil || !strings.EqualFold(u.Scheme, d.api.Scheme) || !strings.EqualFold(u.Host, d.api.Host) {
		return "", fmt.Errorf("openai-chat: sap-orchestration: deployment %s has its URL on another host than the AI API's (%s); the credential is not sent there", rec.ID, d.api.Host)
	}
	return base + "/v2/completion", nil
}

func (d *sapDeployment) get(ctx context.Context, hc *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api.String()+path, nil)
	if err != nil {
		return fmt.Errorf("openai-chat: sap-orchestration: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("openai-chat: sap-orchestration: list deployments: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		e := sapAPIError(resp)
		return fmt.Errorf("openai-chat: sap-orchestration: list deployments: %w", e)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("openai-chat: sap-orchestration: decode deployments: %w", err)
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

// Request.

// sapMessage is a chat message as orchestration takes it: Chat
// Completions' own, except that reasoning is a list of blocks.
type sapMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"`
	ReasoningContent []sapReasonBlock `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall       `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type sapReasonBlock struct {
	Content   string `json:"content"`
	Signature string `json:"signature,omitempty"`
}

func toSAPMessage(m message) sapMessage {
	out := sapMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if m.ReasoningContent != nil && *m.ReasoningContent != "" {
		out.ReasoningContent = []sapReasonBlock{{Content: *m.ReasoningContent}}
	}
	return out
}

// sapPlaceholder opens an orchestration template placeholder.
const sapPlaceholder = "{{?"

// sapEncode wraps body in orchestration's envelope. The conversation
// goes to messages_history, which orchestration passes through as it
// stands, and only the newest message to prompt.template, which it
// renders: a template must hold at least one message, and tools and
// the response format live beside it. The template is rendered for
// {{?name}} placeholders, so a newest message containing one is refused
// rather than sent to be rewritten.
func sapEncode(body *chatRequest, extra map[string]any) ([]byte, error) {
	if len(body.Messages) == 0 {
		return nil, errors.New("openai-chat: sap-orchestration: the request has no messages")
	}
	last := body.Messages[len(body.Messages)-1]
	if hasSAPPlaceholder(last.Content) {
		return nil, fmt.Errorf("openai-chat: sap-orchestration: the newest message contains %q, which orchestration's template would read as a placeholder; the request is refused rather than sent to be rewritten", sapPlaceholder)
	}
	prompt := map[string]any{"template": []sapMessage{toSAPMessage(last)}}
	if len(body.Tools) > 0 {
		prompt["tools"] = body.Tools
	}
	if body.ResponseFormat != nil {
		prompt["response_format"] = body.ResponseFormat
	}

	params := map[string]any{}
	setIf := func(k string, v any, ok bool) {
		if ok {
			params[k] = v
		}
	}
	setIf("max_tokens", body.MaxTokens, body.MaxTokens != nil)
	setIf("temperature", body.Temperature, body.Temperature != nil)
	setIf("top_p", body.TopP, body.TopP != nil)
	setIf("stop", body.Stop, len(body.Stop) > 0)
	setIf("seed", body.Seed, body.Seed != nil)
	setIf("presence_penalty", body.PresencePenalty, body.PresencePenalty != nil)
	setIf("frequency_penalty", body.FrequencyPenalty, body.FrequencyPenalty != nil)
	// tool_choice is not in orchestration's template schema; SAP's SDK
	// passes it as a model parameter, which the model's provider honors
	// or not.
	setIf("tool_choice", body.ToolChoice, body.ToolChoice != nil)
	setIf("stream_options", body.StreamOptions, body.Stream && body.StreamOptions != nil)

	modules := map[string]any{}
	for k, v := range extra {
		if k == sapModulesKey {
			mods, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("openai-chat: sap-orchestration: extra_body %q must be an object of orchestration modules", sapModulesKey)
			}
			for name, cfg := range mods {
				if name == "prompt_templating" {
					return nil, fmt.Errorf("openai-chat: sap-orchestration: extra_body %q sets prompt_templating, which the adapter builds itself", sapModulesKey)
				}
				modules[name] = cfg
			}
			continue
		}
		if _, set := params[k]; set {
			return nil, fmt.Errorf("openai-chat: extra_body sets %q, which this request already sets", k)
		}
		params[k] = v
	}
	model := map[string]any{"name": body.Model}
	if len(params) > 0 {
		model["params"] = params
	}
	modules["prompt_templating"] = map[string]any{"prompt": prompt, "model": model}

	config := map[string]any{"modules": modules}
	if body.Stream {
		config["stream"] = map[string]any{"enabled": true}
	}
	req := map[string]any{"config": config}
	if hist := body.Messages[:len(body.Messages)-1]; len(hist) > 0 {
		out := make([]sapMessage, len(hist))
		for i, m := range hist {
			out[i] = toSAPMessage(m)
		}
		req["messages_history"] = out
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai-chat: encode request: %w", err)
	}
	return raw, nil
}

// hasSAPPlaceholder reports whether a message content (a string or a
// list of parts) contains a template placeholder's opening.
func hasSAPPlaceholder(content any) bool {
	switch c := content.(type) {
	case string:
		return strings.Contains(c, sapPlaceholder)
	case []contentPart:
		for _, p := range c {
			if strings.Contains(p.Text, sapPlaceholder) {
				return true
			}
		}
	}
	return false
}

// Response.

// sapResult is orchestration's LLM module result: a Chat Completion,
// or one chunk of one, with reasoning as a list of blocks.
type sapResult struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []sapChoice `json:"choices"`
	Usage   *wireUsage  `json:"usage"`
}

type sapChoice struct {
	Index        int        `json:"index"`
	Message      sapRespMsg `json:"message"`
	Delta        sapRespMsg `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type sapRespMsg struct {
	Content          *string        `json:"content"`
	Refusal          *string        `json:"refusal"`
	ToolCalls        []respToolCall `json:"tool_calls"`
	ReasoningContent sapReasoning   `json:"reasoning_content"`
}

func (m sapRespMsg) chat() respMessage {
	return respMessage{Content: m.Content, Refusal: m.Refusal, ToolCalls: m.ToolCalls, ReasoningContent: m.ReasoningContent.text}
}

// sapReasoning reads reasoning_content: a list of {content, signature}
// blocks in SAP's spec, joined; a plain string is taken as it stands.
type sapReasoning struct{ text *string }

func (r *sapReasoning) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		r.text = &s
		return nil
	}
	var blocks []sapReasonBlock
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	var sb strings.Builder
	for _, blk := range blocks {
		sb.WriteString(blk.Content)
	}
	if sb.Len() > 0 {
		s := sb.String()
		r.text = &s
	}
	return nil
}

// sapEnvelope is a complete answer or one stream event. final_result is
// the v2 name; orchestration_result the v1 one, read when it is all
// there is.
type sapEnvelope struct {
	RequestID           string          `json:"request_id"`
	FinalResult         *sapResult      `json:"final_result"`
	OrchestrationResult *sapResult      `json:"orchestration_result"`
	Error               json.RawMessage `json:"error"`
}

func (e sapEnvelope) result() *sapResult {
	if e.FinalResult != nil {
		return e.FinalResult
	}
	return e.OrchestrationResult
}

// sapComplete reads a complete answer as a chat response. The id is
// orchestration's request_id, the one SAP support asks for.
func sapComplete(r io.Reader) (*chatResponse, error) {
	var env sapEnvelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return nil, fmt.Errorf("openai-chat: decode response: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, sapErrorFrom(env.Error, 0)
	}
	res := env.result()
	if res == nil {
		return nil, errors.New("openai-chat: sap-orchestration: the response has no final_result")
	}
	cr := &chatResponse{ID: firstNonEmpty(env.RequestID, res.ID), Model: res.Model, Usage: res.Usage}
	for _, ch := range res.Choices {
		c := choice{Index: ch.Index, Message: ch.Message.chat()}
		if ch.FinishReason != nil {
			c.FinishReason = *ch.FinishReason
		}
		cr.Choices = append(cr.Choices, c)
	}
	return cr, nil
}

// sapChunk reads one stream event's data as a chunk.
func sapChunk(data []byte) (*chunk, error) {
	var env sapEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("openai-chat: decode stream chunk: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, sapErrorFrom(env.Error, 0)
	}
	c := &chunk{ID: env.RequestID}
	res := env.result()
	if res == nil {
		return c, nil
	}
	if c.ID == "" {
		c.ID = res.ID
	}
	c.Model, c.Usage = res.Model, res.Usage
	for _, ch := range res.Choices {
		c.Choices = append(c.Choices, chunkChoice{Index: ch.Index, Delta: ch.Delta.chat(), FinishReason: ch.FinishReason})
	}
	return c, nil
}

// Errors.

// sapErr is orchestration's error: a code (an HTTP status), a message,
// and where in the pipeline it happened (a module name).
type sapErr struct {
	Code      json.RawMessage `json:"code"`
	Message   string          `json:"message"`
	Location  string          `json:"location"`
	RequestID string          `json:"request_id"`
}

// sapAPIError reads a non-2xx answer: orchestration's {"error": …}, or
// the AI API's own error shape, or whatever text came back.
func sapAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Error) > 0 && string(env.Error) != "null" {
		return sapErrorFrom(env.Error, resp.StatusCode)
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg}
}

// sapErrorFrom reads an error object, or a list of them (one per
// module configuration tried), into an APIError.
func sapErrorFrom(raw json.RawMessage, status int) error {
	var list []sapErr
	if raw[0] == '[' {
		_ = json.Unmarshal(raw, &list)
	} else {
		var one sapErr
		if json.Unmarshal(raw, &one) == nil {
			list = []sapErr{one}
		}
	}
	if len(list) == 0 {
		// The AI API's own errors put a string here.
		var s string
		_ = json.Unmarshal(raw, &s)
		return &APIError{StatusCode: status, Message: s}
	}
	e := &APIError{StatusCode: status, Type: list[0].Location, Code: sapCode(list[0].Code)}
	msgs := make([]string, 0, len(list))
	for _, x := range list {
		if x.Message != "" {
			msgs = append(msgs, x.Message)
		}
	}
	e.Message = strings.Join(msgs, "; ")
	return e
}

// sapCode renders orchestration's numeric code, or the AI API's string
// one.
func sapCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strconv.Quote(string(raw))
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
