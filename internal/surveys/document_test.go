package surveys_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// TestDocumentFromSurvey_NilListsAndWindow pins finding 4's move: a survey
// with nil VisibleStopList/VisibleRouteList and no window must render a
// document whose corresponding pointers are nil and whose JSON emits
// literal null, not an omitted key or an empty array -- the `show` output
// is fed straight to `edit`, so a wrong shape there breaks the round trip
// (design spec §2.13).
func TestDocumentFromSurvey_NilListsAndWindow(t *testing.T) {
	t.Parallel()
	s := surveys.Survey{
		ID: 7, Name: "Rider satisfaction",
		Study:     surveys.Study{ID: 3, Name: "Study"},
		CreatedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}
	doc := surveys.DocumentFromSurvey(s)
	if doc.VisibleStopList != nil {
		t.Errorf("VisibleStopList = %#v, want nil", doc.VisibleStopList)
	}
	if doc.VisibleRouteList != nil {
		t.Errorf("VisibleRouteList = %#v, want nil", doc.VisibleRouteList)
	}
	if doc.StartDate != nil {
		t.Errorf("StartDate = %#v, want nil", doc.StartDate)
	}
	if doc.EndDate != nil {
		t.Errorf("EndDate = %#v, want nil", doc.EndDate)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"visible_stop_list":null`) {
		t.Errorf("json = %s, want visible_stop_list:null", got)
	}
	if !strings.Contains(got, `"start_date":null`) {
		t.Errorf("json = %s, want start_date:null", got)
	}
}
