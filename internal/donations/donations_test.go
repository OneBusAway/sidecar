package donations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/OneBusAway/sidecar/internal/donations"
)

type fake struct {
	name  string
	calls []string
	fail  string // method name that returns an error
}

func (f *fake) rec(m string) error {
	f.calls = append(f.calls, m)
	if f.fail == m {
		return errors.New(m + " failed")
	}
	return nil
}
func (f *fake) FindOrCreateCustomer(_ context.Context, email, _ string) (string, error) {
	return "cus_" + f.name + "_" + email, f.rec("customer")
}
func (f *fake) CreatePaymentIntent(_ context.Context, cus string, amount int64, _ string) (string, error) {
	return "pi_secret_" + cus, f.rec("intent")
}
func (f *fake) CreateEphemeralKey(_ context.Context, cus string) (string, error) {
	return "ek_" + cus, f.rec("ephemeral")
}
func (f *fake) CreateSubscription(_ context.Context, cus string, amount int64) (string, error) {
	return "sub_secret_" + cus, f.rec("subscription")
}

func newService(live, test *fake) *donations.Service {
	s := &donations.Service{Live: live, NewID: func() string { return "req-1" }}
	if test != nil {
		s.Test = test
	}
	return s
}

func TestOneTime(t *testing.T) {
	live := &fake{name: "live"}
	resp, err := newService(live, nil).Create(context.Background(), donations.Request{
		AmountCents: 500, Email: "jane@example.com", Name: "Jane",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := donations.Response{ClientSecret: "pi_secret_cus_live_jane@example.com", ID: "req-1"}
	if resp != want {
		t.Fatalf("resp %+v, want %+v", resp, want)
	}
	if got := len(live.calls); got != 2 || live.calls[1] != "intent" {
		t.Fatalf("calls %v", live.calls)
	}
}

func TestRecurring(t *testing.T) {
	live := &fake{name: "live"}
	resp, err := newService(live, nil).Create(context.Background(), donations.Request{
		AmountCents: 1000, Recurring: true, Email: "jane@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := donations.Response{
		ClientSecret: "sub_secret_cus_live_jane@example.com",
		CustomerID:   "cus_live_jane@example.com",
		EphemeralKey: "ek_cus_live_jane@example.com",
		ID:           "req-1",
	}
	if resp != want {
		t.Fatalf("resp %+v, want %+v", resp, want)
	}
}

func TestTestModeRouting(t *testing.T) {
	live, test := &fake{name: "live"}, &fake{name: "test"}
	resp, err := newService(live, test).Create(context.Background(), donations.Request{
		AmountCents: 500, Email: "x@example.com", TestMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ClientSecret != "pi_secret_cus_test_x@example.com" || len(live.calls) != 0 {
		t.Fatalf("test mode reached the live gateway: %+v live=%v", resp, live.calls)
	}
	_, err = newService(live, nil).Create(context.Background(), donations.Request{
		AmountCents: 500, Email: "x@example.com", TestMode: true,
	})
	if !errors.Is(err, donations.ErrTestModeUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestValidationAndGatewayErrors(t *testing.T) {
	for _, req := range []donations.Request{
		{AmountCents: 0, Email: "x@example.com"},
		{AmountCents: -5, Email: "x@example.com"},
		{AmountCents: 5, Email: ""},
		{AmountCents: 5, Email: "not-an-email"},
	} {
		if _, err := newService(&fake{}, nil).Create(context.Background(), req); !errors.Is(err, donations.ErrInvalid) {
			t.Errorf("%+v: err %v, want ErrInvalid", req, err)
		}
	}
	for _, step := range []string{"customer", "ephemeral", "subscription"} {
		live := &fake{name: "live", fail: step}
		_, err := newService(live, nil).Create(context.Background(), donations.Request{
			AmountCents: 5, Email: "x@example.com", Recurring: true,
		})
		if err == nil || errors.Is(err, donations.ErrInvalid) {
			t.Errorf("failing %s: err %v", step, err)
		}
	}
}
