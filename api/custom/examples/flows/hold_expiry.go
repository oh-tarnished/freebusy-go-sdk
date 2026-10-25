// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
)

// holdExpiry is the flow "Hold expiry" (freebusy.allocation.v1, id
// hold-expiry): a hold nobody confirms lapses at expire_time; ExtendHold
// pushes the deadline out while it is still ACTIVE. Reads are authoritative
// on expiry, so a caller never sees an ACTIVE hold that no longer holds
// anything, even before the sweeper runs.
func holdExpiry(ctx context.Context, e *env) {
	e.Step("Flow hold-expiry: a hold nobody confirms lapses")
	room := e.room(ctx, "expiry")
	hook := e.subscribe(ctx, "expiry", "/expiry", freebusy.EventHoldExpired)
	defer e.unsubscribe(ctx, hook)

	// HoldService.CreateHold -> Hold (ACTIVE).
	hold, err := e.client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: e.site,
		Hold:   &allocationpb.Hold{Window: freebusy.Window(e.at(9), e.at(10)), Quantity: 1, CandidateResources: []string{room}},
	})
	e.Must(err, "hold")
	first := hold.GetExpireTime().AsTime()
	e.Expect(hold.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE, "CreateHold: %s is ACTIVE until %s", hold.GetName(), first.Format(time.TimeOnly))

	// When the caller needs more time: HoldService.ExtendHold -> Hold (later
	// expire_time). The new deadline is now + duration.
	hold, err = e.client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{
		Name: hold.GetName(), Duration: freebusy.NewDuration(30 * time.Minute), Etag: hold.GetEtag(),
	})
	e.Must(err, "extend")
	e.Expect(hold.GetExpireTime().AsTime().After(first), "ExtendHold: now ACTIVE until %s", hold.GetExpireTime().AsTime().Format(time.TimeOnly))

	// So the example need not wait out a real hold, extend it by two seconds
	// only: its deadline comes in two seconds, and nobody confirms it.
	hold, err = e.client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{
		Name: hold.GetName(), Duration: freebusy.NewDuration(2 * time.Second), Etag: hold.GetEtag(),
	})
	e.Must(err, "extend by 2s")
	time.Sleep(3 * time.Second)

	// HoldService.GetHold -> Hold (EXPIRED): the read is authoritative, before
	// the sweeper has run.
	hold, err = e.client.Holds.Get(ctx, &allocationpb.GetHoldRequest{Name: hold.GetName()})
	e.Expect(err == nil && hold.GetState() == allocationpb.HoldState_HOLD_STATE_EXPIRED, "GetHold after expire_time: %s", hold.GetState())

	// HoldService.ExtendHold -> FAILED_PRECONDITION (HOLD_NOT_ACTIVE).
	_, err = e.client.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{Name: hold.GetName(), Duration: freebusy.NewDuration(time.Minute)})
	e.Expect(errors.Is(err, freebusy.ErrHoldNotActive), "ExtendHold on the lapsed hold: %s", problemOf(err))

	// The Sweeper expires lapsed holds (once a minute), and HoldService emits
	// freebusy.hold.expired to Webhooks.
	e.Say("waiting for the sweeper's freebusy.hold.expired (up to 90s)")
	ev := e.hooks.Wait("/expiry", freebusy.EventHoldExpired, hold.GetName(), 90*time.Second)
	e.Expect(ev != nil, "webhook: %s for %s", freebusy.EventHoldExpired, hold.GetName())
}
