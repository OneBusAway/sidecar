package donations

import (
	"context"
	"fmt"

	stripe "github.com/stripe/stripe-go/v83"
)

// ephemeralKeyStripeVersion is the API version the shipped iOS PaymentSheet
// was built against, carried over from the previous implementation. An
// ephemeral key is only valid for the version it was minted for.
const ephemeralKeyStripeVersion = "2023-08-16"

// StripeGateway is the Gateway backed by one Stripe secret key. Live and
// test keys are two instances.
type StripeGateway struct {
	client *stripe.Client
	// productID is the pre-existing Stripe product recurring donations
	// attach to; a fresh monthly price is created per donation because the
	// amount is rider-chosen.
	productID string
}

// NewStripeGateway builds a gateway for a secret key and the id of the
// product recurring donations bill against.
func NewStripeGateway(secretKey, recurringProductID string) *StripeGateway {
	return &StripeGateway{client: stripe.NewClient(secretKey), productID: recurringProductID}
}

// FindOrCreateCustomer implements Gateway.
func (g *StripeGateway) FindOrCreateCustomer(ctx context.Context, email, name string) (string, error) {
	list := &stripe.CustomerListParams{Email: stripe.String(email)}
	list.Limit = stripe.Int64(1)
	for c, err := range g.client.V1Customers.List(ctx, list) {
		if err != nil {
			return "", err
		}
		return c.ID, nil
	}
	c, err := g.client.V1Customers.Create(ctx, &stripe.CustomerCreateParams{
		Email: stripe.String(email), Name: stripe.String(name),
	})
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// CreatePaymentIntent implements Gateway.
func (g *StripeGateway) CreatePaymentIntent(ctx context.Context, customerID string, amountCents int64, receiptEmail string) (string, error) {
	pi, err := g.client.V1PaymentIntents.Create(ctx, &stripe.PaymentIntentCreateParams{
		Customer:                stripe.String(customerID),
		Amount:                  stripe.Int64(amountCents),
		Currency:                stripe.String("usd"),
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)},
		ReceiptEmail:            stripe.String(receiptEmail),
	})
	if err != nil {
		return "", err
	}
	return pi.ClientSecret, nil
}

// CreateEphemeralKey implements Gateway.
func (g *StripeGateway) CreateEphemeralKey(ctx context.Context, customerID string) (string, error) {
	params := &stripe.EphemeralKeyCreateParams{Customer: stripe.String(customerID)}
	params.StripeVersion = stripe.String(ephemeralKeyStripeVersion)
	key, err := g.client.V1EphemeralKeys.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return key.Secret, nil
}

// CreateSubscription implements Gateway. The subscription is created
// incomplete so the PaymentSheet collects payment for the first invoice;
// its client secret comes back via the invoice's confirmation secret,
// which is where current Stripe API versions expose it.
func (g *StripeGateway) CreateSubscription(ctx context.Context, customerID string, amountCents int64) (string, error) {
	price, err := g.client.V1Prices.Create(ctx, &stripe.PriceCreateParams{
		UnitAmount: stripe.Int64(amountCents),
		Currency:   stripe.String("usd"),
		Recurring:  &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
		Product:    stripe.String(g.productID),
	})
	if err != nil {
		return "", err
	}
	sub, err := g.client.V1Subscriptions.Create(ctx, &stripe.SubscriptionCreateParams{
		Customer:        stripe.String(customerID),
		Items:           []*stripe.SubscriptionCreateItemParams{{Price: stripe.String(price.ID)}},
		PaymentBehavior: stripe.String("default_incomplete"),
		Expand:          []*string{stripe.String("latest_invoice.confirmation_secret")},
	})
	if err != nil {
		return "", err
	}
	if sub.LatestInvoice == nil || sub.LatestInvoice.ConfirmationSecret == nil || sub.LatestInvoice.ConfirmationSecret.ClientSecret == "" {
		return "", fmt.Errorf("stripe: subscription %s has no confirmation secret on its first invoice", sub.ID)
	}
	return sub.LatestInvoice.ConfirmationSecret.ClientSecret, nil
}
