// Copyright 2026 Shokki Engineering
// SPDX-License-Identifier: Apache-2.0

// Command hotel acts out a hotel group running its Lisbon property on freebusy,
// through the Go SDK, against a running server: rooms, a nightly calendar, guests searching,
// holding, booking, changing, moving and cancelling stays, two guests racing
// for the last suite, a checkout that is abandoned, and the week's occupancy.
//
// It exits non-zero if any expectation fails.
package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
)

const zoneName = "Europe/Lisbon"

// hotel is the run's state: the client, the site, and the calendar of nights.
type hotel struct {
	e      *freebusy.Client
	c      *checker
	sfx    string
	org    string
	site   string
	base   time.Time // midnight of night 0, Lisbon time
	rooms  map[string]room
	double []string
	suites []string
}

type room struct {
	Type string `json:"type"`
	Beds string `json:"beds"`
	View string `json:"view"`
}

// checkIn is 15:00 on the day of night n; checkOut 11:00 on day n.
func (h *hotel) checkIn(n int) time.Time {
	return h.base.AddDate(0, 0, n).Add(15 * time.Hour)
}
func (h *hotel) checkOut(n int) time.Time {
	return h.base.AddDate(0, 0, n).Add(11 * time.Hour)
}

// stay is the window of a stay arriving on night n for nights nights.
func (h *hotel) stay(n, nights int) (time.Time, time.Time) {
	return h.checkIn(n), h.checkOut(n + nights)
}

func (h *hotel) describe(n, nights int) string {
	a, b := h.stay(n, nights)
	return fmt.Sprintf("%s 15:00 -> %s 11:00 (%d night(s))", a.Format("Mon 02 Jan"), b.Format("Mon 02 Jan"), nights)
}

func main() {
	ctx := context.Background()
	c := &checker{}
	e, err := connect(ctx)
	c.Must(err, "connect to freebusy")
	defer func() { _ = e.Close() }()

	loc, err := time.LoadLocation(zoneName)
	c.Must(err, "load zone")
	// Night 0 is a Monday at least three weeks out, so no booking rule about
	// notice gets in the way and the week is the same on every run.
	day := time.Now().In(loc).AddDate(0, 0, 21)
	for day.Weekday() != time.Monday {
		day = day.AddDate(0, 0, 1)
	}
	h := &hotel{e: e, c: c, sfx: suffix(), rooms: map[string]room{},
		base: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)}

	h.setup(ctx)
	h.firstGuest(ctx)
	h.updateBooking(ctx)
	h.reschedule(ctx)
	h.cancel(ctx)
	h.raceForLastSuite(ctx)
	h.abandonedCheckout(ctx)
	h.weekAtAGlance(ctx)
	c.Finish()
}

// setup publishes the hotel: an organization, the site, a nightly calendar
// and thirteen rooms.
func (h *hotel) setup(ctx context.Context) {
	c := h.c
	c.Step("Riverside Hotels opens Lisbon Riverside (run %s)", h.sfx)
	var err error
	h.org, h.site, err = newSite(ctx, h.e, "riverside-hotels-"+h.sfx, "Riverside Hotels", "lisbon-riverside-"+h.sfx, "Lisbon Riverside")
	c.Must(err, "create the hotel")
	c.Say("organization %s, site %s", h.org, h.site)

	cal, err := newCalendar(ctx, h.e, "lisbon-rooms-"+h.sfx, "Lisbon Riverside rooms", zoneName)
	c.Must(err, "create calendar")

	// RFC 7953 availability: rooms are let night after night from the first
	// 15:00 check-in onwards. Check-in 15:00 / check-out 11:00 is a property
	// of each stay's window, not of the availability: offered time must be
	// continuous, or no multi-night stay would fit inside it.
	opening := h.base.AddDate(0, 0, -1).Add(15 * time.Hour)
	err = openDaily(ctx, h.e, cal, "Nightly stays", opening, 24*time.Hour, "Check-in 15:00, check-out 11:00")
	c.Must(err, "create availability")

	add := func(id string, r room, occupancy int, types ...string) {
		name, err := newResource(ctx, h.e, resourceSpec{
			ID:               id + "-" + h.sfx,
			DisplayName:      "Room " + strings.ToUpper(id),
			Types:            append([]string{"hotel-room"}, types...),
			Site:             h.site,
			Calendar:         cal,
			Profile:          profile{MaxOccupancy: int32(occupancy), MinDuration: "PT20H"},
			Attributes:       r,
			AttributesSchema: "https://example.com/schema/hotel-room",
		})
		c.Must(err, "create room "+id)
		h.rooms[name] = r
		if r.Type == "suite" {
			h.suites = append(h.suites, name)
		} else {
			h.double = append(h.double, name)
		}
	}
	for i := 1; i <= 10; i++ {
		view := "city"
		if i > 5 {
			view = "river"
		}
		add(fmt.Sprintf("d%02d", i), room{Type: "double", Beds: "1 king", View: view}, 2, "double", "view-"+view)
	}
	for i := 1; i <= 3; i++ {
		add(fmt.Sprintf("s%d", i), room{Type: "suite", Beds: "1 king + sofa bed", View: "river"}, 4, "suite", "view-river")
	}
	c.Say("calendar %s (%s), nightly from %s", cal, zoneName, opening.Format("Mon 02 Jan 15:04"))
	c.Say("%d doubles and %d suites published; each needs at least one night (min_duration PT20H)", len(h.double), len(h.suites))
	c.Say("the week under test starts %s", h.base.Format("Mon 02 Jan 2006"))
}

// search asks the whole organization what of a type is free for a stay.
func (h *hotel) search(ctx context.Context, kind string, n, nights int) ([]string, error) {
	a, b := h.stay(n, nights)
	var names []string
	for m, err := range h.e.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
		Parent: h.org, Window: freebusy.Window(a, b), ResourceTypes: []string{kind}, Quantity: 1,
	}).All() {
		if err != nil {
			return nil, err
		}
		names = append(names, m.GetResource())
	}
	return names, nil
}

// hold takes one of candidates for a stay.
func (h *hotel) hold(ctx context.Context, candidates []string, n, nights int, key string) (*allocationpb.Hold, error) {
	a, b := h.stay(n, nights)
	return h.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: h.site,
		Hold:   &allocationpb.Hold{Window: freebusy.Window(a, b), Quantity: 1, CandidateResources: candidates, IdempotencyKey: key},
	})
}

// reserve turns a hold into a reservation for a guest.
func (h *hotel) reserve(ctx context.Context, hold *allocationpb.Hold, guest string, attrs map[string]any) (*reservationpb.Reservation, error) {
	s, err := freebusy.NewAttributes(attrs)
	if err != nil {
		return nil, err
	}
	return h.e.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent:      h.site,
		Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/" + guest + "-" + h.sfx, Attributes: s},
	})
}

// book is search, hold and reserve in one, for guests whose booking is not
// the point of their scene.
func (h *hotel) book(ctx context.Context, guest, kind string, n, nights int) *reservationpb.Reservation {
	options, err := h.search(ctx, kind, n, nights)
	if !h.c.NoErr(err, guest+": search") || !h.c.Expect(len(options) > 0, "%s: a %s is free for %s", guest, kind, h.describe(n, nights)) {
		return nil
	}
	hold, err := h.hold(ctx, options, n, nights, "")
	if !h.c.NoErr(err, guest+": hold") {
		return nil
	}
	r, err := h.reserve(ctx, hold, guest, map[string]any{"guest_name": guest, "guests": 2})
	if !h.c.NoErr(err, guest+": reserve") {
		return nil
	}
	h.c.Say("%s booked %s for %s", guest, h.label(r.GetResources()[0]), h.describe(n, nights))
	return r
}

func (h *hotel) label(name string) string {
	r := h.rooms[name]
	id := strings.TrimSuffix(strings.TrimPrefix(name, "resources/"), "-"+h.sfx)
	return fmt.Sprintf("%s (%s, %s view)", strings.ToUpper(id), r.Type, r.View)
}

// firstGuest: search, options, hold, reserve -- the whole checkout.
func (h *hotel) firstGuest(ctx context.Context) {
	c := h.c
	c.Step("Ana looks for a double for 3 nights: %s", h.describe(0, 3))
	options, err := h.search(ctx, "double", 0, 3)
	if !c.NoErr(err, "SearchAvailability") {
		return
	}
	c.Expect(len(options) == 10, "all 10 doubles are offered (got %d)", len(options))
	for _, o := range options[:3] {
		c.Say("option: %s", h.label(o))
	}
	c.Say("... and %d more", len(options)-3)

	// She prefers a river view: SearchAvailability's resource_types match any
	// of the types given, not all, so "double AND river view" is filtered here.
	var river []string
	for _, o := range options {
		if h.rooms[o].View == "river" {
			river = append(river, o)
		}
	}
	key := "checkout-ana-" + h.sfx
	hold, err := h.hold(ctx, river, 0, 3, key)
	if !c.NoErr(err, "CreateHold") {
		return
	}
	c.Expect(hold.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE && slices.Contains(river, hold.GetResources()[0]),
		"hold %s is ACTIVE on a river-view double: %s, until %s", lastSegment(hold.GetName()), h.label(hold.GetResources()[0]),
		hold.GetExpireTime().AsTime().Format("15:04:05"))
	again, err := h.hold(ctx, river, 0, 3, key)
	c.Expect(err == nil && again.GetName() == hold.GetName(), "retrying the hold with the same idempotency key returns the same hold")

	r, err := h.reserve(ctx, hold, "ana", map[string]any{"guest_name": "Ana Sousa", "email": "ana@example.com", "guests": 2})
	if !c.NoErr(err, "CreateReservation") {
		return
	}
	c.Expect(r.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED,
		"reservation %s is %s for %s, in one call: CreateReservation converts the hold, with no separate confirm step",
		lastSegment(r.GetName()), r.GetState(), r.GetCustomer())
	got, err := h.e.Holds.Get(ctx, &allocationpb.GetHoldRequest{Name: hold.GetName()})
	c.Expect(err == nil && got.GetState() == allocationpb.HoldState_HOLD_STATE_CONVERTED, "the hold is now CONVERTED")
}

// updateBooking: the guest changes what is kept with the booking, with the
// etag guarding against a concurrent edit.
func (h *hotel) updateBooking(ctx context.Context) {
	c := h.c
	c.Step("Ben books a double, then tells the hotel they are three and arrive late")
	r := h.book(ctx, "ben", "double", 1, 2)
	if r == nil {
		return
	}
	attrs, _ := freebusy.NewAttributes(map[string]any{"guest_name": "Ben Carter", "guests": 3, "late_check_in": true, "eta": "23:30"})
	updated, err := h.e.Reservations.Update(ctx, &reservationpb.UpdateReservationRequest{
		Reservation: &reservationpb.Reservation{Name: r.GetName(), Attributes: attrs, Etag: r.GetEtag()},
		UpdateMask:  freebusy.NewUpdateMask("attributes"),
	})
	if !c.NoErr(err, "UpdateReservation") {
		return
	}
	f := updated.GetAttributes().GetFields()
	c.Expect(f["guests"].GetNumberValue() == 3 && f["late_check_in"].GetBoolValue() && updated.GetEtag() != r.GetEtag(),
		"attributes now guests=%v late_check_in=%v eta=%v; etag moved", f["guests"].GetNumberValue(), f["late_check_in"].GetBoolValue(), f["eta"].GetStringValue())
	c.Expect(updated.GetStartTime().AsTime().Equal(r.GetStartTime().AsTime()) && slices.Equal(updated.GetResources(), r.GetResources()),
		"room and dates are unchanged")

	_, err = h.e.Reservations.Update(ctx, &reservationpb.UpdateReservationRequest{
		Reservation: &reservationpb.Reservation{Name: r.GetName(), Attributes: attrs, Etag: r.GetEtag()},
		UpdateMask:  freebusy.NewUpdateMask("attributes"),
	})
	p := problemOf(err)
	c.Expect(errors.Is(err, freebusy.ErrAborted), "a second edit sent with the old etag is refused: %s", p)
}

// reschedule: the guest moves to other dates; the new dates are secured first.
func (h *hotel) reschedule(ctx context.Context) {
	c := h.c
	c.Step("Chloe books two nights, then moves her stay later in the week")
	r := h.book(ctx, "chloe", "double", 2, 2)
	if r == nil {
		return
	}
	options, err := h.search(ctx, "double", 4, 2)
	if !c.NoErr(err, "search new dates") {
		return
	}
	hold, err := h.hold(ctx, options, 4, 2, "")
	if !c.NoErr(err, "hold new dates") {
		return
	}
	moved, err := h.e.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{
		Name: r.GetName(), Hold: hold.GetName(), Etag: r.GetEtag(),
	})
	if !c.NoErr(err, "RescheduleReservation") {
		return
	}
	a, _ := h.stay(4, 2)
	c.Expect(moved.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && moved.GetStartTime().AsTime().Equal(a) &&
		moved.GetReplaces() == r.GetName(),
		"replacement %s is CONFIRMED for %s in %s", lastSegment(moved.GetName()), h.describe(4, 2), h.label(moved.GetResources()[0]))
	old, err := h.e.Reservations.Get(ctx, &reservationpb.GetReservationRequest{Name: r.GetName()})
	c.Expect(err == nil && old.GetState() == reservationpb.ReservationState_RESERVATION_STATE_RESCHEDULED && old.GetReplacement() == moved.GetName(),
		"the original is RESCHEDULED and points at its replacement")
	c.Expect(moved.GetCustomer() == r.GetCustomer() && moved.GetAttributes().GetFields()["guest_name"].GetStringValue() == "chloe",
		"customer and attributes carried over to the replacement")
}

// cancel: the guest cancels and the room is free again.
func (h *hotel) cancel(ctx context.Context) {
	c := h.c
	c.Step("Dan books two nights, then cancels")
	r := h.book(ctx, "dan", "double", 0, 2)
	if r == nil {
		return
	}
	cancelled, err := h.e.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{
		Name: r.GetName(), Reason: "guest cancelled: flight changed", Etag: r.GetEtag(),
	})
	if !c.NoErr(err, "CancelReservation") {
		return
	}
	c.Expect(cancelled.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED,
		"reservation is %s (%q)", cancelled.GetState(), cancelled.GetCancelReason())
	a, b := h.stay(0, 2)
	check, err := h.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{
		Parent: h.site, Resources: r.GetResources(), Window: freebusy.Window(a, b), Quantity: 1,
	})
	c.Expect(err == nil && check.GetBookable(), "%s is bookable again for those nights", h.label(r.GetResources()[0]))
	again, err := h.e.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{Name: r.GetName(), Reason: "again"})
	c.Expect(err == nil && again.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED,
		"cancelling again is a no-op that answers CANCELLED")
}

// raceForLastSuite: two guests see the same last suite and both try to take
// it at the same instant.
func (h *hotel) raceForLastSuite(ctx context.Context) {
	c := h.c
	c.Step("Two suites go to Eve and Finn; then Gabi and Hugo race for the last one, %s", h.describe(0, 2))
	h.book(ctx, "eve", "suite", 0, 2)
	h.book(ctx, "finn", "suite", 0, 2)

	guests := []string{"gabi", "hugo"}
	seen := make([][]string, len(guests))
	for i, g := range guests {
		options, err := h.search(ctx, "suite", 0, 2)
		c.NoErr(err, g+": search")
		seen[i] = options
		c.Say("%s sees %d suite(s) free: %v", g, len(options), labels(h, options))
	}
	if !c.Expect(len(seen[0]) == 1 && slices.Equal(seen[0], seen[1]), "both see the same single suite") {
		return
	}

	type outcome struct {
		hold *allocationpb.Hold
		err  error
	}
	results := make([]outcome, len(guests))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range guests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			hold, err := h.hold(ctx, seen[i], 0, 2, "")
			results[i] = outcome{hold, err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, g := range guests {
		if results[i].err == nil {
			winners++
			c.Say("%s got hold %s on %s", g, lastSegment(results[i].hold.GetName()), h.label(results[i].hold.GetResources()[0]))
			r, err := h.reserve(ctx, results[i].hold, g, map[string]any{"guests": 4})
			c.Expect(err == nil && r.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "%s's suite is CONFIRMED", g)
			continue
		}
		p := problemOf(results[i].err)
		c.Say("%s was told: %s", g, p)
		c.Expect(errors.Is(results[i].err, freebusy.ErrCapacityExhausted), "%s gets a clear CAPACITY_EXHAUSTED conflict", g)
	}
	c.Expect(winners == 1, "exactly one guest wins the last suite (winners: %d)", winners)
}

// abandonedCheckout: a guest asks for more time at checkout, then gives up;
// releasing the hold frees the room at once, without waiting for it to expire.
func (h *hotel) abandonedCheckout(ctx context.Context) {
	c := h.c
	c.Step("Iris holds a double for %s, asks for more time, then abandons checkout", h.describe(5, 2))
	a, b := h.stay(5, 2)
	free := func() int32 {
		res, err := h.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{Parent: h.site, Resources: h.double, Window: freebusy.Window(a, b)})
		if err != nil {
			c.NoErr(err, "CheckAvailability")
			return -1
		}
		return res.GetFreeCount()
	}
	before := free()
	hold, err := h.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: h.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, b), CandidateResources: h.double},
	})
	if !c.NoErr(err, "CreateHold") {
		return
	}
	c.Expect(free() == before-1, "while held, one double fewer is free (%d -> %d)", before, before-1)

	// ExtendHold's duration counts from now, not from the current deadline.
	extended, err := h.e.Holds.Extend(ctx, &allocationpb.ExtendHoldRequest{
		Name: hold.GetName(), Duration: freebusy.NewDuration(30 * time.Minute), Etag: hold.GetEtag(),
	})
	if c.NoErr(err, "ExtendHold") {
		c.Expect(extended.GetExpireTime().AsTime().After(hold.GetExpireTime().AsTime()),
			"she asks for 30 minutes: the hold now lasts until %[2]s, not %[1]s", hold.GetExpireTime().AsTime().Format("15:04:05"), extended.GetExpireTime().AsTime().Format("15:04:05"))
	}

	released, err := h.e.Holds.Release(ctx, &allocationpb.ReleaseHoldRequest{Name: hold.GetName()})
	if !c.NoErr(err, "ReleaseHold") {
		return
	}
	c.Expect(released.GetState() == allocationpb.HoldState_HOLD_STATE_RELEASED, "Iris closes the tab; the hold is %s", released.GetState())
	c.Expect(free() == before, "and the room is free again (%d free)", before)
	again, err := h.e.Holds.Release(ctx, &allocationpb.ReleaseHoldRequest{Name: hold.GetName()})
	c.Expect(err == nil && again.GetState() == allocationpb.HoldState_HOLD_STATE_RELEASED, "releasing it again is a no-op")

	_, err = h.e.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent: h.site, Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/iris-" + h.sfx},
	})
	p := problemOf(err)
	c.Expect(err != nil && p.Reason == "HOLD_NOT_ACTIVE", "reserving from it now is refused: %s", p)

	found := false
	for l, err := range h.e.Holds.ListAutoPaging(ctx, &allocationpb.ListHoldsRequest{Parent: h.site, Filter: `state = "HOLD_STATE_RELEASED"`}).All() {
		if !c.NoErr(err, "ListHolds by state") {
			return
		}
		found = found || l.GetName() == hold.GetName()
	}
	c.Expect(found, "filtering holds by state RELEASED finds it")
}

// weekAtAGlance: the front desk's view of the week, night by night.
func (h *hotel) weekAtAGlance(ctx context.Context) {
	c := h.c
	c.Step("Front desk: free rooms per night, %s to %s", h.checkIn(0).Format("Mon 02 Jan"), h.checkIn(7).Format("Mon 02 Jan"))
	// Doubles: Ana nights 0-2, Ben 1-2, Chloe moved to 4-5; Dan cancelled,
	// Iris abandoned. Suites: three taken for nights 0-1.
	wantDoubles := []int32{9, 8, 8, 10, 9, 9, 10}
	wantSuites := []int32{0, 0, 3, 3, 3, 3, 3}
	count := func(names []string) []int32 {
		res, err := h.e.Availability.ComputeAvailability(ctx, &allocationpb.ComputeAvailabilityRequest{
			Parent: h.site, Resources: names, Window: freebusy.Window(h.checkIn(0), h.checkIn(7)),
			Granularity: allocationpb.AvailabilityGranularity_AVAILABILITY_GRANULARITY_DAY, TimeZone: zoneName,
		})
		if !c.NoErr(err, "ComputeAvailability") {
			return nil
		}
		var out []int32
		for _, b := range res.GetBuckets() {
			out = append(out, b.GetFreeCount())
		}
		return out
	}
	doubles, suites := count(h.double), count(h.suites)
	c.Say("night        doubles  suites")
	for n := 0; n < len(doubles) && n < len(suites); n++ {
		c.Say("%s   %2d/10    %d/3", h.checkIn(n).Format("Mon 02 Jan"), doubles[n], suites[n])
	}
	c.Expect(slices.Equal(doubles, wantDoubles), "doubles free per night %v, want %v", doubles, wantDoubles)
	c.Expect(slices.Equal(suites, wantSuites), "suites free per night %v, want %v", suites, wantSuites)
}

func labels(h *hotel, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = h.label(n)
	}
	return out
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}
