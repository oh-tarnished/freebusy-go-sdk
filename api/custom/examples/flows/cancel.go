// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
)

// cancel is the flow "Cancel a reservation" (freebusy.reservation.v1, id
// cancel): cancelling frees the resources; repeating it changes nothing.
func cancel(ctx context.Context, e *env) {
	e.Step("Flow cancel: cancel, and cancel again")
	room := e.room(ctx, "cancel")
	hook := e.subscribe(ctx, "cancel", "/cancel", freebusy.EventReservationCancelled)
	defer e.unsubscribe(ctx, hook)
	res := bookRoom(ctx, e, room, e.at(16), e.at(17))

	// ReservationService.CancelReservation: CONFIRMED -> CANCELLING ->
	// CANCELLED.
	res, err := e.client.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{
		Name: res.GetName(), Reason: "customer asked", Etag: res.GetEtag(),
	})
	e.Must(err, "cancel")
	e.Expect(res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED, "CancelReservation: %s is CANCELLED (%q)", res.GetName(), res.GetCancelReason())

	// The resources are free again.
	check, err := e.client.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{
		Parent: e.site, Resources: []string{room}, Window: freebusy.Window(e.at(16), e.at(17)), Quantity: 1,
	})
	e.Expect(err == nil && check.GetBookable(), "CheckAvailability: the room is bookable again")

	// ReservationService emits freebusy.reservation.cancelled to Webhooks.
	ev := e.hooks.Wait("/cancel", freebusy.EventReservationCancelled, res.GetName(), 30*time.Second)
	e.Expect(ev != nil, "webhook: %s for %s", freebusy.EventReservationCancelled, res.GetName())

	// ReservationService.CancelReservation again: idempotent -> Reservation
	// (CANCELLED), and no second event.
	again, err := e.client.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{Name: res.GetName()})
	e.Expect(err == nil && again.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED, "CancelReservation again: still CANCELLED")
	time.Sleep(8 * time.Second) // past a dispatcher round
	n := len(e.hooks.Find("/cancel", freebusy.EventReservationCancelled, res.GetName()))
	e.Expect(n == 1, "exactly one %s delivered (%d)", freebusy.EventReservationCancelled, n)
}
