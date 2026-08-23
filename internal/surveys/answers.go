package surveys

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// MaxAnswers caps the distinct question_ids one response may hold. The 64 KB
// body limit bounds a single request; amends accumulate, so without this an
// unauthenticated caller could grow one row without limit (design spec §2.6).
const MaxAnswers = 500

// MaxAnswerBytes, MaxQuestionLabelBytes, and MaxQuestionTypeBytes cap the
// three string fields of one answer element, checked in bytes after
// stringish coercion (design spec §2.5). Combined with MaxAnswers this
// bounds a stored response row to a few MB, which matters because every
// amend rewrites the whole row inside one immediate transaction that holds
// the process-wide write lock (design spec §2.6): an unbounded row would
// serialize every other survey write behind copying it.
const (
	MaxAnswerBytes        = 4096
	MaxQuestionLabelBytes = 1024
	MaxQuestionTypeBytes  = 64
)

// ParseAnswers decodes the responses parameter: a string holding a JSON
// array of objects (spec §7.2). Structure is strict -- anything that is not
// an array of objects each carrying an integral question_id is
// ErrMalformedAnswers. Values are lenient, matching the reference's
// attribute coercion: answer, question_type and question_label become
// strings (numbers and booleans stringified, null/absent -> "", arrays and
// objects kept as compact JSON text), extra keys are dropped, and a repeated question_id keeps its first position with its
// last value so the caller always sees one answer per question. Each of the
// three string fields is capped (MaxAnswerBytes, MaxQuestionLabelBytes,
// MaxQuestionTypeBytes); exceeding any one is ErrAnswerTooLong.
func ParseAnswers(raw string) ([]Answer, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &elems); err != nil || elems == nil {
		return nil, ErrMalformedAnswers
	}
	out := make([]Answer, 0, len(elems))
	index := make(map[int64]int, len(elems))
	for _, elem := range elems {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(elem, &fields); err != nil || fields == nil {
			return nil, ErrMalformedAnswers
		}
		id, ok := questionID(fields["question_id"])
		if !ok {
			return nil, ErrMalformedAnswers
		}
		a := Answer{
			QuestionID:    id,
			QuestionType:  stringish(fields["question_type"]),
			QuestionLabel: stringish(fields["question_label"]),
			Answer:        stringish(fields["answer"]),
		}
		if len(a.Answer) > MaxAnswerBytes || len(a.QuestionLabel) > MaxQuestionLabelBytes || len(a.QuestionType) > MaxQuestionTypeBytes {
			return nil, ErrAnswerTooLong
		}
		if i, seen := index[id]; seen {
			out[i] = a
			continue
		}
		index[id] = len(out)
		out = append(out, a)
	}
	if len(out) > MaxAnswers {
		return nil, ErrTooManyAnswers
	}
	return out, nil
}

// questionID accepts a JSON number with no fractional part, or a string that
// strconv.ParseInt accepts after trimming; anything else (absent, null, 1.5,
// "abc") is malformed.
func questionID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		// Beyond 2^53 a float64 cannot represent the integer exactly, so the id
		// the client meant is unknowable; reject to prevent silent corruption.
		if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return 0, false
		}
		return int64(f), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// stringish coerces a JSON value to its string form: strings as-is,
// numbers and booleans formatted, null and absent "". An array or object
// (a client sending a native checkbox list rather than the string form
// both shipped apps use) is kept as its compact JSON text: the server never
// interprets answers, and an answer that vanishes with a 201 is the one
// outcome an agency cannot detect.
func stringish(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			return ""
		}
		return b.String()
	}
}

// MergeAnswers applies an amend (spec §7.2): an incoming answer replaces the
// stored answer with the same question_id in place; other stored answers
// are kept; new question_ids are appended in incoming order. Neither input
// is mutated.
func MergeAnswers(stored, incoming []Answer) []Answer {
	out := make([]Answer, 0, len(stored)+len(incoming))
	index := make(map[int64]int, len(stored))
	for _, a := range stored {
		index[a.QuestionID] = len(out)
		out = append(out, a)
	}
	for _, a := range incoming {
		if i, ok := index[a.QuestionID]; ok {
			out[i] = a
			continue
		}
		index[a.QuestionID] = len(out)
		out = append(out, a)
	}
	return out
}
