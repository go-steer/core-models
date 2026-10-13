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
	"math"
	"strings"
	"testing"
)

func TestParseAcceptsTheExpositionFormat(t *testing.T) {
	in := `# HELP x A metric.
# TYPE x counter

x_total 3
x_total{a="1"} 4.5 1700000000000
y{a="quote \" here",b="back\\slash",c="line\nbreak",} 1e3
nan_metric NaN
pinf +Inf
ninf -Inf
spaced { a = "v" } 7
colon:name 2
`
	ss, err := parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 8 {
		t.Fatalf("parsed %d samples, want 8: %+v", len(ss), ss)
	}
	if ss[1].labels["a"] != "1" || ss[1].value != 4.5 {
		t.Errorf("labelled sample with timestamp = %+v", ss[1])
	}
	if l := ss[2].labels; l["a"] != `quote " here` || l["b"] != `back\slash` || l["c"] != "line\nbreak" || ss[2].value != 1000 {
		t.Errorf("escaped labels = %q, value %v", l, ss[2].value)
	}
	if !math.IsNaN(ss[3].value) || !math.IsInf(ss[4].value, 1) || !math.IsInf(ss[5].value, -1) {
		t.Errorf("special values = %v %v %v", ss[3].value, ss[4].value, ss[5].value)
	}
	if ss[6].labels["a"] != "v" || ss[6].value != 7 {
		t.Errorf("spaced label set = %+v", ss[6])
	}
	if ss[7].name != "colon:name" {
		t.Errorf("colon name = %q", ss[7].name)
	}
}

func TestParseRefusesMalformedLines(t *testing.T) {
	for name, line := range map[string]string{
		"no value":           "x",
		"bad value":          "x abc",
		"bad timestamp":      "x 1 soon",
		"too many fields":    "x 1 2 3",
		"unterminated set":   `x{a="1" 1`,
		"unquoted label":     "x{a=1} 1",
		"unterminated value": `x{a="1} 1`,
		"label without =":    `x{a} 1`,
		"bad name":           "1x 1",
		"colon in label":     `x{a:b="1"} 1`,
	} {
		if _, err := parse(strings.NewReader(line + "\n")); err == nil {
			t.Errorf("%s: %q parsed without error", name, line)
		}
	}
}
