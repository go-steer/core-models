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

package kvmetrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// sample is one line of the Prometheus text exposition format: a metric
// name, its labels, and a value. Timestamps, when present, are read and
// dropped; a sampler times readings itself.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// parse reads the Prometheus text exposition format (version 0.0.4):
// comment and blank lines are skipped, and every sample line becomes a
// sample. It is deliberately small — counters and gauges are all a
// KV-cache sampler needs, and histogram/summary series are just more
// named samples to it — so the core module gains no dependency
// (AGENTS.md rule 10). A malformed line fails the whole read, with its
// line number: a half-parsed exposition would report wrong numbers.
func parse(r io.Reader) ([]sample, error) {
	var out []sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("kvmetrics: line %d: %w", n, err)
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kvmetrics: %w", err)
	}
	return out, nil
}

func parseLine(line string) (sample, error) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return sample{}, fmt.Errorf("no value in %q", line)
	}
	s := sample{name: line[:i]}
	if !validName(s.name) {
		return sample{}, fmt.Errorf("bad metric name %q", s.name)
	}
	rest := strings.TrimLeft(line[i:], " \t")
	if rest != "" && rest[0] == '{' {
		labels, n, err := parseLabels(rest)
		if err != nil {
			return sample{}, err
		}
		s.labels = labels
		rest = rest[n:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 {
		return sample{}, fmt.Errorf("want a value and an optional timestamp after %q", s.name)
	}
	v, err := parseValue(fields[0])
	if err != nil {
		return sample{}, err
	}
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return sample{}, fmt.Errorf("bad timestamp %q", fields[1])
		}
	}
	s.value = v
	return s, nil
}

// parseLabels reads a {k="v",…} block at the start of s and returns the
// labels and how many bytes it consumed. Values may escape \\, \" and \n.
func parseLabels(s string) (map[string]string, int, error) {
	labels := map[string]string{}
	i := 1 // past '{'
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated label set")
		}
		if s[i] == '}' {
			return labels, i + 1, nil
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, 0, fmt.Errorf("label without '='")
		}
		key := strings.TrimSpace(s[i : i+eq])
		if !validLabel(key) {
			return nil, 0, fmt.Errorf("bad label name %q", key)
		}
		i += eq + 1
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] != '"' {
			return nil, 0, fmt.Errorf("label %q value is not quoted", key)
		}
		i++
		var b strings.Builder
		closed := false
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case 'n':
					b.WriteByte('\n')
				case '\\', '"':
					b.WriteByte(s[i+1])
				default:
					b.WriteByte('\\')
					b.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			b.WriteByte(c)
			i++
		}
		if !closed {
			return nil, 0, fmt.Errorf("label %q value is unterminated", key)
		}
		labels[key] = b.String()
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i < len(s) && s[i] == ',' {
			i++
		}
	}
}

func parseValue(f string) (float64, error) {
	switch f {
	case "NaN":
		return math.NaN(), nil
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	v, err := strconv.ParseFloat(f, 64)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", f)
	}
	return v, nil
}

func validName(n string) bool {
	for i, c := range n {
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return n != ""
}

func validLabel(n string) bool {
	return n != "" && !strings.Contains(n, ":") && validName(n)
}
