package alertpush_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/alertpush"
)

func TestNotifIDRoundTrip(t *testing.T) {
	if s := alertpush.NotifID(42); s != "alertpush:42" {
		t.Errorf("NotifID(42) = %q", s)
	}
	id, ok := alertpush.ParseNotifID("alertpush:42")
	if !ok || id != 42 {
		t.Errorf("ParseNotifID = %d, %v; want 42, true", id, ok)
	}
	for _, bad := range []string{"", "alertpush:", "alertpush:x", "alertpush:0", "alertpush:-1", "alarm:42", "42"} {
		if _, ok := alertpush.ParseNotifID(bad); ok {
			t.Errorf("ParseNotifID(%q) ok = true, want false", bad)
		}
	}
}

func TestParseAudience(t *testing.T) {
	cases := map[string]alertpush.Audience{"": alertpush.AudienceAll, "all": alertpush.AudienceAll, "test": alertpush.AudienceTest}
	for in, want := range cases {
		got, err := alertpush.ParseAudience(in)
		if err != nil || got != want {
			t.Errorf("ParseAudience(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := alertpush.ParseAudience("everyone"); err == nil {
		t.Error("ParseAudience(everyone) error = nil, want error")
	}
}

func TestStatusTerminal(t *testing.T) {
	for s, want := range map[alertpush.Status]bool{
		alertpush.StatusQueued: false, alertpush.StatusSending: false,
		alertpush.StatusSent: true, alertpush.StatusFailed: true, alertpush.StatusCanceled: true,
	} {
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}
