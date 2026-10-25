// Copyright 2026 Shokki Engineering
// SPDX-License-Identifier: Apache-2.0

// Command webhooks acts out a coworking space's front desk listening to
// freebusy: it runs its own HTTPS-style receiver, subscribes it to a site's
// bookings, and follows a meeting-room booking from hold to cancellation as
// signed events arrive. It then shows what a receiver must handle: forged
// requests, a delivery sent twice, a rotated secret, and a paused webhook.
//
// Webhooks are how an application hears about changes it did not make itself,
// or made somewhere else -- typical uses:
//
//   - send the guest a confirmation, change or cancellation email
//     (freebusy.reservation.confirmed, .updated, .cancelled);
//   - capture a payment on confirmation and refund it on cancellation;
//   - empty a checkout cart when its hold lapses (freebusy.hold.expired);
//   - mirror bookings into another system: a PMS, a channel manager, a
//     door-lock or parking-barrier controller, a CRM;
//   - keep a search index or cache of free inventory fresh
//     (freebusy.resource.*, freebusy.availability.*);
//   - alert operations, e.g. in Slack, when a booking fails.
//
// The server must accept http:// endpoints on loopback for this to run
// locally: in its config, [webhooks] enabled, allow_insecure_endpoints and
// allow_private_networks set to true (the dev overlay does this).
//
// It exits non-zero if any expectation fails.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
	"github.com/shokki-engineering/freebusy-go/pb/webhookpb"
)

const zoneName = "Europe/Amsterdam"

// wait is how long an event may take: the server sends on a short interval.
const wait = 45 * time.Second

func main() {
	ctx := context.Background()
	c := &checker{}
	e, err := connect(ctx)
	c.Must(err, "connect to freebusy")
	defer func() { _ = e.Close() }()

	rx, err := listen()
	c.Must(err, "start the receiver")
	defer func() { _ = rx.srv.Close() }()

	d := &desk{e: e, c: c, rx: rx, sfx: suffix()}
	d.setup(ctx)
	d.subscribe(ctx)
	d.booking(ctx)
	d.forgery()
	d.retry(ctx)
	d.rotate(ctx)
	d.pause(ctx)
	d.cleanup(ctx)
	c.Finish()
}

// receiver is the front desk's endpoint: it verifies each delivery with the
// SDK, answers 204, and remembers which events it has already handled.
type receiver struct {
	srv *http.Server
	url string

	mu       sync.Mutex
	secrets  []string
	seen     map[string]bool
	rejected int
	events   chan received
}

type received struct {
	event     *freebusy.WebhookEvent
	duplicate bool
}

func listen() (*receiver, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	rx := &receiver{seen: map[string]bool{}, events: make(chan received, 64)}
	rx.url = "http://" + l.Addr().String() + "/hooks/freebusy"
	rx.srv = &http.Server{Handler: rx, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = rx.srv.Serve(l) }()
	return rx, nil
}

func (rx *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rx.mu.Lock()
	secrets := slices.Clone(rx.secrets)
	rx.mu.Unlock()

	event, err := freebusy.ParseWebhook(r, secrets...)
	if err != nil {
		rx.mu.Lock()
		rx.rejected++
		rx.mu.Unlock()
		http.Error(w, "signature does not verify", http.StatusUnauthorized)
		return
	}
	// Deliveries are at least once: the same event can arrive again (a retry,
	// or a 2xx the server never saw). Its id says so.
	rx.mu.Lock()
	duplicate := rx.seen[event.ID]
	rx.seen[event.ID] = true
	rx.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	rx.events <- received{event, duplicate}
}

func (rx *receiver) trust(secrets ...string) {
	rx.mu.Lock()
	rx.secrets = secrets
	rx.mu.Unlock()
}

// next waits for the next delivery of eventType about subject, skipping
// others; ok is false on timeout.
func (rx *receiver) next(eventType, subject string, within time.Duration) (received, bool) {
	deadline := time.After(within)
	for {
		select {
		case r := <-rx.events:
			if r.event.Type == eventType && r.event.Subject == subject {
				return r, true
			}
		case <-deadline:
			return received{}, false
		}
	}
}

// quiet reports whether nothing arrives for d.
func (rx *receiver) quiet(d time.Duration) (received, bool) {
	select {
	case r := <-rx.events:
		return r, false
	case <-time.After(d):
		return received{}, true
	}
}

type desk struct {
	e   *freebusy.Client
	c   *checker
	rx  *receiver
	sfx string

	site, room string
	day        time.Time
	hook       *webhookpb.Webhook
	confirmed  string // the delivery of the confirmation
}

func (d *desk) setup(ctx context.Context) {
	c := d.c
	c.Step("Harbour Coworking opens one bookable meeting room (run %s)", d.sfx)
	_, site, err := newSite(ctx, d.e, "harbour-"+d.sfx, "Harbour Coworking", "harbour-amsterdam-"+d.sfx, "Harbour Amsterdam")
	c.Must(err, "create the site")
	d.site = site

	loc, err := time.LoadLocation(zoneName)
	c.Must(err, "load zone")
	t := time.Now().In(loc).AddDate(0, 0, 7)
	d.day = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)

	cal, err := newCalendar(ctx, d.e, "harbour-rooms-"+d.sfx, "Harbour rooms", zoneName)
	c.Must(err, "create calendar")
	c.Must(openDaily(ctx, d.e, cal, "Opening hours", d.day.AddDate(0, 0, -1).Add(8*time.Hour), 12*time.Hour, "08:00-20:00"), "open the room")
	d.room, err = newResource(ctx, d.e, resourceSpec{
		ID: "zuiderzee-" + d.sfx, DisplayName: "Zuiderzee (8 seats)", Types: []string{"meeting-room"},
		Site: d.site, Calendar: cal, Profile: profile{MaxOccupancy: 8, MinDuration: "PT30M"},
	})
	c.Must(err, "create the room")
	c.Say("site %s, room %s, receiver at %s", d.site, d.room, d.rx.url)
}

func (d *desk) subscribe(ctx context.Context) {
	c := d.c
	c.Step("The front desk subscribes to the site's holds and bookings")
	hook, err := d.e.Webhooks.New(ctx, &webhookpb.CreateWebhookRequest{
		WebhookId: "front-desk-" + d.sfx,
		Webhook: &webhookpb.Webhook{
			DisplayName: "Front desk",
			Uri:         d.rx.url,
			EventTypes: []string{
				freebusy.EventHoldActive, freebusy.EventHoldReleased,
				freebusy.EventReservationConfirmed, freebusy.EventReservationUpdated, freebusy.EventReservationCancelled,
			},
			OrganizationalUnit: d.site,
		},
	})
	c.Must(err, "CreateWebhook")
	d.hook = hook
	d.rx.trust(hook.GetSecret())
	c.Expect(hook.GetState() == webhookpb.WebhookState_WEBHOOK_STATE_ACTIVE && strings.HasPrefix(hook.GetSecret(), "whsec_"),
		"webhook %s is %s; its signing secret (whsec_...) comes back this once", hook.GetName(), hook.GetState())
	got, err := d.e.Webhooks.Get(ctx, &webhookpb.GetWebhookRequest{Name: hook.GetName()})
	c.Expect(err == nil && got.GetSecret() == "", "reading it back does not return the secret")
}

// booking follows one booking through its life, one change at a time: the
// server may fold quick successive changes to one record into one event.
func (d *desk) booking(ctx context.Context) {
	c := d.c
	a, b := d.day.Add(10*time.Hour), d.day.Add(12*time.Hour)
	c.Step("Noor books the room %s 10:00-12:00, changes it, then cancels", a.Format("Mon 02 Jan"))

	hold, err := d.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: d.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, b), Quantity: 1, CandidateResources: []string{d.room}},
	})
	if !c.NoErr(err, "CreateHold") {
		return
	}
	if r, ok := d.rx.next(freebusy.EventHoldActive, hold.GetName(), wait); c.Expect(ok, "%s arrives for the hold", freebusy.EventHoldActive) {
		h, err := r.event.Hold()
		c.Expect(err == nil && h.GetState() == allocationpb.HoldState_HOLD_STATE_ACTIVE && slices.Equal(h.GetResources(), []string{d.room}),
			"its data decodes to the hold: %s on %s", h.GetState(), h.GetResources())
	}

	attrs, _ := freebusy.NewAttributes(map[string]any{"attendees": 6, "catering": false})
	res, err := d.e.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
		Parent: d.site, Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: "users/noor-" + d.sfx, Attributes: attrs},
	})
	if !c.NoErr(err, "CreateReservation") {
		return
	}
	if r, ok := d.rx.next(freebusy.EventReservationConfirmed, res.GetName(), wait); c.Expect(ok, "%s arrives: time to email Noor", freebusy.EventReservationConfirmed) {
		got, err := r.event.Reservation()
		c.Expect(err == nil && got.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CONFIRMED && got.GetCustomer() == res.GetCustomer(),
			"it carries the whole reservation: %s for %s", got.GetState(), got.GetCustomer())
		d.confirmed = r.event.ID
	}

	attrs, _ = freebusy.NewAttributes(map[string]any{"attendees": 8, "catering": true})
	res, err = d.e.Reservations.Update(ctx, &reservationpb.UpdateReservationRequest{
		Reservation: &reservationpb.Reservation{Name: res.GetName(), Attributes: attrs, Etag: res.GetEtag()},
		UpdateMask:  freebusy.NewUpdateMask("attributes"),
	})
	if !c.NoErr(err, "UpdateReservation") {
		return
	}
	if r, ok := d.rx.next(freebusy.EventReservationUpdated, res.GetName(), wait); c.Expect(ok, "%s arrives: tell the kitchen", freebusy.EventReservationUpdated) {
		got, _ := r.event.Reservation()
		f := got.GetAttributes().GetFields()
		c.Expect(got.GetLastChange() == reservationpb.Reservation_CHANGE_DETAILS && f["catering"].GetBoolValue(),
			"last_change is %s and catering is now %v", got.GetLastChange(), f["catering"].GetBoolValue())
	}

	_, err = d.e.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{Name: res.GetName(), Reason: "meeting moved online", Etag: res.GetEtag()})
	if !c.NoErr(err, "CancelReservation") {
		return
	}
	if r, ok := d.rx.next(freebusy.EventReservationCancelled, res.GetName(), wait); c.Expect(ok, "%s arrives: refund and free the catering slot", freebusy.EventReservationCancelled) {
		got, _ := r.event.Reservation()
		c.Expect(got.GetState() == reservationpb.ReservationState_RESERVATION_STATE_CANCELLED && got.GetCancelReason() == "meeting moved online",
			"cancelled, with the reason %q", got.GetCancelReason())
	}
}

// forgery: anyone can POST to a public endpoint; only signed deliveries count.
func (d *desk) forgery() {
	c := d.c
	c.Step("Someone POSTs a fake confirmation to the endpoint")
	body := `{"specversion":"1.0","id":"fake","type":"freebusy.reservation.confirmed","subject":"x","data":{}}`
	req, _ := http.NewRequest(http.MethodPost, d.rx.url, strings.NewReader(body))
	req.Header.Set("webhook-id", "fake")
	req.Header.Set("webhook-timestamp", fmt.Sprint(time.Now().Unix()))
	req.Header.Set("webhook-signature", "v1,c2lnbmVkIGJ5IG5vYm9keQ==")
	resp, err := http.DefaultClient.Do(req)
	if !c.NoErr(err, "POST") {
		return
	}
	_ = resp.Body.Close()
	c.Expect(resp.StatusCode == http.StatusUnauthorized, "freebusy.ParseWebhook rejects it: %s", resp.Status)
}

// retry: a delivery sent again is the same event, and the receiver must
// treat it so.
func (d *desk) retry(ctx context.Context) {
	c := d.c
	c.Step("The desk's logs went missing: it asks for the confirmation again")
	var confirmation *webhookpb.Delivery
	n := 0
	for del, err := range d.e.Webhooks.Deliveries.ListAutoPaging(ctx, &webhookpb.ListDeliveriesRequest{Parent: d.hook.GetName()}).All() {
		if !c.NoErr(err, "ListDeliveries") {
			return
		}
		n++
		if del.GetEventId() == d.confirmed {
			confirmation = del
		}
		if del.GetState() != webhookpb.DeliveryState_DELIVERY_STATE_SUCCEEDED {
			c.Note("delivery %s of %s is %s", del.GetName(), del.GetEventType(), del.GetState())
		}
	}
	c.Say("%d deliveries so far", n)
	if !c.Expect(confirmation != nil && confirmation.GetResponseCode() == http.StatusNoContent,
		"ListDeliveries shows the confirmation, answered %d", confirmation.GetResponseCode()) {
		return
	}
	_, err := d.e.Webhooks.Deliveries.Retry(ctx, &webhookpb.RetryDeliveryRequest{Name: confirmation.GetName()})
	if !c.NoErr(err, "RetryDelivery") {
		return
	}
	r, ok := d.rx.next(freebusy.EventReservationConfirmed, confirmation.GetSubject(), wait)
	c.Expect(ok && r.duplicate && r.event.ID == d.confirmed, "it arrives again with the same id, so the receiver knows not to email twice")
}

// rotate: a new secret, with the old one still signing for a day.
func (d *desk) rotate(ctx context.Context) {
	c := d.c
	c.Step("The secret is rotated; the receiver switches to the new one")
	rotated, err := d.e.Webhooks.RotateSecret(ctx, &webhookpb.RotateWebhookSecretRequest{Name: d.hook.GetName()})
	if !c.NoErr(err, "RotateWebhookSecret") {
		return
	}
	c.Expect(rotated.GetSecret() != d.hook.GetSecret() && strings.HasPrefix(rotated.GetSecret(), "whsec_"), "a new whsec_ secret comes back")
	d.rx.trust(rotated.GetSecret())

	a := d.day.Add(14 * time.Hour)
	hold, err := d.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: d.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, a.Add(time.Hour)), CandidateResources: []string{d.room}},
	})
	if !c.NoErr(err, "CreateHold") {
		return
	}
	_, ok := d.rx.next(freebusy.EventHoldActive, hold.GetName(), wait)
	c.Expect(ok, "the next event verifies under the new secret alone")
	_, err = d.e.Holds.Release(ctx, &allocationpb.ReleaseHoldRequest{Name: hold.GetName()})
	if c.NoErr(err, "ReleaseHold") {
		_, ok = d.rx.next(freebusy.EventHoldReleased, hold.GetName(), wait)
		c.Expect(ok, "%s arrives when the hold is given up", freebusy.EventHoldReleased)
	}
}

// pause: changes made while paused are not sent, even after resuming.
func (d *desk) pause(ctx context.Context) {
	c := d.c
	c.Step("The desk pauses the webhook for maintenance")
	paused, err := d.e.Webhooks.Pause(ctx, &webhookpb.PauseWebhookRequest{Name: d.hook.GetName()})
	if !c.NoErr(err, "PauseWebhook") {
		return
	}
	c.Expect(paused.GetState() == webhookpb.WebhookState_WEBHOOK_STATE_PAUSED, "webhook is %s", paused.GetState())
	a := d.day.Add(16 * time.Hour)
	if _, err := d.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: d.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, a.Add(time.Hour)), CandidateResources: []string{d.room}},
	}); !c.NoErr(err, "CreateHold while paused") {
		return
	}
	r, quiet := d.rx.quiet(15 * time.Second)
	c.Expect(quiet, "a hold taken while paused is not sent (got %v)", r.event)

	resumed, err := d.e.Webhooks.Resume(ctx, &webhookpb.ResumeWebhookRequest{Name: d.hook.GetName()})
	if !c.NoErr(err, "ResumeWebhook") {
		return
	}
	c.Expect(resumed.GetState() == webhookpb.WebhookState_WEBHOOK_STATE_ACTIVE, "resumed: %s, sending changes from now on", resumed.GetState())
	a = d.day.Add(18 * time.Hour)
	hold, err := d.e.Holds.New(ctx, &allocationpb.CreateHoldRequest{
		Parent: d.site, Hold: &allocationpb.Hold{Window: freebusy.Window(a, a.Add(time.Hour)), CandidateResources: []string{d.room}},
	})
	if c.NoErr(err, "CreateHold after resuming") {
		_, ok := d.rx.next(freebusy.EventHoldActive, hold.GetName(), wait)
		c.Expect(ok, "the next hold is sent again")
	}
}

func (d *desk) cleanup(ctx context.Context) {
	c := d.c
	c.Step("The desk unsubscribes")
	err := d.e.Webhooks.Delete(ctx, &webhookpb.DeleteWebhookRequest{Name: d.hook.GetName(), Force: true})
	c.Expect(err == nil, "DeleteWebhook with force removes it and its deliveries")
	d.rx.mu.Lock()
	c.Say("the receiver turned away %d unsigned request(s)", d.rx.rejected)
	d.rx.mu.Unlock()
}
