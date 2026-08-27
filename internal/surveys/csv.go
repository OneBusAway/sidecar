package surveys

import (
	"encoding/csv"
	"io"
	"strconv"

	"github.com/OneBusAway/sidecar/internal/csvsafe"
)

// WriteResponsesCSV writes long-format CSV: one row per answer, so no
// answer to a since-deleted question is lost and the sheet pivots cleanly;
// a response with no answers still gets a row so abandoned submissions are
// visible (design spec 2.14).
//
// survey is not read by the row builder below. It stays in the signature
// anyway: the HTTP handler has already loaded the survey for its tenancy
// check, and passing it here makes "these responses belong to this survey"
// part of the call itself rather than a convention a future caller could
// forget. (golangci-lint's unparam does not flag it: that check only fires
// on unexported functions, and this one is part of the package's exported
// API.)
func WriteResponsesCSV(w io.Writer, survey Survey, responses []Response) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"response_id", "user_identifier", "stop_identifier", "stop_latitude", "stop_longitude",
		"created_at", "updated_at", "question_id", "question_type", "question_label", "answer"}); err != nil {
		return err
	}
	for _, r := range responses {
		// The public id is server-minted, but its URL-safe base64 alphabet
		// includes '-', so about one id in 64 opens with a formula trigger.
		prefix := []string{csvsafe.Cell(r.PublicID), csvsafe.Cell(r.UserIdentifier), csvsafe.Cell(r.StopIdentifier), csvsafe.Float(r.StopLatitude), csvsafe.Float(r.StopLongitude),
			FormatTime(r.CreatedAt), FormatTime(r.UpdatedAt)}
		if len(r.Answers) == 0 {
			if err := cw.Write(append(prefix, "", "", "", "")); err != nil {
				return err
			}
			continue
		}
		for _, a := range r.Answers {
			row := append(append([]string{}, prefix...), strconv.FormatInt(a.QuestionID, 10), csvsafe.Cell(a.QuestionType), csvsafe.Cell(a.QuestionLabel), csvsafe.Cell(a.Answer))
			if err := cw.Write(row); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
