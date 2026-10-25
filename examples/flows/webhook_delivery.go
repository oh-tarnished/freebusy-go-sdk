// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/webhookpb"
)

// webhookDelivery is the flow "Webhook delivery" (freebusy.webhook.v1, id
// webhook-delivery): every change a webhook subscribes to is recorded as a
// Delivery and sent as a signed CloudEvent (Standard Webhooks). Failed
// deliveries are retried with backoff, and can be sent again by hand.
func webhookDelivery(ctx context.Context, e *env) {
	e.Step("Flow webhook-delivery: a failed delivery, retried by hand")
	room := e.room(ctx, "delivery")

	// WebhookService.CreateWebhook -> Webhook (ACTIVE). Its endpoint is down
	// for now: it answers 503.
	e.hooks.Fail("/delivery", true)
	hook := e.subscribe(ctx, "delivery", "/delivery", freebusy.EventReservationConfirmed)
	defer e.unsubscribe(ctx, hook)
	e.Expect(hook.GetState() == webhookpb.WebhookState_WEBHOOK_STATE_ACTIVE, "CreateWebhook: %s is ACTIVE", hook.GetName())

	// ReservationService records a change (a reservation confirmed): one
	// Delivery per matching webhook, PENDING.
	res := bookRoom(ctx, e, room, e.at(8), e.at(9))

	// The Dispatcher POSTs the signed CloudEvent; the endpoint answers 5xx,
	// so the delivery is retried with backoff and FAILED when no attempts are
	// left. WebhookService.ListDeliveries with filter state = FAILED finds it.
	e.Say("waiting for the delivery to fail (up to 60s)")
	var failed *webhookpb.Delivery
	deadline := time.Now().Add(60 * time.Second)
	for failed == nil && time.Now().Before(deadline) {
		for d, err := range e.client.Webhooks.Deliveries.ListAutoPaging(ctx, &webhookpb.ListDeliveriesRequest{
			Parent: hook.GetName(), Filter: `state = FAILED`,
		}).All() {
			if !e.NoErr(err, "list failed deliveries") {
				return
			}
			if d.GetSubject() == res.GetName() {
				failed = d
			}
		}
		if failed == nil {
			time.Sleep(time.Second)
		}
	}
	if failed == nil {
		// A server with more than one attempt is still retrying it: take the
		// delivery as it is, and send it again by hand all the same.
		for d, err := range e.client.Webhooks.Deliveries.ListAutoPaging(ctx, &webhookpb.ListDeliveriesRequest{Parent: hook.GetName()}).All() {
			if err == nil && d.GetSubject() == res.GetName() {
				failed = d
			}
		}
		if failed == nil {
			e.Expect(false, "a delivery of %s to %s", res.GetName(), hook.GetName())
			return
		}
		e.Note("delivery %s is %s after %d attempt(s), not FAILED: run the server with [webhooks] max_attempts = 1 to see it fail outright",
			failed.GetName(), failed.GetState(), failed.GetAttemptCount())
	} else {
		e.Expect(true, "ListDeliveries (state = FAILED): %s, %d attempt(s), last answer %d", failed.GetName(), failed.GetAttemptCount(), failed.GetResponseCode())
	}

	// The endpoint is back. WebhookService.RetryDelivery -> Delivery (PENDING).
	e.hooks.Fail("/delivery", false)
	retried, err := e.client.Webhooks.Deliveries.Retry(ctx, &webhookpb.RetryDeliveryRequest{Name: failed.GetName()})
	e.Must(err, "retry delivery")
	e.Expect(retried.GetState() == webhookpb.DeliveryState_DELIVERY_STATE_PENDING, "RetryDelivery: %s is PENDING again", retried.GetName())

	// The endpoint answers 2xx: the Delivery is SUCCEEDED.
	ev := e.hooks.Wait("/delivery", freebusy.EventReservationConfirmed, res.GetName(), 30*time.Second)
	e.Expect(ev != nil, "the endpoint receives %s for %s", freebusy.EventReservationConfirmed, res.GetName())
	var d *webhookpb.Delivery
	for range 20 {
		d, err = e.client.Webhooks.Deliveries.Get(ctx, &webhookpb.GetDeliveryRequest{Name: failed.GetName()})
		if err != nil || d.GetState() == webhookpb.DeliveryState_DELIVERY_STATE_SUCCEEDED {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.Expect(err == nil && d.GetState() == webhookpb.DeliveryState_DELIVERY_STATE_SUCCEEDED, "GetDelivery: %s", d.GetState())
}
