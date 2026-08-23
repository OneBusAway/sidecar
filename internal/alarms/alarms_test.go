package alarms_test

import (
	"encoding/json"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alarms"
)

func TestNormalizeSecondsBefore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    int64
		ok   bool
		want int64
	}{
		{300, true, 300},
		{0, true, 600},
		{-5, true, 600},
		{0, false, 600}, // absent or non-numeric
	}
	for _, tt := range tests {
		if got := alarms.NormalizeSecondsBefore(tt.v, tt.ok); got != tt.want {
			t.Errorf("NormalizeSecondsBefore(%d, %v) = %d; want %d", tt.v, tt.ok, got, tt.want)
		}
	}
}

func TestMessages(t *testing.T) {
	t.Parallel()
	if got, want := alarms.ComposeMessage("44", "Ballard", 600), "The 44 to Ballard leaves in 10 minutes"; got != want {
		t.Errorf("ComposeMessage = %q; want %q", got, want)
	}
	if got, want := alarms.ComposeMessage("E Line", "Aurora Village", 60), "The E Line to Aurora Village leaves in 1 minute"; got != want {
		t.Errorf("ComposeMessage = %q; want %q", got, want)
	}
	// Sub-minute lead times still say 1 minute, not 0.
	if got, want := alarms.GenericMessage(30), "The bus leaves in 1 minute"; got != want {
		t.Errorf("GenericMessage = %q; want %q", got, want)
	}
	if got, want := alarms.GenericMessage(600), "The bus leaves in 10 minutes"; got != want {
		t.Errorf("GenericMessage = %q; want %q", got, want)
	}
}

func TestPushData(t *testing.T) {
	t.Parallel()
	seq := int64(3)
	full := alarms.Alarm{RegionID: 1, StopID: "1_570", TripID: "1_604370",
		ServiceDate: 1754809200000, VehicleID: "1_4361", StopSequence: &seq}
	b, err := json.Marshal(full.PushData())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"arrival_and_departure":{"region_id":1,"service_date":1754809200000,` +
		`"stop_id":"1_570","stop_sequence":3,"trip_id":"1_604370","vehicle_id":"1_4361"}}`
	if string(b) != want {
		t.Errorf("PushData = %s\nwant     %s", b, want)
	}

	// Omitted fields are null, not "" or 0 (spec §5.2: "null trip fields").
	empty := alarms.Alarm{RegionID: 2}
	b, _ = json.Marshal(empty.PushData())
	want = `{"arrival_and_departure":{"region_id":2,"service_date":null,` +
		`"stop_id":null,"stop_sequence":null,"trip_id":null,"vehicle_id":null}}`
	if string(b) != want {
		t.Errorf("PushData(empty) = %s\nwant            %s", b, want)
	}

	// StopSequence: ptr(0) is a real value (first stop), marshals as 0, not null.
	zero := int64(0)
	zeroSeq := alarms.Alarm{RegionID: 3, StopSequence: &zero}
	b, _ = json.Marshal(zeroSeq.PushData())
	want = `{"arrival_and_departure":{"region_id":3,"service_date":null,` +
		`"stop_id":null,"stop_sequence":0,"trip_id":null,"vehicle_id":null}}`
	if string(b) != want {
		t.Errorf("PushData(zeroSeq) = %s\nwant             %s", b, want)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		until, before int64
		want          alarms.Decision
	}{
		{700, 600, alarms.Wait},
		{601, 600, alarms.Wait},
		{600, 600, alarms.Fire}, // boundary: not yet only when until > before
		{1, 600, alarms.Fire},
		{0, 600, alarms.Fire}, // leaving right now is still worth the push
		{-1, 600, alarms.Expire},
	}
	for _, tt := range tests {
		if got := alarms.Decide(tt.until, tt.before); got != tt.want {
			t.Errorf("Decide(%d, %d) = %v; want %v", tt.until, tt.before, got, tt.want)
		}
	}
}
