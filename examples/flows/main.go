// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

// Command flows runs the flows freebusy documents in its API (docs/flows in
// the freebusy repository) through the SDK, one file per flow:
//
//	book.go              book               search, check, hold, confirm
//	hold_expiry.go       hold-expiry        a hold nobody confirms lapses
//	reschedule.go        reschedule         move a booking onto a new hold
//	cancel.go            cancel             cancel, and cancel again
//	webhook_delivery.go  webhook-delivery   a failed delivery, retried by hand
//
// Each step is commented with the flow step it is. Run one flow by its id,
// or all of them:
//
//	FREEBUSY_ENDPOINT=localhost:50051 FREEBUSY_INSECURE=true go run ./examples/flows book
//	FREEBUSY_ENDPOINT=localhost:50051 FREEBUSY_INSECURE=true go run ./examples/flows
//
// hold-expiry waits for the server's hold sweeper, which runs once a minute,
// so it takes up to a minute and a half. webhook-delivery is quickest
// against a server with [webhooks] max_attempts = 1, where one failed attempt
// makes a delivery FAILED; with more attempts it retries the delivery while
// it is still being retried with backoff.
//
// It exits non-zero if any expectation fails.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/webhookpb"
)

const zone = "Europe/London"

// flow is one documented flow: its id in docs/flows, and the function that
// runs it.
type flow struct {
	id  string
	run func(context.Context, *env)
}

var flows = []flow{
	{"book", book},
	{"hold-expiry", holdExpiry},
	{"reschedule", reschedule},
	{"cancel", cancel},
	{"webhook-delivery", webhookDelivery},
}

// env is what every flow books against: one organization and site, a
// calendar open 08:00-18:00 every day, a webhook receiver, and the day the
// flows book on, a week from now.
type env struct {
	*checker
	client   *freebusy.Client
	sfx      string
	org      string
	site     string
	calendar string
	day      time.Time
	hooks    *receiver
}

func main() {
	ctx := context.Background()
	c := &checker{}
	want := map[string]bool{}
	for _, id := range os.Args[1:] {
		known := false
		for _, f := range flows {
			known = known || f.id == id
		}
		if !known {
			fmt.Fprintf(os.Stderr, "unknown flow %q; flows: book, hold-expiry, reschedule, cancel, webhook-delivery\n", id)
			os.Exit(2)
		}
		want[id] = true
	}

	client, err := connect(ctx)
	c.Must(err, "connect")
	defer func() { _ = client.Close() }()
	e := &env{checker: c, client: client, sfx: suffix()}

	c.Step("Setup: an organization, a site, opening hours and a webhook receiver")
	loc, err := time.LoadLocation(zone)
	c.Must(err, "load zone")
	e.org, e.site, err = newSite(ctx, client, "flows-"+e.sfx, "Flows", "office-"+e.sfx, "Office")
	c.Must(err, "create site")
	e.calendar, err = newCalendar(ctx, client, "office-hours-"+e.sfx, "Office hours", zone)
	c.Must(err, "create calendar")
	day := time.Now().In(loc).AddDate(0, 0, 7)
	e.day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	c.Must(openDaily(ctx, client, e.calendar, "Office hours", e.day.Add(8*time.Hour), 10*time.Hour, "08:00-18:00"), "open daily")
	e.hooks, err = listen()
	c.Must(err, "listen")
	defer e.hooks.Close()
	c.Say("site %s, receiver at %s", e.site, e.hooks.URL(""))

	for _, f := range flows {
		if len(want) == 0 || want[f.id] {
			f.run(ctx, e)
		}
	}
	c.Finish()
}

// room creates a meeting room at the site for one flow, so flows never book
// each other's rooms.
func (e *env) room(ctx context.Context, id string) string {
	name, err := newResource(ctx, e.client, resourceSpec{
		ID: id + "-" + e.sfx, DisplayName: "Room " + id, Types: []string{"meeting-room"},
		Site: e.site, Calendar: e.calendar, Profile: profile{MaxOccupancy: 1, MinDuration: "PT30M"},
	})
	e.Must(err, "create room "+id)
	return name
}

// subscribe creates a webhook for the site's events of the given types,
// delivered to path on the receiver, and returns it. The flow deletes it
// when done.
func (e *env) subscribe(ctx context.Context, id, path string, types ...string) *webhookpb.Webhook {
	hook, err := e.client.Webhooks.New(ctx, &webhookpb.CreateWebhookRequest{
		WebhookId: id + "-" + e.sfx,
		Webhook: &webhookpb.Webhook{
			Uri:                e.hooks.URL(path),
			EventTypes:         types,
			OrganizationalUnit: e.site,
		},
	})
	e.Must(err, "create webhook "+id)
	e.hooks.Secret(path, hook.GetSecret())
	return hook
}

// unsubscribe deletes a webhook and its deliveries.
func (e *env) unsubscribe(ctx context.Context, hook *webhookpb.Webhook) {
	e.NoErr(e.client.Webhooks.Delete(ctx, &webhookpb.DeleteWebhookRequest{Name: hook.GetName(), Force: true}), "delete webhook")
}

// at is hh:00 on the booking day.
func (e *env) at(hour int) time.Time { return e.day.Add(time.Duration(hour) * time.Hour) }

// receiver is a webhook endpoint for the flows: every verified delivery is
// kept, and a path can be told to fail.
type receiver struct {
	srv *http.Server
	url string

	mu      sync.Mutex
	secrets map[string]string // path -> signing secret
	failing map[string]bool   // path -> answer 500
	events  []received
}

type received struct {
	path  string
	event *freebusy.WebhookEvent
}

func listen() (*receiver, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &receiver{url: "http://" + l.Addr().String(), secrets: map[string]string{}, failing: map[string]bool{}}
	r.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: r}
	go func() { _ = r.srv.Serve(l) }()
	return r, nil
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	secret, fail := r.secrets[req.URL.Path], r.failing[req.URL.Path]
	r.mu.Unlock()
	if fail {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	// freebusy.ParseWebhook verifies the Standard Webhooks signature with
	// the webhook's secret and decodes the CloudEvent.
	ev, err := freebusy.ParseWebhook(req, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	r.events = append(r.events, received{path: req.URL.Path, event: ev})
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// URL is the receiver's URL for path.
func (r *receiver) URL(path string) string { return r.url + path }

// Secret records the signing secret deliveries to path are verified with.
func (r *receiver) Secret(path, secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[path] = secret
}

// Fail makes path answer 503 (fail true) or accept deliveries again.
func (r *receiver) Fail(path string, fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing[path] = fail
}

// Wait returns the first event of type typ about subject delivered to path,
// or nil after timeout.
func (r *receiver) Wait(path, typ, subject string, timeout time.Duration) *freebusy.WebhookEvent {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if evs := r.Find(path, typ, subject); len(evs) > 0 {
			return evs[0]
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

// Find returns every event of type typ about subject delivered to path.
func (r *receiver) Find(path, typ, subject string) []*freebusy.WebhookEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*freebusy.WebhookEvent
	for _, x := range r.events {
		if x.path == path && x.event.Type == typ && x.event.Subject == subject {
			out = append(out, x.event)
		}
	}
	return out
}

func (r *receiver) Close() { _ = r.srv.Close() }
