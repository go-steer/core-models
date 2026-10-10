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

// Originally derived from google.golang.org/adk/v2@v2.5.0:internal/llminternal/stream_aggregator.go

package gemini

import (
	"maps"
	"reflect"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
)

// aggregator folds a stream's partial responses into the one complete
// response a non-streaming call would have returned: consecutive text
// of one kind (thought or answer) joined, function calls kept whole —
// including ones Gemini 3 streams argument by argument through
// PartialArgs — and the last usage, grounding and citation seen.
//
// It is ADK v2.5's aggregator. v1.7's splits an argument's JSON Path on
// dots alone, so an argument addressed by index ("$.items[0]") or by a
// quoted name landed under an invented key; v2.5 parses RFC 9535
// normalized paths and drops what it cannot parse. Unlike both, the
// complete response keeps the stream's ModelVersion.
type aggregator struct {
	usageMetadata     *genai.GenerateContentResponseUsageMetadata
	groundingMetadata *genai.GroundingMetadata
	citationMetadata  *genai.CitationMetadata
	last              *llm.Response
	modelVersion      string

	thoughtSignature []byte

	sequence      []*genai.Part
	text          string
	textIsThought bool
	finishReason  genai.FinishReason

	fnName             string
	fnID               string
	fnArgs             map[string]any
	fnThoughtSignature []byte
}

// add folds r into the aggregate and marks it partial.
func (s *aggregator) add(r *llm.Response) {
	s.last = r
	if r.UsageMetadata != nil {
		s.usageMetadata = r.UsageMetadata
	}
	if r.GroundingMetadata != nil {
		s.groundingMetadata = r.GroundingMetadata
	}
	if r.CitationMetadata != nil {
		s.citationMetadata = r.CitationMetadata
	}
	if r.ModelVersion != "" {
		s.modelVersion = r.ModelVersion
	}
	if r.FinishReason != "" {
		s.finishReason = r.FinishReason
	}
	r.Partial = true
	if r.Content == nil {
		return
	}
	for _, part := range r.Content.Parts {
		// Gemini 3 ends a stream with an empty part.
		if part == nil || reflect.ValueOf(*part).IsZero() {
			continue
		}
		if len(part.ThoughtSignature) > 0 {
			s.thoughtSignature = part.ThoughtSignature
		}
		switch {
		case part.Text != "":
			if s.text != "" && part.Thought != s.textIsThought {
				s.flushText()
			}
			if s.text == "" {
				s.textIsThought = part.Thought
			}
			s.text += part.Text
		case part.FunctionCall != nil:
			s.addFunctionCall(part)
		default:
			s.flushText()
			s.sequence = append(s.sequence, part)
		}
	}
}

func (s *aggregator) addFunctionCall(part *genai.Part) {
	fc := part.FunctionCall
	if fc.PartialArgs != nil || (fc.WillContinue != nil && *fc.WillContinue) {
		if len(part.ThoughtSignature) > 0 && s.fnThoughtSignature == nil {
			s.fnThoughtSignature = part.ThoughtSignature
		}
		s.addStreamingFunctionCall(part)
		return
	}
	if fc.Name == "" {
		return
	}
	s.flushText()
	if part.ThoughtSignature == nil && s.thoughtSignature != nil {
		part.ThoughtSignature = s.thoughtSignature
	}
	s.thoughtSignature = nil
	s.sequence = append(s.sequence, part)
}

func (s *aggregator) addStreamingFunctionCall(part *genai.Part) {
	fc := part.FunctionCall
	if fc.Name != "" {
		s.fnName = fc.Name
	}
	if fc.ID != "" {
		s.fnID = fc.ID
	}
	for _, arg := range fc.PartialArgs {
		segments, ok := parseJSONPath(arg.JsonPath)
		if !ok {
			continue
		}
		value, ok := s.partialValue(arg, segments)
		if !ok {
			continue
		}
		if s.fnArgs == nil {
			s.fnArgs = map[string]any{}
		}
		// Arguments are an object, so a path that starts at an array
		// index addresses nothing.
		if args, ok := setInto(s.fnArgs, segments, value).(map[string]any); ok {
			s.fnArgs = args
		}
	}
	if fc.WillContinue != nil && *fc.WillContinue {
		return
	}
	s.flushText()
	s.flushFunctionCall()
}

func (s *aggregator) partialValue(arg *genai.PartialArg, segments []pathSegment) (any, bool) {
	switch {
	case arg.StringValue != "":
		// A string streams in chunks that share one path: append.
		existing, _ := valueByPath(s.fnArgs, segments)
		if str, ok := existing.(string); ok {
			return str + arg.StringValue, true
		}
		return arg.StringValue, true
	case arg.NumberValue != nil:
		return *arg.NumberValue, true
	case arg.BoolValue != nil:
		return *arg.BoolValue, true
	case arg.NULLValue != "":
		return nil, true
	}
	return nil, false
}

func (s *aggregator) flushText() {
	if s.text == "" {
		return
	}
	s.sequence = append(s.sequence, &genai.Part{Text: s.text, Thought: s.textIsThought})
	s.text, s.textIsThought = "", false
}

func (s *aggregator) flushFunctionCall() {
	if s.fnName == "" {
		return
	}
	p := &genai.Part{FunctionCall: &genai.FunctionCall{Name: s.fnName, Args: maps.Clone(s.fnArgs), ID: s.fnID}}
	if s.fnThoughtSignature != nil {
		p.ThoughtSignature = s.fnThoughtSignature
	}
	s.sequence = append(s.sequence, p)
	s.fnName, s.fnID, s.fnThoughtSignature, s.fnArgs = "", "", nil, nil
}

// close returns the complete response, or nil for a stream that sent
// nothing.
func (s *aggregator) close() *llm.Response {
	if s.last == nil {
		return nil
	}
	s.flushText()
	s.flushFunctionCall()
	r := &llm.Response{
		Content:           &genai.Content{Parts: s.sequence, Role: genai.RoleModel},
		UsageMetadata:     s.usageMetadata,
		GroundingMetadata: s.groundingMetadata,
		CitationMetadata:  s.citationMetadata,
		FinishReason:      s.finishReason,
		ModelVersion:      s.modelVersion,
	}
	if s.finishReason != genai.FinishReasonStop {
		r.ErrorCode = s.last.ErrorCode
		r.ErrorMessage = s.last.ErrorMessage
	}
	return r
}

// pathSegment is one step of a JSON Path: an object member name, or an
// array index when isIndex is set.
type pathSegment struct {
	name    string
	index   int
	isIndex bool
}

// maxPathIndex bounds an array index, which keeps it from overflowing
// int and bounds the slice one index can grow.
const maxPathIndex = 1 << 16

// parseJSONPath splits the RFC 9535 normalized path that addresses a
// streamed argument — "$.name", "$['name']", "$[0]" — into segments.
// Anything else reports false, so the chunk is dropped rather than
// written to an invented key.
func parseJSONPath(jsonPath string) ([]pathSegment, bool) {
	rest := strings.TrimPrefix(jsonPath, "$")
	var segments []pathSegment
	for rest != "" {
		if rest[0] == '[' {
			segment, remainder, ok := parseBracketSegment(rest)
			if !ok {
				return nil, false
			}
			segments, rest = append(segments, segment), remainder
			continue
		}
		if rest[0] == '.' {
			rest = rest[1:]
		} else if len(segments) > 0 {
			return nil, false
		}
		name, remainder := rest, ""
		if i := strings.IndexAny(rest, ".["); i >= 0 {
			name, remainder = rest[:i], rest[i:]
		}
		if name == "" {
			return nil, false
		}
		segments, rest = append(segments, pathSegment{name: name}), remainder
	}
	return segments, len(segments) > 0
}

func parseBracketSegment(path string) (pathSegment, string, bool) {
	body := path[1:]
	if body != "" && (body[0] == '\'' || body[0] == '"') {
		name, rest, ok := parseQuotedName(body)
		if !ok || rest == "" || rest[0] != ']' {
			return pathSegment{}, "", false
		}
		return pathSegment{name: name}, rest[1:], true
	}
	end := strings.IndexByte(body, ']')
	if end <= 0 {
		return pathSegment{}, "", false
	}
	index := 0
	for i := range end {
		d := body[i]
		if d < '0' || d > '9' {
			return pathSegment{}, "", false
		}
		index = index*10 + int(d-'0')
		if index > maxPathIndex {
			return pathSegment{}, "", false
		}
	}
	return pathSegment{index: index, isIndex: true}, body[end+1:], true
}

func parseQuotedName(path string) (name, rest string, ok bool) {
	quote := path[0]
	var b strings.Builder
	for i := 1; i < len(path); i++ {
		switch c := path[i]; c {
		case quote:
			return b.String(), path[i+1:], true
		case '\\':
			i++
			if i == len(path) {
				return "", "", false
			}
			e, ok := unescape(path[i])
			if !ok {
				return "", "", false
			}
			b.WriteByte(e)
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}

func unescape(c byte) (byte, bool) {
	switch c {
	case '\'', '"', '\\', '/':
		return c, true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	}
	return 0, false
}

// setInto returns container with value written where segments point,
// creating maps and slices on the way. A slice grows to fit an index,
// so elements that arrive out of order keep their positions.
func setInto(container any, segments []pathSegment, value any) any {
	seg := segments[0]
	if seg.isIndex {
		elems, _ := container.([]any)
		for len(elems) <= seg.index {
			elems = append(elems, nil)
		}
		elems[seg.index] = descend(elems[seg.index], segments, value)
		return elems
	}
	members, ok := container.(map[string]any)
	if !ok {
		members = map[string]any{}
	}
	members[seg.name] = descend(members[seg.name], segments, value)
	return members
}

func descend(existing any, segments []pathSegment, value any) any {
	if len(segments) == 1 {
		return value
	}
	return setInto(existing, segments[1:], value)
}

func valueByPath(args map[string]any, segments []pathSegment) (any, bool) {
	var cur any = args
	for _, seg := range segments {
		if seg.isIndex {
			elems, ok := cur.([]any)
			if !ok || seg.index >= len(elems) {
				return nil, false
			}
			cur = elems[seg.index]
			continue
		}
		members, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = members[seg.name]; !ok {
			return nil, false
		}
	}
	return cur, true
}
