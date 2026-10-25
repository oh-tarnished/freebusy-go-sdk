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

// book is the flow "Book a resource" (freebusy.reservation.v1, id book):
// search, check, hold, then confirm. Only the hold secures capacity: search
// and check are advisory and can be stale the instant they answer.
func book(ctx context.Context, e *env) {
	e.Step("Flow book: search, check, hold, confirm")
	room := e.room(ctx, "book")
	hook := e.subscribe(ctx, "book", "/book", freebusy.EventReservationConfirmed)
	defer e.unsubscribe(ctx, hook)
	window := freebusy.Window(e.at(10), e.at(11))

	// AllocationService.SearchAvailability: which rooms are free 10:00-11:00.
	var free []string
	for m, err := range e.client.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
		Parent:        e.org,
		Window:        window,
		ResourceTypes: []string{"meeting-room"},
		Quantity:      1,
	}).All() {
		if !e.NoErr(err, "search") {
			return
		}
		if m.GetResource() == room {
			free = append(free, room)
		}
	}
	e.Expect(len(free) == 1, "SearchAvailability finds the room free 10:00-11:00")

	// AllocationService.CheckAvailability: advisory only.
	check, err := e.client.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{
		Parent: e.site, Resources: []string{room}, Window: window, Quantity: 1,
	})
	e.Must(err, "check")
	e.Expect(check.GetBookable() && check.GetFreeCount() == 1, "CheckAvailability: bookable, %d free", check.GetFreeCount())

	// When there is enough capacity: HoldService.CreateHold -> Hold (ACTIVE).
	// With an idempotency_key, a retry after a lost answer returns the hold
	// the first call took instead of taking a second one.
	create := &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold:   &allocationpb.Hold{Window: window, Quantity: 1, CandidateResources: free, IdempotencyKey: "book-" + e.sfx},
	}
	hold, err := e.client.Holds.New(ctx, create)
	e.Must(err, "hold")
	e.Expect(hold.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE, "CreateHold: %s is ACTIVE until %s",
		hold.GetName(), hold.GetExpireTime().AsTime().Format(time.Kitchen))
	again, err := e.client.Holds.New(ctx, create)
	e.Expect(err == nil && again.GetName() == hold.GetName(), "CreateHold again with the same idempotency_key returns the same hold")

	// While the hold is ACTIVE: ReservationService.CreateReservation ->
	// Reservation (CONFIRMED), and the hold becomes CONVERTED.
	res, err := e.client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent:      e.site,
		Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/dana-" + e.sfx},
	})
	e.Must(err, "reserve")
	e.Expect(res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "CreateReservation: %s is CONFIRMED", res.GetName())
	hold, err = e.client.Holds.Get(ctx, &allocationpb.GetHoldRequest{Name: hold.GetName()})
	e.Expect(err == nil && hold.GetState() == allocationpb.HoldState_HOLD_STATE_CONVERTED, "the hold is now %s", hold.GetState())

	// ReservationService emits freebusy.reservation.confirmed to Webhooks.
	ev := e.hooks.Wait("/book", freebusy.EventReservationConfirmed, res.GetName(), 30*time.Second)
	e.Expect(ev != nil, "webhook: %s for %s", freebusy.EventReservationConfirmed, res.GetName())

	// When capacity is taken: CreateHold -> FAILED_PRECONDITION (CAPACITY_EXHAUSTED).
	_, err = e.client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold:   &allocationpb.Hold{Window: window, Quantity: 1, CandidateResources: []string{room}},
	})
	e.Expect(errors.Is(err, freebusy.ErrCapacityExhausted), "CreateHold on the booked room: %s", problemOf(err))

	// When the hold expired or was released: CreateReservation ->
	// FAILED_PRECONDITION (HOLD_NOT_ACTIVE).
	later, err := e.client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold:   &allocationpb.Hold{Window: freebusy.Window(e.at(14), e.at(15)), Quantity: 1, CandidateResources: []string{room}},
	})
	e.Must(err, "hold 14:00")
	_, err = e.client.Holds.Release(ctx, &allocationpb.ReleaseHoldRequest{Name: later.GetName(), Etag: later.GetEtag()})
	e.Must(err, "release")
	_, err = e.client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent:      e.site,
		Reservation: &reservationpb.Reservation{Hold: later.GetName(), Customer: "users/dana-" + e.sfx},
	})
	e.Expect(errors.Is(err, freebusy.ErrHoldNotActive), "CreateReservation on a released hold: %s", problemOf(err))
}
