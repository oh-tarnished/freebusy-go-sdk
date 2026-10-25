# Flows

The flows the freebusy API documents (its `docs/flows/*.yaml`), each
written out with this SDK, one file per flow. Every step in a file is
commented with the flow step it is.

| Flow | File | What it shows |
| --- | --- | --- |
| `book` | [`book.go`](book.go) | search, check, hold, confirm; capacity taken; a released hold |
| `hold-expiry` | [`hold_expiry.go`](hold_expiry.go) | extend a hold; let it lapse; the sweeper's event |
| `reschedule` | [`reschedule.go`](reschedule.go) | move a booking onto a new hold; a lapsed replacement |
| `cancel` | [`cancel.go`](cancel.go) | cancel, the room is free again, cancel again |
| `webhook-delivery` | [`webhook_delivery.go`](webhook_delivery.go) | a delivery that failed, retried by hand |

Run one flow by its id, or all of them, against a freebusy server:

```sh
FREEBUSY_ENDPOINT=localhost:50051 FREEBUSY_INSECURE=true go run ./examples/flows book
FREEBUSY_ENDPOINT=localhost:50051 FREEBUSY_INSECURE=true go run ./examples/flows
```

`hold-expiry` waits for the server's hold sweeper (once a minute).
`webhook-delivery` is quickest against a server with
`[webhooks] max_attempts = 1` (`FREEBUSY_WEBHOOKS__MAX_ATTEMPTS=1`), where one
failed attempt makes a delivery FAILED.

## Book a resource

Search, check, hold, then confirm. Only the hold secures capacity: search and
check are advisory. From [`book.go`](book.go):

```go
// AllocationService.SearchAvailability
for m, err := range client.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
	Parent: org, Window: freebusy.Window(start, end), ResourceTypes: []string{"meeting-room"}, Quantity: 1,
}).All() { … }

// AllocationService.CheckAvailability: advisory only
check, err := client.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{
	Parent: site, Resources: []string{room}, Window: freebusy.Window(start, end), Quantity: 1,
})

// HoldService.CreateHold -> Hold (ACTIVE); a retry with the same
// idempotency_key returns the same hold
hold, err := client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
	Parent: site,
	Hold:   &allocationpb.Hold{Window: freebusy.Window(start, end), Quantity: 1, CandidateResources: free, IdempotencyKey: key},
})

// ReservationService.CreateReservation -> Reservation (CONFIRMED); the hold becomes CONVERTED
res, err := client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
	Parent: site, Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/dana"},
})

// capacity taken / hold no longer active
if errors.Is(err, freebusy.ErrCapacityExhausted) { … }
if errors.Is(err, freebusy.ErrHoldNotActive) { … }
```

## Hold expiry

A hold nobody confirms lapses at `expire_time`; `ExtendHold` pushes the
deadline out while it is still ACTIVE. Reads are authoritative on expiry. From
[`hold_expiry.go`](hold_expiry.go):

```go
// HoldService.ExtendHold -> Hold (later expire_time): now + duration
hold, err = client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{
	Name: hold.GetName(), Duration: freebusy.NewDuration(30 * time.Minute), Etag: hold.GetEtag(),
})

// after expire_time: HoldService.GetHold -> Hold (EXPIRED), even before the sweeper ran
hold, err = client.Holds.Get(ctx, &allocationpb.GetHoldRequest{Name: hold.GetName()})

// HoldService.ExtendHold on it -> FAILED_PRECONDITION (HOLD_NOT_ACTIVE)
_, err = client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{Name: hold.GetName(), Duration: freebusy.NewDuration(time.Minute)})
errors.Is(err, freebusy.ErrHoldNotActive) // true

// the sweeper's freebusy.hold.expired arrives at your webhook
```

## Reschedule a reservation

Moves a booking onto a new hold. The original keeps its resources until the
replacement is confirmed. From [`reschedule.go`](reschedule.go):

```go
// HoldService.CreateHold with hold.replaces = the reservation's hold
next, err := client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
	Parent: site,
	Hold: &allocationpb.Hold{Window: freebusy.Window(newStart, newEnd), Quantity: 1,
		CandidateResources: []string{room}, Replaces: res.GetHold()},
})

// ReservationService.RescheduleReservation -> replacement Reservation (CONFIRMED);
// the original becomes RESCHEDULED
replacement, err := client.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{
	Name: res.GetName(), Hold: next.GetName(), Etag: res.GetEtag(),
})

// the new hold lapsed: FAILED_PRECONDITION (REPLACEMENT_FAILED), the original stays CONFIRMED
errors.Is(err, freebusy.ErrReplacementFailed)
```

## Cancel a reservation

Cancelling frees the resources; repeating it changes nothing. From
[`cancel.go`](cancel.go):

```go
// ReservationService.CancelReservation: CONFIRMED -> CANCELLING -> CANCELLED
res, err := client.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{
	Name: res.GetName(), Reason: "customer asked", Etag: res.GetEtag(),
})

// again: idempotent, still CANCELLED, and no second freebusy.reservation.cancelled
res, err = client.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{Name: res.GetName()})
```

## Webhook delivery

Every change a webhook subscribes to is a Delivery, sent as a signed
CloudEvent. Failed deliveries are retried with backoff, and can be sent again
by hand. From [`webhook_delivery.go`](webhook_delivery.go):

```go
// WebhookService.CreateWebhook -> Webhook (ACTIVE), with its signing secret
hook, err := client.Webhooks.New(ctx, &webhookpb.CreateWebhookRequest{
	WebhookId: "bookings",
	Webhook: &webhookpb.Webhook{Uri: "https://example.com/hooks",
		EventTypes: []string{freebusy.EventReservationConfirmed}, OrganizationalUnit: site},
})

// your endpoint: verify the signature and decode the event
ev, err := freebusy.ParseWebhook(r, hook.GetSecret())
res, err := ev.Reservation()

// WebhookService.ListDeliveries, filter state = FAILED
for d, err := range client.Webhooks.Deliveries.ListAutoPaging(ctx, &webhookpb.ListDeliveriesRequest{
	Parent: hook.GetName(), Filter: `state = FAILED`,
}).All() { … }

// WebhookService.RetryDelivery -> Delivery (PENDING), then SUCCEEDED once your endpoint answers 2xx
d, err := client.Webhooks.Deliveries.Retry(ctx, &webhookpb.RetryDeliveryRequest{Name: failed.GetName()})
```
