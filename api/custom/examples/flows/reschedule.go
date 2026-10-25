// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
)

// reschedule is the flow "Reschedule a reservation" (freebusy.reservation.v1,
// id reschedule): it moves a booking onto a new hold. The original keeps its
// resources until the replacement is confirmed, so a failure part-way leaves
// the old booking in place.
func reschedule(ctx context.Context, e *env) {
	e.Step("Flow reschedule: move a booking onto a new hold")
	room := e.room(ctx, "reschedule")
	hook := e.subscribe(ctx, "reschedule", "/reschedule", freebusy.EventReservationRescheduled)
	defer e.unsubscribe(ctx, hook)
	booked := bookRoom(ctx, e, room, e.at(10), e.at(11))

	// ReservationService.GetReservation -> Reservation (CONFIRMED).
	res, err := e.client.Reservations.Get(ctx, &reservationpb.GetReservationRequest{Name: booked.GetName()})
	e.Must(err, "get reservation")
	e.Expect(res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "GetReservation: %s is CONFIRMED", res.GetName())

	// When the new hold lapsed: RescheduleReservation -> FAILED_PRECONDITION
	// (REPLACEMENT_FAILED), and the original goes back to CONFIRMED.
	lapsed := replacementHold(ctx, e, res, room, e.at(12), e.at(13))
	_, err = e.client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{Name: lapsed.GetName(), Duration: freebusy.NewDuration(time.Second)})
	e.Must(err, "let the new hold lapse")
	time.Sleep(2 * time.Second)
	_, err = e.client.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{Name: res.GetName(), Hold: lapsed.GetName()})
	e.Expect(errors.Is(err, freebusy.ErrReplacementFailed), "RescheduleReservation onto a lapsed hold: %s", problemOf(err))
	res, err = e.client.Reservations.Get(ctx, &reservationpb.GetReservationRequest{Name: res.GetName()})
	e.Expect(err == nil && res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "the original is still %s", res.GetState())

	// HoldService.CreateHold with hold.replaces = the reservation's hold ->
	// Hold (ACTIVE). The original keeps its resources meanwhile.
	next := replacementHold(ctx, e, res, room, e.at(14), e.at(15))

	// When the new hold is still active: RescheduleReservation -> replacement
	// Reservation (CONFIRMED); the original becomes RESCHEDULED.
	replacement, err := e.client.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{
		Name: res.GetName(), Hold: next.GetName(), Etag: res.GetEtag(),
	})
	e.Must(err, "reschedule")
	e.Expect(replacement.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && replacement.GetReplaces() == res.GetName(),
		"RescheduleReservation: replacement %s is CONFIRMED, %s-%s", replacement.GetName(),
		replacement.GetStartTime().AsTime().In(e.day.Location()).Format("15:04"), replacement.GetEndTime().AsTime().In(e.day.Location()).Format("15:04"))
	res, err = e.client.Reservations.Get(ctx, &reservationpb.GetReservationRequest{Name: res.GetName()})
	e.Expect(err == nil && res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_RESCHEDULED && res.GetReplacement() == replacement.GetName(),
		"the original is %s, replaced by %s", res.GetState(), res.GetReplacement())

	// ReservationService emits freebusy.reservation.rescheduled to Webhooks.
	ev := e.hooks.Wait("/reschedule", freebusy.EventReservationRescheduled, res.GetName(), 30*time.Second)
	e.Expect(ev != nil, "webhook: %s for %s", freebusy.EventReservationRescheduled, res.GetName())
}

// bookRoom holds room for [start, end) and confirms the hold, the book flow
// without its checks.
func bookRoom(ctx context.Context, e *env, room string, start, end time.Time) *reservationpb.Reservation {
	hold, err := e.client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold:   &allocationpb.Hold{Window: freebusy.Window(start, end), Quantity: 1, CandidateResources: []string{room}},
	})
	e.Must(err, "hold")
	res, err := e.client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent:      e.site,
		Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/dana-" + e.sfx},
	})
	e.Must(err, "reserve")
	return res
}

// replacementHold holds room for [start, end) as the replacement of res's
// hold.
func replacementHold(ctx context.Context, e *env, res *reservationpb.Reservation, room string, start, end time.Time) *allocationpb.Hold {
	hold, err := e.client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold: &allocationpb.Hold{
			Window: freebusy.Window(start, end), Quantity: 1, CandidateResources: []string{room},
			Replaces: res.GetHold(),
		},
	})
	e.Must(err, "replacement hold")
	e.Expect(hold.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE, "CreateHold (replaces %s): %s is ACTIVE", res.GetHold(), hold.GetName())
	return hold
}
