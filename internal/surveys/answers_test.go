package surveys_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

func TestParseAnswers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    []surveys.Answer
		wantErr error
	}{
		{"ios shape", `[{"question_id":21,"question_type":"radio","question_label":"Trip?","answer":"Great"}]`,
			[]surveys.Answer{{QuestionID: 21, QuestionType: "radio", QuestionLabel: "Trip?", Answer: "Great"}}, nil},
		// iOS checkbox answers are a JSON-array string; stored verbatim.
		{"ios checkbox verbatim", `[{"question_id":3,"answer":"[\"Bus\",\"Train\"]"}]`,
			[]surveys.Answer{{QuestionID: 3, Answer: `["Bus","Train"]`}}, nil},
		// Android checkbox answers are Kotlin List.toString(); stored verbatim.
		{"android checkbox verbatim", `[{"question_id":3,"question_type":"checkbox","question_label":"Modes","answer":"[Bus, Train]"}]`,
			[]surveys.Answer{{QuestionID: 3, QuestionType: "checkbox", QuestionLabel: "Modes", Answer: "[Bus, Train]"}}, nil},
		{"empty array", `[]`, []surveys.Answer{}, nil},
		{"numeric answer stringified", `[{"question_id":1,"answer":4}]`, []surveys.Answer{{QuestionID: 1, Answer: "4"}}, nil},
		{"bool answer stringified", `[{"question_id":1,"answer":true}]`, []surveys.Answer{{QuestionID: 1, Answer: "true"}}, nil},
		{"missing answer is empty", `[{"question_id":1}]`, []surveys.Answer{{QuestionID: 1}}, nil},
		{"null answer is empty", `[{"question_id":1,"answer":null}]`, []surveys.Answer{{QuestionID: 1}}, nil},
		// A native array or object answer is kept as its JSON text, never
		// silently dropped to "".
		{"array answer kept as json", `[{"question_id":3,"answer":[ "Bus", "Train" ]}]`, []surveys.Answer{{QuestionID: 3, Answer: `["Bus","Train"]`}}, nil},
		{"object answer kept as json", `[{"question_id":3,"answer":{"k": 1}}]`, []surveys.Answer{{QuestionID: 3, Answer: `{"k":1}`}}, nil},
		{"integral float id", `[{"question_id":21.0,"answer":"x"}]`, []surveys.Answer{{QuestionID: 21, Answer: "x"}}, nil},
		{"numeric string id", `[{"question_id":"21","answer":"x"}]`, []surveys.Answer{{QuestionID: 21, Answer: "x"}}, nil},
		{"extra keys dropped", `[{"question_id":1,"answer":"x","extra":"y"}]`, []surveys.Answer{{QuestionID: 1, Answer: "x"}}, nil},
		{"duplicate id last wins in place", `[{"question_id":1,"answer":"a"},{"question_id":2,"answer":"b"},{"question_id":1,"answer":"c"}]`,
			[]surveys.Answer{{QuestionID: 1, Answer: "c"}, {QuestionID: 2, Answer: "b"}}, nil},
		{"empty string", ``, nil, surveys.ErrMalformedAnswers},
		{"not json", `not-json`, nil, surveys.ErrMalformedAnswers},
		{"json null", `null`, nil, surveys.ErrMalformedAnswers},
		{"object not array", `{"question_id":1}`, nil, surveys.ErrMalformedAnswers},
		{"element not object", `[1]`, nil, surveys.ErrMalformedAnswers},
		{"element null", `[null]`, nil, surveys.ErrMalformedAnswers},
		{"missing question_id", `[{"answer":"x"}]`, nil, surveys.ErrMalformedAnswers},
		{"fractional question_id", `[{"question_id":1.5}]`, nil, surveys.ErrMalformedAnswers},
		{"non-numeric string id", `[{"question_id":"abc"}]`, nil, surveys.ErrMalformedAnswers},
		{"large integral id", `[{"question_id":5000000000000,"answer":"x"}]`, []surveys.Answer{{QuestionID: 5000000000000, Answer: "x"}}, nil},
		{"id beyond float64 exactness", `[{"question_id":1e17}]`, nil, surveys.ErrMalformedAnswers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := surveys.ParseAnswers(tt.raw)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("answers = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseAnswersCap(t *testing.T) {
	t.Parallel()
	build := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"question_id":%d,"answer":"x"}`, i+1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	if _, err := surveys.ParseAnswers(build(surveys.MaxAnswers)); err != nil {
		t.Fatalf("exactly MaxAnswers: err = %v, want nil", err)
	}
	if _, err := surveys.ParseAnswers(build(surveys.MaxAnswers + 1)); !errors.Is(err, surveys.ErrTooManyAnswers) {
		t.Fatalf("MaxAnswers+1: err = %v, want ErrTooManyAnswers", err)
	}
}

func TestParseAnswersFieldCaps(t *testing.T) {
	t.Parallel()
	build := func(field string, n int) string {
		return fmt.Sprintf(`[{"question_id":1,%q:%q}]`, field, strings.Repeat("x", n))
	}
	tests := []struct {
		name  string
		field string
		max   int
	}{
		{"answer", "answer", surveys.MaxAnswerBytes},
		{"question_label", "question_label", surveys.MaxQuestionLabelBytes},
		{"question_type", "question_type", surveys.MaxQuestionTypeBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := surveys.ParseAnswers(build(tt.field, tt.max)); err != nil {
				t.Fatalf("%s exactly at cap (%d bytes): err = %v, want nil", tt.field, tt.max, err)
			}
			if _, err := surveys.ParseAnswers(build(tt.field, tt.max+1)); !errors.Is(err, surveys.ErrAnswerTooLong) {
				t.Fatalf("%s cap+1 (%d bytes): err = %v, want ErrAnswerTooLong", tt.field, tt.max+1, err)
			}
		})
	}
}

func TestMergeAnswers(t *testing.T) {
	t.Parallel()
	stored := []surveys.Answer{{QuestionID: 1, Answer: "a"}, {QuestionID: 2, Answer: "b"}, {QuestionID: 3, Answer: "c"}}
	incoming := []surveys.Answer{{QuestionID: 2, Answer: "B"}, {QuestionID: 9, Answer: "z"}}
	got := surveys.MergeAnswers(stored, incoming)
	want := []surveys.Answer{{QuestionID: 1, Answer: "a"}, {QuestionID: 2, Answer: "B"}, {QuestionID: 3, Answer: "c"}, {QuestionID: 9, Answer: "z"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %+v, want %+v", got, want)
	}
	if stored[1].Answer != "b" {
		t.Fatal("MergeAnswers mutated its stored argument")
	}
	if got := surveys.MergeAnswers(stored, nil); !reflect.DeepEqual(got, stored) {
		t.Fatalf("merge with empty incoming = %+v, want stored unchanged", got)
	}
	if got := surveys.MergeAnswers(nil, incoming); !reflect.DeepEqual(got, incoming) {
		t.Fatalf("merge into empty stored = %+v, want incoming", got)
	}
}
