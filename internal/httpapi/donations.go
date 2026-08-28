package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/donations"
)

// maxDonationBody bounds POST /api/v1/payment_intents. The body is five
// short fields; anything larger is not the app.
const maxDonationBody = 4096

// donationsPerMinute is the per-source throttle on donation requests. Not
// in spec section 2.6, but every request creates Stripe objects, and the
// app never sends more than one per tap.
const donationsPerMinute = 10

type donationsHandler struct {
	deps Deps
}

// donationRequest is the wire shape (spec section 11). Amount and test_mode
// are decoded leniently: the iOS app sends the amount as a number and
// test_mode as the string "1"/"0", but a boolean or a numeric string for
// either is accepted rather than turned into a 400 the rider cannot act on.
type donationRequest struct {
	AmountCents json.RawMessage `json:"donation_amount_in_cents"`
	Frequency   string          `json:"donation_frequency"`
	Name        string          `json:"name"`
	Email       string          `json:"email"`
	TestMode    json.RawMessage `json:"test_mode"`
}

// create serves POST /api/v1/payment_intents. Bad input is a 400; a Stripe
// failure is a 500 with an empty body, which is the shipped contract.
func (h *donationsHandler) create(w http.ResponseWriter, r *http.Request) {
	var in donationRequest
	if err := decodeJSON(w, r, maxDonationBody, &in); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	amount, ok := rawInt(in.AmountCents)
	if !ok {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, "donation_amount_in_cents must be an integer")
		return
	}
	req := donations.Request{
		AmountCents: amount,
		Recurring:   strings.EqualFold(strings.TrimSpace(in.Frequency), "recurring"),
		Name:        strings.TrimSpace(in.Name),
		Email:       strings.TrimSpace(in.Email),
		TestMode:    rawTrue(in.TestMode),
	}
	resp, err := h.deps.Donations.Create(r.Context(), req)
	switch {
	case err == nil:
		writeJSON(w, h.deps.Logger, http.StatusOK, resp)
	case errors.Is(err, donations.ErrInvalid):
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
	default:
		// Includes ErrTestModeUnavailable: a test-mode request against a
		// deployment with no test key is a configuration gap, logged as such.
		h.deps.Logger.Error("httpapi: donation", "recurring", req.Recurring, "test_mode", req.TestMode, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// rawInt reads a JSON number or a numeric string as an int64.
func rawInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		n = json.Number(strings.TrimSpace(s))
	}
	v, err := n.Int64()
	return v, err == nil
}

// rawTrue reads test_mode: true, "1", "true", "t", "on" (the strict
// allow-list the apns_sandbox rule in spec section 2.7 uses) mean test.
func rawTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "t", "on":
		return true
	}
	return false
}
