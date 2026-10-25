// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

// Command quickstart walks freebusy end to end from an empty server, with
// every call through the SDK: an organization and a site, three meeting
// rooms, a calendar with opening hours, a search for a free room, a hold, a
// reservation, and a webhook receiving the reservation's event.
//
// It exits non-zero if any expectation fails.
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/option"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/organizationalunitpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
	"github.com/shokki-engineering/freebusy-go/pb/resourcepb"
	"github.com/shokki-engineering/freebusy-go/pb/webhookpb"
)

const zone = "Europe/London"

func main() {
	ctx := context.Background()
	c := &checker{}
	client, err := connect(ctx)
	c.Must(err, "connect")
	defer func() { _ = client.Close() }()
	sfx := suffix()
	loc, err := time.LoadLocation(zone)
	c.Must(err, "load zone")

	c.Step("A webhook receiver, subscribed before anything happens")
	events := make(chan *freebusy.WebhookEvent, 64)
	var secret string
	l, err := net.Listen("tcp", "127.0.0.1:0")
	c.Must(err, "listen")
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, err := freebusy.ParseWebhook(r, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		events <- e
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()
	hook, err := client.Webhooks.New(ctx, &webhookpb.CreateWebhookRequest{
		WebhookId: "quickstart-" + sfx,
		Webhook: &webhookpb.Webhook{
			Uri:        "http://" + l.Addr().String() + "/hooks",
			EventTypes: []string{freebusy.EventReservationConfirmed},
		},
	})
	c.Must(err, "create webhook")
	secret = hook.GetSecret()
	c.Expect(secret != "", "webhook %s created with a signing secret", hook.GetName())

	c.Step("Organization, site, calendar and opening hours")
	org, site, err := newSite(ctx, client, "acme-"+sfx, "Acme", "hq-"+sfx, "Acme HQ")
	c.Must(err, "create site")
	c.Say("organization %s, site %s", org, site)
	got, err := client.Sites.Get(ctx, &organizationalunitpb.GetOrganizationalUnitRequest{Name: site})
	c.Expect(err == nil && got.GetName() == site, "client.Sites.Get reads it back")
	cal, err := newCalendar(ctx, client, "hq-rooms-"+sfx, "HQ meeting rooms", zone)
	c.Must(err, "create calendar")
	day := time.Now().In(loc).AddDate(0, 0, 7)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	c.Must(openDaily(ctx, client, cal, "Office hours", day.Add(8*time.Hour), 10*time.Hour, "08:00-18:00"), "open daily")

	c.Step("Three meeting rooms")
	var rooms []string
	for _, id := range []string{"oak", "elm", "ash"} {
		name, err := newResource(ctx, client, resourceSpec{
			ID: id + "-" + sfx, DisplayName: "Room " + id, Types: []string{"meeting-room"},
			Site: site, Calendar: cal, Profile: profile{MaxOccupancy: 1, MinDuration: "PT30M"},
		})
		c.Must(err, "create room")
		rooms = append(rooms, name)
	}
	n := 0
	for r, err := range client.Resources.ListAutoPaging(ctx, &resourcepb.ListResourcesRequest{Filter: `organizational_unit = "` + site + `"`}).All() {
		if err != nil {
			c.Note("listing resources by site: %v", err)
			break
		}
		_ = r
		n++
	}
	c.Say("client.Resources.ListAutoPaging sees %d resource(s) at the site", n)
	c.Expect(len(rooms) == 3, "three rooms created: %v", rooms)

	c.Step("Search, hold, reserve")
	start, end := day.Add(10*time.Hour), day.Add(11*time.Hour)
	var free []string
	for m, err := range client.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
		Parent: org, Window: freebusy.Window(start, end), ResourceTypes: []string{"meeting-room"}, Quantity: 1,
	}).All() {
		if !c.NoErr(err, "search") {
			break
		}
		free = append(free, m.GetResource())
	}
	c.Expect(len(free) == 3, "all three rooms are free 10:00-11:00 (%d)", len(free))
	hold, err := client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: site,
		Hold:   &allocationpb.Hold{Window: freebusy.Window(start, end), Quantity: 1, CandidateResources: free, IdempotencyKey: "quickstart-" + sfx},
	})
	c.Must(err, "hold")
	c.Expect(hold.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE, "hold %s is ACTIVE on %v", hold.GetName(), hold.GetResources())
	res, err := client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent:      site,
		Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/dana-" + sfx},
	})
	c.Must(err, "reserve")
	c.Expect(res.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED, "reservation %s is CONFIRMED", res.GetName())

	c.Step("The same room again is refused, with a typed error")
	_, err = client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: site, Hold: &allocationpb.Hold{Window: freebusy.Window(start, end), Quantity: 1, CandidateResources: hold.GetResources()},
	}, option.WithTimeout(10*time.Second))
	c.Expect(errors.Is(err, freebusy.ErrCapacityExhausted), "errors.Is(err, freebusy.ErrCapacityExhausted): %s", problemOf(err))

	c.Step("The webhook delivers the reservation")
	select {
	case e := <-events:
		r, err := e.Reservation()
		c.Expect(err == nil && e.Type == freebusy.EventReservationConfirmed && r.GetName() == res.GetName(),
			"%s arrived for %s", e.Type, r.GetName())
	case <-time.After(30 * time.Second):
		c.Expect(false, "a %s delivery within 30s", freebusy.EventReservationConfirmed)
	}
	deliveries := 0
	for _, err := range client.Webhooks.Deliveries.ListAutoPaging(ctx, &webhookpb.ListDeliveriesRequest{Parent: hook.GetName()}).All() {
		if !c.NoErr(err, "list deliveries") {
			break
		}
		deliveries++
	}
	c.Expect(deliveries >= 1, "client.Webhooks.Deliveries lists %d delivery(ies)", deliveries)

	c.Step("Clean up the webhook")
	c.NoErr(client.Webhooks.Delete(ctx, &webhookpb.DeleteWebhookRequest{Name: hook.GetName(), Force: true}), "delete webhook (force: its deliveries go with it)")
	_, err = client.Webhooks.Get(ctx, &webhookpb.GetWebhookRequest{Name: hook.GetName()})
	c.Expect(errors.Is(err, freebusy.ErrNotFound), "it is gone: %s", problemOf(err))
	c.Finish()
}
