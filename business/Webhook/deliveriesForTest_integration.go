//go:build integration

package business

import (
	"context"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	"github.com/google/uuid"
)

// ObserveDeliveriesForTest shows the integration test every delivery an
// outgoing webhook is sent, in place of sending it: which webhook, the
// event, and the payload. It exists only in the integration build: the tag
// keeps it out of the shipped binary, so no product code can reach it.
//
// A test can't receive a webhook: the sender refuses every private address
// (helpers.ValidateOutboundURL), and the test's own server is on loopback.
// What a webhook is sent, and which webhooks are sent anything, is decided
// before that, in dispatch, which this leaves as it is: it stands in for the
// queue dispatch hands its deliveries to (business/Hooks).
func ObserveDeliveriesForTest(fn func(webhookID uuid.UUID, eventType string, payload []byte)) (restore func()) {
	was := queueDeliveries
	queueDeliveries = func(_ context.Context, _ uuid.UUID, eventType string, targets []hooksBusiness.Target) error {
		for _, t := range targets {
			fn(t.WebhookID, eventType, t.Payload)
		}
		return nil
	}
	return func() { queueDeliveries = was }
}
