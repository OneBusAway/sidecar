package export_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/export"
)

var base = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// validDoc carries one row of every id family the document holds.
func validDoc() *export.Document {
	return &export.Document{
		Format: export.Format, ExportedAt: base, RegionID: 1,
		Alerts: []export.Alert{{ID: 10, AgencyID: "1", HeaderText: "h", StartTime: base}},
		Studies: []export.Study{{ID: 20, Name: "s", Surveys: []export.Survey{{
			ID: 30, Name: "sv",
			Questions: []export.Question{{ID: 40, Position: 1, Content: json.RawMessage(`{"type":"text","label_text":"q"}`)}},
		}}}},
		SurveyResponses: []export.SurveyResponse{{SurveyID: 30, PublicID: "r1", UserIdentifier: "u"}},
	}
}

// TestValidate_IDFloor (SC-13): every id the document carries verbatim
// must sit below the sidecar's sequence floor. An id at or above it would
// either collide with ids this database mints after `sequence bump`, or --
// at MaxInt64 -- park an AUTOINCREMENT sequence where SQLite has no id left
// to mint, for every region sharing the table.
func TestValidate_IDFloor(t *testing.T) {
	t.Parallel()
	if export.DefaultIDFloor != 1_000_000 {
		t.Fatalf("DefaultIDFloor = %d; the runbook's `sequence bump --min` and OBACloud's ID_HEADROOM are 1000000", export.DefaultIDFloor)
	}
	families := map[string]func(d *export.Document, id int64){
		"alert":    func(d *export.Document, id int64) { d.Alerts[0].ID = id },
		"study":    func(d *export.Document, id int64) { d.Studies[0].ID = id },
		"survey":   func(d *export.Document, id int64) { d.Studies[0].Surveys[0].ID, d.SurveyResponses[0].SurveyID = id, id },
		"question": func(d *export.Document, id int64) { d.Studies[0].Surveys[0].Questions[0].ID = id },
		// A response may name a survey migrated by an earlier run, so its
		// survey_id is checked on its own too.
		"response survey_id": func(d *export.Document, id int64) { d.SurveyResponses[0].SurveyID = id },
	}
	for name, set := range families {
		for _, id := range []int64{export.DefaultIDFloor, export.DefaultIDFloor + 1, math.MaxInt64} {
			d := validDoc()
			set(d, id)
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), "sequence floor") {
				t.Errorf("%s id %d: err %v, want a sequence-floor rejection", name, id, err)
			}
		}
	}
	d := validDoc()
	d.Alerts[0].ID = export.DefaultIDFloor - 1
	if err := d.Validate(); err != nil {
		t.Fatalf("id just below the floor rejected: %v", err)
	}

	// A deployment that raised its floor in lockstep with OBACloud passes
	// the new floor; zero means the default.
	d = validDoc()
	d.Alerts[0].ID = 1_500_000
	d.IDFloor = 2_000_000
	if err := d.Validate(); err != nil {
		t.Fatalf("raised floor: %v", err)
	}
	d.IDFloor = 1_200_000
	if err := d.Validate(); err == nil {
		t.Fatal("id above a lowered floor accepted")
	}
}

// TestDocument_IDFloorNotOnTheWire: the floor is the importer's setting,
// never something the document can carry for itself.
func TestDocument_IDFloorNotOnTheWire(t *testing.T) {
	t.Parallel()
	d := validDoc()
	d.IDFloor = 5
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "floor") {
		t.Fatalf("IDFloor leaked into the wire format: %s", b)
	}
}
