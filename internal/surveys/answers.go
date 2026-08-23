package surveys

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// MaxAnswers caps the distinct question_ids one response may hold. The 64 KB
// body limit bounds a single request; amends accumulate, so without this an
// unauthenticated caller could grow one row without limit (design spec §2.6).
const MaxAnswers = 500

// ParseAnswers decodes the responses parameter: a string holding a JSON
// array of objects (spec §7.2). Structure is strict -- anything that is not
// an array of objects each carrying an integral question_id is
// ErrMalformedAnswers. Values are lenient, matching the reference's
// attribute coercion: answer, question_type and question_label become
// strings (numbers and booleans stringified, null/absent -> ""), extra keys
// are dropped, and a repeated question_id keeps its first position with its
// last value so the caller always sees one answer per question.
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

// questionID accepts a JSON number with no fractional part or a string of
// digits; anything else (absent, null, 1.5, "abc") is malformed.
func questionID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f != math.Trunc(f) || math.Abs(f) > math.MaxInt32*1024 {
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

// stringish coerces a scalar JSON value to its string form; null, absent,
// and non-scalars become "".
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
	default:
		return ""
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
