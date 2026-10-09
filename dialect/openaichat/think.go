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

import (
	"strings"
	"unicode"
)

const (
	openTag  = "<think>"
	closeTag = "</think>"
)

// thinkSplitter separates a leading <think>…</think> block from the
// answer, for servers that put a model's reasoning inline in content
// rather than in reasoning_content (Ollama does, and vLLM without a
// reasoning parser). It is fed content in whatever pieces the server
// sends — the tags themselves can arrive split across stream chunks —
// and returns, per piece, the reasoning and answer text it can already
// commit to.
//
// Only a block at the very start of the answer is reasoning. Text that
// merely mentions <think> later on is the model's answer and is left
// alone.
type thinkSplitter struct {
	state   int // splitStart, splitThink, splitAnswer
	pending string
	// answerStarted is set once a non-space answer character has been
	// emitted; whitespace between </think> and the answer is dropped.
	answerStarted bool
}

const (
	splitStart = iota
	splitThink
	splitAnswer
)

// feed consumes s and returns what can be emitted now.
func (t *thinkSplitter) feed(s string) (reasoning, answer string) {
	t.pending += s
	for {
		switch t.state {
		case splitStart:
			trimmed := strings.TrimLeftFunc(t.pending, unicode.IsSpace)
			switch {
			case strings.HasPrefix(trimmed, openTag):
				t.pending = trimmed[len(openTag):]
				t.state = splitThink
				continue
			case strings.HasPrefix(openTag, trimmed):
				return reasoning, answer // could still become <think>; wait
			default:
				t.state = splitAnswer
				continue
			}
		case splitThink:
			if i := strings.Index(t.pending, closeTag); i >= 0 {
				reasoning += t.pending[:i]
				t.pending = t.pending[i+len(closeTag):]
				t.state = splitAnswer
				continue
			}
			// Hold back a tail that could be the start of </think>.
			keep := partialSuffix(t.pending, closeTag)
			reasoning += t.pending[:len(t.pending)-keep]
			t.pending = t.pending[len(t.pending)-keep:]
			return reasoning, answer
		case splitAnswer:
			out := t.pending
			t.pending = ""
			if !t.answerStarted {
				out = strings.TrimLeftFunc(out, unicode.IsSpace)
				t.answerStarted = out != ""
			}
			return reasoning, answer + out
		}
	}
}

// flush returns whatever is still held when the response ends.
func (t *thinkSplitter) flush() (reasoning, answer string) {
	rest := t.pending
	t.pending = ""
	switch t.state {
	case splitThink:
		return rest, "" // an unterminated block is still reasoning
	case splitStart:
		return "", strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	if !t.answerStarted {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	return "", rest
}

// partialSuffix is the length of the longest suffix of s that is a
// proper prefix of tag.
func partialSuffix(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
