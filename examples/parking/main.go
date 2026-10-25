// Copyright 2026 Shokki Engineering
// SPDX-License-Identifier: Apache-2.0

// Command parking acts out a parking operator running a city-centre garage on
// freebusy, through the Go SDK, against a running server: 50 bays (10 with EV chargers), open
// 06:00-23:00 every day and sold by the hour; an EV driver's booking, a rush
// of 60 drivers for 50 bays, an extension, a booking outside opening hours,
// and a cancelled bay going back on sale.
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

const (
	zoneName = "Europe/Madrid"
	bays     = 50
	evBays   = 10
	drivers  = 60
)

type garage struct {
	e    *freebusy.Client
	c    *checker
	sfx  string
	org  string
	site string
	day  time.Time // midnight of the first day under test, garage time
	ev   []string
	all  []string

	// marta is the EV driver's reservation, extended later.
	marta *reservationpb.Reservation
	// rush are the reservations the peak-hour rush made; losers the drivers
	// who were turned away.
	rush   []*reservationpb.Reservation
	losers []string
}

// at is hour h of day d, counted from the first day under test.
func (g *garage) at(d, h int) time.Time {
	return g.day.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour)
}

func main() {
	ctx := context.Background()
	c := &checker{}
	e, err := connect(ctx)
	c.Must(err, "connect to freebusy")
	defer func() { _ = e.Close() }()

	loc, err := time.LoadLocation(zoneName)
	c.Must(err, "load zone")
	d := time.Now().In(loc).AddDate(0, 0, 14)
	g := &garage{e: e, c: c, sfx: suffix(), day: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)}

	g.setup(ctx)
	g.evDriver(ctx)
	g.slots(ctx)
	g.rushHour(ctx)
	g.extend(ctx)
	g.outsideHours(ctx)
	g.cancelAndResell(ctx)
	g.unknownSite(ctx)
	c.Finish()
}

func (g *garage) setup(ctx context.Context) {
	c := g.c
	c.Step("CityPark opens Central Garage (run %s)", g.sfx)
	var err error
	g.org, g.site, err = newSite(ctx, g.e, "citypark-"+g.sfx, "CityPark", "central-garage-"+g.sfx, "Central Garage")
	c.Must(err, "create the garage")
	c.Say("organization %s, site %s", g.org, g.site)

	cal, err := newCalendar(ctx, g.e, "central-garage-bays-"+g.sfx, "Central Garage bays", zoneName)
	c.Must(err, "create calendar")
	opens := g.at(-1, 6)
	err = openDaily(ctx, g.e, cal, "Opening hours", opens, 17*time.Hour, "Open 06:00-23:00 daily")
	c.Must(err, "create opening hours")

	for i := 1; i <= bays; i++ {
		types := []string{"parking-bay"}
		attrs := map[string]any{"level": -1 - (i-1)/25, "bay": i}
		if i <= evBays {
			types = append(types, "ev-charging")
			attrs["charger_kw"] = 22
		}
		name, err := newResource(ctx, g.e, resourceSpec{
			ID:               fmt.Sprintf("bay-%02d-%s", i, g.sfx),
			DisplayName:      fmt.Sprintf("Bay %02d", i),
			Types:            types,
			Site:             g.site,
			Calendar:         cal,
			Profile:          profile{MaxOccupancy: 1, MinDuration: "PT1H", MaxDuration: "PT17H"},
			Attributes:       attrs,
			AttributesSchema: "https://example.com/schema/parking-bay",
		})
		c.Must(err, "create bay")
		g.all = append(g.all, name)
		if i <= evBays {
			g.ev = append(g.ev, name)
		}
	}
	c.Say("%d bays (%d with 22 kW chargers), open 06:00-23:00 %s, from %s; bookings 1h to 17h",
		len(g.all), len(g.ev), zoneName, opens.Format("Mon 02 Jan"))
}

func (g *garage) label(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(name, "resources/"), "-"+g.sfx)
}

func (g *garage) hold(ctx context.Context, a, b time.Time, candidates []string) (*allocationpb.Hold, error) {
	return g.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: g.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, b), Quantity: 1, CandidateResources: candidates},
	})
}

func (g *garage) reserve(ctx context.Context, h *allocationpb.Hold, driver, plate string) (*reservationpb.Reservation, error) {
	attrs, _ := freebusy.NewAttributes(map[string]any{"plate": plate})
	return g.e.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent: g.site, Reservation: &reservationpb.Reservation{Hold: h.GetName(), Customer: "users/" + driver + "-" + g.sfx, Attributes: attrs},
	})
}

func (g *garage) evDriver(ctx context.Context) {
	c := g.c
	a, b := g.at(0, 9), g.at(0, 11)
	c.Step("Marta needs an EV bay %s 09:00-11:00", a.Format("Mon 02 Jan"))
	var options []string
	for m, err := range g.e.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
		Parent: g.org, Window: freebusy.Window(a, b), ResourceTypes: []string{"ev-charging"}, Quantity: 1,
	}).All() {
		if !c.NoErr(err, "SearchAvailability") {
			return
		}
		options = append(options, m.GetResource())
	}
	c.Expect(len(options) == evBays, "%d EV bays offered (want %d)", len(options), evBays)
	h, err := g.hold(ctx, a, b, options)
	if !c.NoErr(err, "CreateHold") {
		return
	}
	c.Expect(slices.Contains(g.ev, h.GetResources()[0]), "held %s until %s", g.label(h.GetResources()[0]), h.GetExpireTime().AsTime().In(a.Location()).Format("15:04"))
	r, err := g.reserve(ctx, h, "marta", "1234-KLM")
	if !c.NoErr(err, "CreateReservation") {
		return
	}
	c.Expect(r.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "reservation %s %s", g.label(r.GetResources()[0]), r.GetState())
	g.marta = r
}

func (g *garage) slots(ctx context.Context) {
	c := g.c
	c.Step("The operator's hourly board for %s", g.day.Format("Mon 02 Jan"))
	res, err := g.e.Availability.ComputeAvailability(ctx, &allocationpb.ComputeAvailabilityRequest{
		Parent: g.site, Window: freebusy.Window(g.at(0, 5), g.at(1, 0)),
		Granularity: allocationpb.AvailabilityGranularity_AVAILABILITY_GRANULARITY_SLOT, SlotDuration: freebusy.NewDuration(time.Hour),
	})
	if !c.NoErr(err, "ComputeAvailability") {
		return
	}
	var line []string
	ok := true
	for _, bk := range res.GetBuckets() {
		start := bk.GetWindow().GetStartTime().AsTime().In(g.day.Location())
		line = append(line, fmt.Sprintf("%02d:%d", start.Hour(), bk.GetFreeCount()))
		want := int32(bays)
		switch h := start.Hour(); {
		case h < 6 || h >= 23:
			want = 0
		case h == 9 || h == 10:
			want = bays - 1
		}
		ok = ok && bk.GetFreeCount() == want
	}
	c.Say("%s", strings.Join(line[:10], "  "))
	c.Say("%s", strings.Join(line[10:], "  "))
	c.Expect(len(res.GetBuckets()) == 19 && ok, "closed before 06:00 and from 23:00, 49 free at 09 and 10, 50 otherwise")
}

func (g *garage) rushHour(ctx context.Context) {
	c := g.c
	a, b := g.at(1, 8), g.at(1, 10)
	c.Step("Rush hour: %d drivers try to book 08:00-10:00 on %s at once, for %d bays", drivers, a.Format("Mon 02 Jan"), bays)
	type outcome struct {
		r       *reservationpb.Reservation
		err     error
		aborted int // CreateHold calls answered ABORTED before the final one
	}
	// A driver's app retries a hold the server answered ABORTED
	// (CONCURRENT_MODIFICATION: "the caller may retry"), a few times.
	const retries = 5
	results := make([]outcome, drivers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	began := time.Now()
	for i := range drivers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var o outcome
			var h *allocationpb.Hold
			for attempt := 0; ; attempt++ {
				h, o.err = g.hold(ctx, a, b, nil)
				if o.err == nil || !errors.Is(o.err, freebusy.ErrAborted) || attempt == retries {
					break
				}
				o.aborted++
			}
			if o.err == nil {
				o.r, o.err = g.reserve(ctx, h, fmt.Sprintf("driver-%02d", i), fmt.Sprintf("%04d-RSH", i))
			}
			results[i] = o
		}(i)
	}
	close(start)
	wg.Wait()

	exhausted, aborts, abortedDrivers, other := 0, 0, 0, map[string]int{}
	for i, o := range results {
		aborts += o.aborted
		if o.aborted > 0 {
			abortedDrivers++
		}
		switch p := problemOf(o.err); {
		case o.err == nil:
			g.rush = append(g.rush, o.r)
		case p.Reason == "CAPACITY_EXHAUSTED":
			exhausted++
			g.losers = append(g.losers, fmt.Sprintf("driver-%02d", i))
		default:
			other[p.Code+"/"+p.Reason]++
		}
	}
	c.Say("in %s: %d booked, %d told the garage is full, other errors %v", time.Since(began).Round(time.Millisecond), len(g.rush), exhausted, other)
	if aborts > 0 {
		c.Note("rush hour: %d of %d drivers had CreateHold answer ABORTED/CONCURRENT_MODIFICATION (%d times in all) while bays were still free; they only got through by retrying",
			abortedDrivers, drivers, aborts)
	}
	c.Expect(len(g.rush) == bays, "exactly %d drivers got a bay (got %d)", bays, len(g.rush))
	c.Expect(exhausted == drivers-bays, "exactly %d were turned away with CAPACITY_EXHAUSTED (got %d)", drivers-bays, exhausted)
	c.Expect(len(other) == 0, "no driver saw any other error")

	// The ledger: every confirmed reservation in the window, one per bay.
	byBay := map[string]int{}
	n := 0
	// The iterator fetches page after page, 25 reservations at a time.
	for r, err := range g.e.Reservations.ListAutoPaging(ctx, &reservationpb.ListReservationsRequest{Parent: g.site, PageSize: 25}).All() {
		if !c.NoErr(err, "ListReservations") {
			return
		}
		if r.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && r.GetStartTime().AsTime().Equal(a) {
			n++
			for _, res := range r.GetResources() {
				byBay[res]++
			}
		}
	}
	double := 0
	for _, k := range byBay {
		if k > 1 {
			double++
		}
	}
	c.Expect(n == bays && len(byBay) == bays && double == 0, "ListReservations shows %d confirmed bookings on %d distinct bays, %d bay(s) double-booked", n, len(byBay), double)
	check, err := g.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{Parent: g.site, Window: freebusy.Window(a, b), Quantity: 1})
	c.Expect(err == nil && !check.GetBookable() && check.GetFreeCount() == 0, "CheckAvailability now says the garage is full for 08:00-10:00")
}

func (g *garage) extend(ctx context.Context) {
	c := g.c
	if g.marta == nil {
		return
	}
	bay := g.marta.GetResources()[0]
	a, b := g.at(0, 9), g.at(0, 13)
	c.Step("Marta's meeting runs long: she extends to 13:00 on the same bay")
	_, err := g.hold(ctx, a, b, []string{bay})
	p := problemOf(err)
	c.Expect(err != nil && p.Reason == "CAPACITY_EXHAUSTED",
		"a plain hold for 09:00-13:00 on %s is refused while her 09:00-11:00 booking stands: %s / %s", g.label(bay), p.Code, p.Reason)

	h, err := g.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: g.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, b), Quantity: 1, CandidateResources: []string{bay}, Replaces: g.marta.GetHold()},
	})
	if !c.NoErr(err, "hold 09:00-13:00 on her bay, replacing her booking") {
		return
	}
	c.Expect(slices.Equal(h.GetResources(), []string{bay}), "with replaces set to her booking's hold, the hold keeps %s", g.label(bay))
	_, err = g.reserve(ctx, h, "someone-else", "XX-00-XX")
	p = problemOf(err)
	c.Expect(err != nil && p.Reason == "HOLD_FOR_RESCHEDULE", "that hold cannot be booked as a new reservation: %s / %s", p.Code, p.Reason)
	moved, err := g.e.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{Name: g.marta.GetName(), Hold: h.GetName(), Etag: g.marta.GetEtag()})
	if !c.NoErr(err, "RescheduleReservation") {
		return
	}
	c.Expect(moved.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && moved.GetEndTime().AsTime().Equal(b) &&
		slices.Equal(moved.GetResources(), []string{bay}),
		"now %s until 13:00, still on %s", moved.GetState(), g.label(moved.GetResources()[0]))
	for _, w := range [][2]time.Time{{g.at(0, 9), g.at(0, 11)}, {g.at(0, 11), g.at(0, 13)}} {
		check, err := g.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{Parent: g.site, Resources: []string{bay}, Window: freebusy.Window(w[0], w[1])})
		c.Expect(err == nil && !check.GetBookable(), "%s stays taken %s-%s: nobody slipped in during the move", g.label(bay), w[0].Format("15:04"), w[1].Format("15:04"))
	}
}

func (g *garage) outsideHours(ctx context.Context) {
	c := g.c
	c.Step("Late arrivals try to book outside opening hours")
	noted := false
	for _, w := range []struct {
		what string
		a, b time.Time
	}{
		{"22:00-24:00", g.at(0, 22), g.at(1, 0)},
		{"05:00-07:00", g.at(0, 5), g.at(0, 7)},
		// The opening hours run from 06:00 the day before, with no end: the
		// day before that is before they start, so nothing is offered.
		{"10:00-12:00 two days before, before the opening hours start", g.at(-2, 10), g.at(-2, 12)},
	} {
		_, err := g.hold(ctx, w.a, w.b, nil)
		p := problemOf(err)
		c.Expect(errors.Is(err, freebusy.ErrFailedPrecondition), "CreateHold %s is refused: %s / %s", w.what, p.Code, p.Reason)
		c.Say("the driver reads: %q", p.Message)
		if (!strings.Contains(strings.ToLower(p.Message), "availab") || p.Reason == "CAPACITY_EXHAUSTED") && !noted {
			noted = true
			c.Note("CreateHold outside opening hours answers %s (%q...) and does not say the garage is closed; only CheckAvailability gives the reason", p.Reason, p.Message[:min(len(p.Message), 60)])
		}
		check, err := g.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{Parent: g.site, Window: freebusy.Window(w.a, w.b), Quantity: 1})
		if !c.NoErr(err, "CheckAvailability") {
			continue
		}
		codesSeen := map[allocationpb.UnbookableReasonCode]int{}
		var sample string
		for _, r := range check.GetReasons() {
			codesSeen[r.GetCode()]++
			sample = r.GetMessage()
		}
		c.Expect(!check.GetBookable() && codesSeen[allocationpb.UnbookableReasonCode_UNBOOKABLE_REASON_CODE_OUTSIDE_AVAILABILITY] > 0,
			"CheckAvailability %s: bookable=%v, reasons %v (%q)", w.what, check.GetBookable(), codesSeen, sample)
	}
}

func (g *garage) cancelAndResell(ctx context.Context) {
	c := g.c
	if len(g.rush) == 0 || len(g.losers) == 0 {
		return
	}
	a, b := g.at(1, 8), g.at(1, 10)
	r := g.rush[0]
	c.Step("%s cancels; %s, turned away earlier, tries again", r.GetCustomer(), g.losers[0])
	cancelled, err := g.e.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{Name: r.GetName(), Reason: "plans changed"})
	if !c.NoErr(err, "CancelReservation") {
		return
	}
	c.Expect(cancelled.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED, "%s on %s is %s", lastSegment(r.GetName()), g.label(r.GetResources()[0]), cancelled.GetState())
	check, err := g.e.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{Parent: g.site, Window: freebusy.Window(a, b), Quantity: 1})
	c.Expect(err == nil && check.GetFreeCount() == 1 && slices.Equal(check.GetCandidateResources(), r.GetResources()),
		"exactly one bay is free 08:00-10:00, and it is %s", g.label(r.GetResources()[0]))
	h, err := g.hold(ctx, a, b, nil)
	if !c.NoErr(err, "re-hold") {
		return
	}
	again, err := g.reserve(ctx, h, g.losers[0], "9999-NEW")
	c.Expect(err == nil && again.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && slices.Equal(again.GetResources(), r.GetResources()),
		"%s now has %s", g.losers[0], g.label(h.GetResources()[0]))
}

// unknownSite: a call naming a site that does not exist is answered NOT_FOUND,
// with SITE_NOT_FOUND as the reason.
func (g *garage) unknownSite(ctx context.Context) {
	c := g.c
	site := "organizationalUnits/no-such-garage-" + g.sfx
	c.Step("A typo: a driver's app asks %s for a bay", site)
	_, err := g.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: site, Hold: &allocationpb.Hold{Window: freebusy.Window(g.at(0, 12), g.at(0, 13))},
	})
	p := problemOf(err)
	c.Expect(errors.Is(err, freebusy.ErrNotFound) && p.Reason == "SITE_NOT_FOUND",
		"the hold is refused NOT_FOUND / SITE_NOT_FOUND: %s", p)
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}
