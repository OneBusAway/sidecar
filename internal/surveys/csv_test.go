package surveys_test

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// TestWriteResponsesCSV_LongFormat pins the long-format shape: one row per
// answer, a response with no answers still gets a row (abandoned
// submissions stay visible), and the header names every column.
func TestWriteResponsesCSV_LongFormat(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lat, lon := 47.6, -122.3
	responses := []surveys.Response{
		{
			PublicID: "two-answers", UserIdentifier: "dev-1", StopIdentifier: "1_570",
			StopLatitude: &lat, StopLongitude: &lon,
			Answers: []surveys.Answer{
				{QuestionID: 1, QuestionType: "radio", QuestionLabel: "How was your trip?", Answer: "Great"},
				{QuestionID: 77, QuestionType: "checkbox", QuestionLabel: "Modes", Answer: "[Bus, Train]"},
			},
			CreatedAt: base, UpdatedAt: base,
		},
		{
			PublicID: "abandoned", UserIdentifier: "dev-2",
			CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
		},
	}

	var buf bytes.Buffer
	if err := surveys.WriteResponsesCSV(&buf, surveys.Survey{}, responses); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, buf.String())
	}

	wantHeader := "response_id,user_identifier,stop_identifier,stop_latitude,stop_longitude,created_at,updated_at,question_id,question_type,question_label,answer"
	if got := strings.Join(rows[0], ","); got != wantHeader {
		t.Fatalf("header = %s", got)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d (%v), want header + 2 answers + 1 abandoned", len(rows), rows)
	}
	if rows[1][0] != "two-answers" || rows[1][2] != "1_570" || rows[1][3] != "47.6" || rows[1][10] != "Great" {
		t.Errorf("row 1 = %v", rows[1])
	}
	if rows[2][7] != "77" || rows[2][10] != "[Bus, Train]" {
		t.Errorf("row 2 = %v", rows[2])
	}
	if rows[3][0] != "abandoned" || rows[3][2] != "" || rows[3][3] != "" || rows[3][7] != "" || rows[3][10] != "" {
		t.Errorf("abandoned row = %v, want empty stop and answer cells", rows[3])
	}
	if !strings.HasSuffix(rows[1][5], "Z") || len(rows[1][5]) != len("2026-01-01T00:00:00.000Z") {
		t.Errorf("created_at = %q, want wire format", rows[1][5])
	}
}

// TestWriteResponsesCSV_NeutralizesFormulas pins finding 3: a rider-sourced
// cell that opens with a formula-trigger character (=, +, -, @, tab, CR)
// must not be handed to a spreadsheet as a live formula on open. A plain
// cell must pass through unchanged. This is the assertion that the guard
// survived the move out of the CLI into this package.
func TestWriteResponsesCSV_NeutralizesFormulas(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	responses := []surveys.Response{
		{
			PublicID: "-formula-row", UserIdentifier: "@evil",
			Answers: []surveys.Answer{
				{QuestionID: 1, QuestionType: "radio", QuestionLabel: "How was your trip?", Answer: "=1+1"},
			},
			CreatedAt: base, UpdatedAt: base,
		},
	}

	var buf bytes.Buffer
	if err := surveys.WriteResponsesCSV(&buf, surveys.Survey{}, responses); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d (%v), want header + 1 answer", len(rows), rows)
	}
	// securetoken's URL-safe alphabet includes '-', so ids need the guard too.
	if got := rows[1][0]; got != "'-formula-row" {
		t.Errorf("response_id cell = %q, want the apostrophe-guarded value", got)
	}
	if got := rows[1][1]; got != "'@evil" {
		t.Errorf("user_identifier cell = %q, want the apostrophe-guarded value", got)
	}
	if got := rows[1][10]; got != "'=1+1" {
		t.Errorf("answer cell = %q, want the apostrophe-guarded value", got)
	}

	// A plain answer (no formula-trigger prefix) must survive untouched.
	responses = append(responses, surveys.Response{
		PublicID: "plain-row",
		Answers: []surveys.Answer{
			{QuestionID: 999, QuestionType: "text", QuestionLabel: "Plain", Answer: "just text"},
		},
		CreatedAt: base, UpdatedAt: base,
	})
	buf.Reset()
	if err = surveys.WriteResponsesCSV(&buf, surveys.Survey{}, responses); err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, buf.String())
	}
	found := false
	for _, row := range rows[1:] {
		if row[7] == "999" {
			found = true
			if row[10] != "just text" {
				t.Errorf("plain answer cell = %q, want unchanged", row[10])
			}
		}
	}
	if !found {
		t.Fatal("plain answer row not found")
	}
}
