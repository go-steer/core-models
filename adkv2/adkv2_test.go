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

package adkv2_test

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/adkv2"
	"github.com/go-steer/core-models/llm"
)

// The mirror must have every field ADK's type has, with the same type,
// and nothing ADK's lacks. This is what fails the day an ADK bump adds
// a field — the conversions would otherwise drop it in silence.
func TestResponseMirrorsADKFieldForField(t *testing.T) {
	assertSameFields(t, reflect.TypeFor[model.LLMResponse](), reflect.TypeFor[llm.Response](), nil)
}

func TestRequestMirrorsADKMinusTools(t *testing.T) {
	assertSameFields(t, reflect.TypeFor[model.LLMRequest](), reflect.TypeFor[llm.Request](), map[string]bool{"Tools": true})
}

func assertSameFields(t *testing.T, adk, mirror reflect.Type, skip map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	for f := range adk.Fields() {
		if skip[f.Name] {
			continue
		}
		seen[f.Name] = true
		m, ok := mirror.FieldByName(f.Name)
		if !ok {
			t.Errorf("%s.%s (%s) has no counterpart in %s", adk, f.Name, f.Type, mirror)
			continue
		}
		if m.Type != f.Type {
			t.Errorf("%s.%s is %s, mirror has %s", adk, f.Name, f.Type, m.Type)
		}
	}
	for f := range mirror.Fields() {
		if !seen[f.Name] {
			t.Errorf("%s.%s has no counterpart in %s", mirror, f.Name, adk)
		}
	}
}

// fill sets every field of the struct v points at to a non-zero value,
// so a conversion that forgets a field loses something DeepEqual sees.
func fill(t *testing.T, v any) {
	t.Helper()
	s := reflect.ValueOf(v).Elem()
	for i := range s.NumField() {
		f := s.Field(i)
		switch f.Kind() {
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Map:
			m := reflect.MakeMap(f.Type())
			m.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf(any("v")).Convert(f.Type().Elem()))
			f.Set(m)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Float64:
			f.SetFloat(0.5)
		default:
			t.Fatalf("fill: no rule for %s.%s of kind %s", s.Type(), s.Type().Field(i).Name, f.Kind())
		}
	}
}

func TestResponseRoundTripsThroughBothDirections(t *testing.T) {
	want := &llm.Response{}
	fill(t, want)
	if got := adkv2.Response(adkv2.ADKResponse(want)); !reflect.DeepEqual(got, want) {
		t.Errorf("llm → ADK → llm lost something:\n got %+v\nwant %+v", got, want)
	}
	adk := &model.LLMResponse{}
	fill(t, adk)
	if got := adkv2.ADKResponse(adkv2.Response(adk)); !reflect.DeepEqual(got, adk) {
		t.Errorf("ADK → llm → ADK lost something:\n got %+v\nwant %+v", got, adk)
	}
}

func TestRequestRoundTripDropsOnlyTools(t *testing.T) {
	adk := &model.LLMRequest{}
	fill(t, adk)
	got := adkv2.ADKRequest(adkv2.Request(adk))
	if got.Tools != nil {
		t.Errorf("Tools = %v, want nil: it has no counterpart", got.Tools)
	}
	adk.Tools = nil
	if !reflect.DeepEqual(got, adk) {
		t.Errorf("ADK → llm → ADK lost something besides Tools:\n got %+v\nwant %+v", got, adk)
	}
}

// fake is a core-models LLM that records the request it was sent and
// yields a scripted sequence.
type fake struct {
	got   *llm.Request
	yield []*llm.Response
	err   error
}

func (f *fake) Name() string { return "fake-model" }

func (f *fake) GenerateContent(_ context.Context, req *llm.Request, _ bool) iter.Seq2[*llm.Response, error] {
	f.got = req
	return func(yield func(*llm.Response, error) bool) {
		for _, r := range f.yield {
			if !yield(r, nil) {
				return
			}
		}
		if f.err != nil {
			yield(nil, f.err)
		}
	}
}

func TestWrapDrivesACoreModelThroughADKsInterface(t *testing.T) {
	boom := errors.New("provider said no")
	f := &fake{
		yield: []*llm.Response{
			{Partial: true, Content: genai.NewContentFromText("hel", genai.RoleModel)},
			{TurnComplete: true, ModelVersion: "fake-model-001", Content: genai.NewContentFromText("hello", genai.RoleModel)},
		},
		err: boom,
	}
	m := adkv2.Wrap(f)
	if m.Name() != "fake-model" {
		t.Errorf("Name = %q", m.Name())
	}
	req := &model.LLMRequest{Model: "fake-model", Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	var got []*model.LLMResponse
	var gotErr error
	for r, err := range m.GenerateContent(context.Background(), req, true) {
		if err != nil {
			gotErr = err
			continue
		}
		got = append(got, r)
	}
	if f.got == nil || f.got.Model != "fake-model" || len(f.got.Contents) != 1 {
		t.Errorf("the core model was sent %+v", f.got)
	}
	if len(got) != 2 || !got[0].Partial || got[1].ModelVersion != "fake-model-001" {
		t.Errorf("responses = %+v", got)
	}
	if !errors.Is(gotErr, boom) {
		t.Errorf("err = %v, want the provider's error passed through", gotErr)
	}
}

func TestWrapStopsWhenTheConsumerDoes(t *testing.T) {
	f := &fake{yield: []*llm.Response{{Partial: true}, {Partial: true}, {TurnComplete: true}}}
	n := 0
	for range adkv2.Wrap(f).GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("yielded %d responses after the consumer stopped", n)
	}
}

func TestWrapAndFromADKUnwrapEachOther(t *testing.T) {
	f := &fake{}
	if got := adkv2.FromADK(adkv2.Wrap(f)); got != llm.LLM(f) {
		t.Errorf("FromADK(Wrap(m)) = %T, want m itself", got)
	}
}
