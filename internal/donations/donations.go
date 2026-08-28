// Package donations implements spec section 11: in-app donations through
// Stripe's PaymentSheet. The Stripe calls sit behind Gateway so the request
// handling and the live/test routing are testable without a network, and
// so the shape of what the app receives is pinned by tests rather than by
// whatever Stripe happens to return.
package donations

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Request is the app's POST body (spec section 11): raw JSON, not form.
type Request struct {
	AmountCents int64
	Recurring   bool
	Name        string
	Email       string
	TestMode    bool
}

// Response is what the app decodes (PaymentIntentResponse in the iOS
// client). CustomerID and EphemeralKey are present only for recurring
// donations, where the PaymentSheet manages the customer's payment methods.
type Response struct {
	ClientSecret string `json:"client_secret"`
	CustomerID   string `json:"customer_id,omitempty"`
	EphemeralKey string `json:"ephemeral_key,omitempty"`
	ID           string `json:"id"`
}

// Gateway is the slice of Stripe a donation needs.
type Gateway interface {
	// FindOrCreateCustomer returns the id of the customer with this email,
	// creating one when none exists.
	FindOrCreateCustomer(ctx context.Context, email, name string) (string, error)
	// CreatePaymentIntent returns the client secret of a one-time intent
	// for the customer.
	CreatePaymentIntent(ctx context.Context, customerID string, amountCents int64, receiptEmail string) (string, error)
	// CreateEphemeralKey returns an ephemeral key secret the PaymentSheet
	// uses to act on the customer.
	CreateEphemeralKey(ctx context.Context, customerID string) (string, error)
	// CreateSubscription creates a monthly subscription at amountCents and
	// returns the client secret of its first invoice's payment.
	CreateSubscription(ctx context.Context, customerID string, amountCents int64) (string, error)
}

// ErrTestModeUnavailable is returned for test_mode requests when no test
// gateway is configured.
var ErrTestModeUnavailable = errors.New("donations: test mode requested but no test gateway is configured")

// ErrInvalid marks a request the app should not have sent (400), as opposed
// to a Stripe failure (500 per spec section 11).
var ErrInvalid = errors.New("donations: invalid request")

// Service routes each request to the live or test gateway and runs the
// spec's two flows. NewID supplies the per-request correlation id.
type Service struct {
	Live  Gateway
	Test  Gateway // nil means test_mode requests fail with ErrTestModeUnavailable
	NewID func() string
}

// Create runs one donation request.
func (s *Service) Create(ctx context.Context, req Request) (Response, error) {
	if err := validate(req); err != nil {
		return Response{}, err
	}
	gw := s.Live
	if req.TestMode {
		gw = s.Test
		if gw == nil {
			return Response{}, ErrTestModeUnavailable
		}
	}
	customerID, err := gw.FindOrCreateCustomer(ctx, req.Email, req.Name)
	if err != nil {
		return Response{}, fmt.Errorf("donations: customer: %w", err)
	}
	resp := Response{ID: s.NewID()}
	if !req.Recurring {
		resp.ClientSecret, err = gw.CreatePaymentIntent(ctx, customerID, req.AmountCents, req.Email)
		if err != nil {
			return Response{}, fmt.Errorf("donations: payment intent: %w", err)
		}
		return resp, nil
	}
	resp.CustomerID = customerID
	resp.EphemeralKey, err = gw.CreateEphemeralKey(ctx, customerID)
	if err != nil {
		return Response{}, fmt.Errorf("donations: ephemeral key: %w", err)
	}
	resp.ClientSecret, err = gw.CreateSubscription(ctx, customerID, req.AmountCents)
	if err != nil {
		return Response{}, fmt.Errorf("donations: subscription: %w", err)
	}
	return resp, nil
}

func validate(req Request) error {
	switch {
	case req.AmountCents <= 0:
		return fmt.Errorf("%w: donation_amount_in_cents must be positive", ErrInvalid)
	case strings.TrimSpace(req.Email) == "" || !strings.Contains(req.Email, "@"):
		return fmt.Errorf("%w: email is required", ErrInvalid)
	}
	return nil
}
