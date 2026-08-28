package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/donations"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

type donationGateway struct {
	last donations.Request // reconstructed from the calls
	fail bool
}

func (g *donationGateway) FindOrCreateCustomer(_ context.Context, email, name string) (string, error) {
	g.last.Email, g.last.Name = email, name
	if g.fail {
		return "", errors.New("stripe down")
	}
	return "cus_1", nil
}
func (g *donationGateway) CreatePaymentIntent(_ context.Context, _ string, amount int64, _ string) (string, error) {
	g.last.AmountCents = amount
	return "pi_secret", nil
}
func (g *donationGateway) CreateEphemeralKey(context.Context, string) (string, error) {
	return "ek_secret", nil
}
func (g *donationGateway) CreateSubscription(_ context.Context, _ string, amount int64) (string, error) {
	g.last.AmountCents, g.last.Recurring = amount, true
	return "sub_secret", nil
}

func newDonationsServer(t *testing.T, live, test *donationGateway) http.Handler {
	t.Helper()
	store := sqlitetest.Open(t)
	svc := &donations.Service{Live: live, NewID: func() string { return "11111111-2222-3333-4444-555555555555" }}
	if test != nil {
		svc.Test = test
	}
	return httpapi.NewRouter(httpapi.Deps{
		Alerts:          store.Alerts(),
		Regions:         store.Regions(),
		Donations:       svc,
		DonationLimiter: ratelimit.New(2, time.Minute),
		Now:             func() time.Time { return base },
		Logger:          slog.New(slog.DiscardHandler),
	})
}

func postDonation(h http.Handler, body, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/payment_intents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The exact bodies the iOS app sends: amount as a number, test_mode as a
// string, frequency "onetime" or "recurring".
const iosOneTime = `{"donation_amount_in_cents":500,"donation_frequency":"onetime","name":"Jane Rider","email":"jane@example.com","test_mode":"0"}`
const iosRecurring = `{"donation_amount_in_cents":1000,"donation_frequency":"recurring","name":"Jane Rider","email":"jane@example.com","test_mode":"1"}`

func TestDonations_OneTime(t *testing.T) {
	t.Parallel()
	live := &donationGateway{}
	rec := postDonation(newDonationsServer(t, live, nil), iosOneTime, "203.0.113.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["client_secret"] != "pi_secret" || got["id"] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("body %v", got)
	}
	if _, has := got["customer_id"]; has {
		t.Fatalf("one-time response carries customer_id: %v", got)
	}
	if live.last.AmountCents != 500 || live.last.Email != "jane@example.com" || live.last.Name != "Jane Rider" {
		t.Fatalf("gateway saw %+v", live.last)
	}
}

func TestDonations_RecurringRoutesToTestKey(t *testing.T) {
	t.Parallel()
	live, test := &donationGateway{}, &donationGateway{}
	rec := postDonation(newDonationsServer(t, live, test), iosRecurring, "203.0.113.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["client_secret"] != "sub_secret" || got["customer_id"] != "cus_1" || got["ephemeral_key"] != "ek_secret" {
		t.Fatalf("body %v", got)
	}
	if live.last.Email != "" || !test.last.Recurring || test.last.AmountCents != 1000 {
		t.Fatalf("live=%+v test=%+v", live.last, test.last)
	}
}

func TestDonations_Errors(t *testing.T) {
	t.Parallel()
	h := newDonationsServer(t, &donationGateway{fail: true}, nil)
	if rec := postDonation(h, "{not json", "203.0.113.1"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: %d", rec.Code)
	}
	if rec := postDonation(h, `{"donation_amount_in_cents":"lots","email":"a@b"}`, "203.0.113.2"); rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer amount: %d", rec.Code)
	}
	if rec := postDonation(h, `{"donation_amount_in_cents":0,"email":"a@b"}`, "203.0.113.3"); rec.Code != http.StatusBadRequest {
		t.Errorf("zero amount: %d", rec.Code)
	}
	// Stripe failure: 500, empty body (spec section 11).
	rec := postDonation(h, iosOneTime, "203.0.113.4")
	if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Errorf("gateway failure: %d %q", rec.Code, rec.Body.String())
	}
	// test_mode with no test gateway is also a 500, not a silent live charge.
	rec = postDonation(h, iosRecurring, "203.0.113.5")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("test mode without test key: %d", rec.Code)
	}
}

func TestDonations_ThrottleAndAbsence(t *testing.T) {
	t.Parallel()
	h := newDonationsServer(t, &donationGateway{}, nil)
	postDonation(h, iosOneTime, "203.0.113.9")
	postDonation(h, iosOneTime, "203.0.113.9")
	if rec := postDonation(h, iosOneTime, "203.0.113.9"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("third request: %d, want 429", rec.Code)
	}
	store := sqlitetest.Open(t)
	off := httpapi.NewRouter(httpapi.Deps{Alerts: store.Alerts(), Regions: store.Regions(), Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler)})
	if rec := postDonation(off, iosOneTime, "203.0.113.9"); rec.Code != http.StatusNotFound {
		t.Errorf("unconfigured deployment: %d, want 404", rec.Code)
	}
}

// TestDonations_TestModeSpellings pins which test_mode encodings route to
// the test key: a wrong answer here charges a real card during testing.
func TestDonations_TestModeSpellings(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		`"1"`: true, `"true"`: true, `"T"`: true, `"on"`: true, `true`: true,
		`"0"`: false, `"false"`: false, `false`: false, `"yes"`: false, `null`: false, ``: false,
	}
	for raw, wantTest := range cases {
		live, test := &donationGateway{}, &donationGateway{}
		h := newDonationsServer(t, live, test)
		body := `{"donation_amount_in_cents":"500","donation_frequency":"onetime","name":"x","email":"x@example.com"`
		if raw != `` {
			body += `,"test_mode":` + raw
		}
		body += `}`
		rec := postDonation(h, body, "203.0.113.7")
		if rec.Code != http.StatusOK {
			t.Errorf("test_mode=%s: status %d %s", raw, rec.Code, rec.Body.String())
			continue
		}
		if gotTest := test.last.Email != ""; gotTest != wantTest {
			t.Errorf("test_mode=%s routed to test=%v, want %v", raw, gotTest, wantTest)
		}
	}
	// A fractional amount is rejected.
	rec := postDonation(newDonationsServer(t, &donationGateway{}, nil), `{"donation_amount_in_cents":500.5,"email":"x@example.com"}`, "203.0.113.8")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("fractional amount: %d", rec.Code)
	}
}
