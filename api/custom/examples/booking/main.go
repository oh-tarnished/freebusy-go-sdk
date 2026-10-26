// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

// Command booking runs freebusy end to end from an empty server.
//
// It sets up two meeting rooms open 08:00-18:00, blocks one room
// for maintenance from a calendar, and checks that the engine sees the block.
// Then it books three customers into the same hour, so the third is moved to
// the next free hour, and receives the bookings over a webhook. Finally it
// reschedules one booking and cancels another.
//
//	FREEBUSY_ENDPOINT=localhost:50051 FREEBUSY_INSECURE=true go run ./examples/booking
//
// The maintenance block is written over CalDAV when FREEBUSY_CALDAV_URL is
// set (http://localhost:8080/dav, with [caldav] enabled on the server), and
// through the Events API otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/availabilitypb"
	"github.com/shokki-engineering/freebusy-go/pb/calendarpb"
	"github.com/shokki-engineering/freebusy-go/pb/eventpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
	"github.com/shokki-engineering/freebusy-go/pb/resourcepb"
	"github.com/shokki-engineering/freebusy-go/pb/webhookpb"
	"google.golang.org/genproto/googleapis/type/datetime"
)

const zone = "Europe/London"

// room is a bookable meeting room and the calendar it keeps its hours on.
type room struct {
	name     string
	calendar string
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("done")
}

func run(ctx context.Context) error {
	client, err := freebusy.NewClient(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	loc, err := time.LoadLocation(zone)
	if err != nil {
		return err
	}
	next := time.Now().In(loc).AddDate(0, 0, 7)
	day := time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
	at := func(hour int) time.Time { return day.Add(time.Duration(hour) * time.Hour) }
	run := fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffff)

	var rooms []room
	for _, id := range []string{"oak", "elm"} {
		r, err := createRoom(ctx, client, id+"-"+run, at(8))
		if err != nil {
			return err
		}
		rooms = append(rooms, r)
	}
	fmt.Printf("rooms %s and %s, open 08:00-18:00\n", rooms[0].name, rooms[1].name)

	hooks, err := listen()
	if err != nil {
		return err
	}
	defer hooks.Close()
	hook, err := client.Webhooks.New(ctx, &webhookpb.CreateWebhookRequest{
		WebhookId: "booking-" + run,
		Webhook: &webhookpb.Webhook{
			Uri:        hooks.url,
			EventTypes: []string{freebusy.EventReservationConfirmed, freebusy.EventReservationCancelled},
		},
	})
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	hooks.setSecret(hook.GetSecret())
	defer func() {
		if err := client.Webhooks.Delete(ctx, &webhookpb.DeleteWebhookRequest{Name: hook.GetName(), Force: true}); err != nil {
			log.Printf("delete webhook: %v", err)
		}
	}()

	// Maintenance on the first room, 12:00-13:00, from its calendar.
	if err := blockOut(ctx, client, rooms[0].calendar, "Maintenance", at(12), at(13)); err != nil {
		return err
	}
	check, err := client.Availability.CheckAvailability(ctx, &allocationpb.CheckAvailabilityRequest{
		Resources: []string{rooms[0].name}, Window: freebusy.Window(at(12), at(13)), Quantity: 1,
	})
	if err != nil {
		return fmt.Errorf("check availability: %w", err)
	}
	if check.GetBookable() {
		return fmt.Errorf("%s is bookable during its maintenance", rooms[0].name)
	}
	fb, err := client.Calendars.QueryFreeBusy(ctx, &calendarpb.QueryFreeBusyRequest{
		Name: rooms[0].calendar, Window: freebusy.Window(at(8), at(18)),
	})
	if err != nil {
		return fmt.Errorf("query free/busy: %w", err)
	}
	for _, p := range fb.GetPeriods() {
		fmt.Printf("%s: %s %s-%s\n", rooms[0].calendar, p.GetBusyType(),
			p.GetPeriod().GetStartTime().AsTime().In(loc).Format("15:04"), p.GetPeriod().GetEndTime().AsTime().In(loc).Format("15:04"))
	}

	// Three customers want 10:00 and there are two rooms.
	var booked []*reservationpb.Reservation
	for _, customer := range []string{"dana", "lee", "sam"} {
		res, err := book(ctx, client, "users/"+customer+"-"+run, at(10), at(18), time.Hour)
		if err != nil {
			return err
		}
		booked = append(booked, res)
		roomID, err := resourcepb.ParseResourceName(res.GetResources()[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s booked %s %s-%s\n", res.GetCustomer(), roomID,
			res.GetStartTime().AsTime().In(loc).Format("15:04"), res.GetEndTime().AsTime().In(loc).Format("15:04"))
	}
	for _, res := range booked {
		if err := hooks.wait(freebusy.EventReservationConfirmed, res.GetName(), 30*time.Second); err != nil {
			return err
		}
	}
	fmt.Printf("webhook received %d confirmations\n", len(booked))

	moved, err := reschedule(ctx, client, booked[0], at(15), at(16))
	if err != nil {
		return err
	}
	fmt.Printf("%s moved to %s-%s\n", moved.GetCustomer(),
		moved.GetStartTime().AsTime().In(loc).Format("15:04"), moved.GetEndTime().AsTime().In(loc).Format("15:04"))

	cancelled, err := client.Reservations.Cancel(ctx, &reservationpb.CancelReservationRequest{
		Name: booked[1].GetName(), Reason: "customer asked", Etag: booked[1].GetEtag(),
	})
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if err := hooks.wait(freebusy.EventReservationCancelled, cancelled.GetName(), 30*time.Second); err != nil {
		return err
	}
	fmt.Printf("%s cancelled\n", cancelled.GetCustomer())
	return nil
}

// book reserves one meeting room for customer for length, at the first hour
// from start (and before end) that any room is free.
func book(ctx context.Context, client *freebusy.Client, customer string, start, end time.Time, length time.Duration) (*reservationpb.Reservation, error) {
	for from := start; !from.Add(length).After(end); from = from.Add(time.Hour) {
		window := freebusy.Window(from, from.Add(length))
		var free []string
		for m, err := range client.Availability.SearchAvailabilityAutoPaging(ctx, &allocationpb.SearchAvailabilityRequest{
			Window: window, ResourceTypes: []string{"meeting-room"},
		}).All() {
			if err != nil {
				return nil, fmt.Errorf("search: %w", err)
			}
			free = append(free, m.GetResource())
		}
		if len(free) == 0 {
			continue
		}
		hold, err := client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
			Hold: &allocationpb.Hold{
				Window: window, Quantity: 1, CandidateResources: free,
				// A retry after a lost answer gets this hold back instead of
				// taking a second room.
				IdempotencyKey: customer + "-" + from.Format("1504"),
			},
		})
		switch {
		case errors.Is(err, freebusy.ErrCapacityExhausted):
			continue // taken since the search: try the next hour
		case err != nil:
			return nil, fmt.Errorf("hold: %w", err)
		}
		res, err := client.Reservations.New(ctx, &reservationpb.CreateReservationRequest{
			Reservation: &reservationpb.Reservation{Hold: hold.GetName(), Customer: customer},
		})
		switch {
		case errors.Is(err, freebusy.ErrHoldNotActive):
			continue // the hold lapsed before it was confirmed: start over
		case err != nil:
			return nil, fmt.Errorf("reserve: %w", err)
		}
		return res, nil
	}
	return nil, fmt.Errorf("no room free for %s between %s and %s", customer, start.Format("15:04"), end.Format("15:04"))
}

// reschedule moves res to [start, end) in the same room. The original keeps
// its room until the move is confirmed.
func reschedule(ctx context.Context, client *freebusy.Client, res *reservationpb.Reservation, start, end time.Time) (*reservationpb.Reservation, error) {
	for attempt := 0; attempt < 2; attempt++ {
		hold, err := client.Holds.New(ctx, &allocationpb.CreateHoldRequest{
			Hold: &allocationpb.Hold{
				Window: freebusy.Window(start, end), Quantity: 1,
				CandidateResources: res.GetResources(), Replaces: res.GetHold(),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("hold for the move: %w", err)
		}
		moved, err := client.Reservations.Reschedule(ctx, &reservationpb.RescheduleReservationRequest{
			Name: res.GetName(), Hold: hold.GetName(),
		})
		switch {
		case errors.Is(err, freebusy.ErrReplacementFailed):
			continue // the new hold lapsed; the booking kept its slot: try again
		case err != nil:
			return nil, fmt.Errorf("reschedule: %w", err)
		}
		return moved, nil
	}
	return nil, fmt.Errorf("could not move %s", res.GetName())
}

// createRoom creates a meeting room with its own calendar, open 08:00-18:00
// every day from opening.
func createRoom(ctx context.Context, client *freebusy.Client, id string, opening time.Time) (room, error) {
	cal, err := client.Calendars.New(ctx, &calendarpb.CreateCalendarRequest{
		CalendarId: id,
		Calendar:   &calendarpb.Calendar{DisplayName: "Room " + id, TimeZone: &datetime.TimeZone{Id: zone}},
	})
	if err != nil {
		return room{}, fmt.Errorf("create calendar: %w", err)
	}
	start := &availabilitypb.CalendarTime{Value: &availabilitypb.CalendarTime_DateTime{DateTime: wallClock(opening)}}
	_, err = client.Availabilities.New(ctx, &availabilitypb.CreateAvailabilityRequest{
		Parent: cal.GetName(),
		Availability: &availabilitypb.Availability{
			Summary: "Opening hours",
			Start:   start,
			AvailablePeriods: []*availabilitypb.AvailablePeriod{{
				Start:          start,
				EndForm:        &availabilitypb.AvailablePeriod_Duration{Duration: freebusy.NewDuration(10 * time.Hour)},
				RecurrenceRule: "FREQ=DAILY",
			}},
		},
	})
	if err != nil {
		return room{}, fmt.Errorf("publish opening hours: %w", err)
	}
	// freebusy reads a resource's booking policy from this structured data.
	policy, err := json.Marshal(map[string]any{"max_occupancy": 1, "min_duration": "PT30M"})
	if err != nil {
		return room{}, err
	}
	r, err := client.Resources.New(ctx, &resourcepb.CreateResourceRequest{
		ResourceId: id,
		Resource: &resourcepb.Resource{
			DisplayName: "Room " + id, ResourceTypes: []string{"meeting-room"},
			Calendar: cal.GetName(),
			StructuredData: []*resourcepb.StructuredData{{
				Schema:     "https://freebusy.shokki.engineering/schema/booking-profile",
				FormatType: "application/json",
				Value:      &resourcepb.StructuredData_Text{Text: string(policy)},
			}},
		},
	})
	if err != nil {
		return room{}, fmt.Errorf("create room: %w", err)
	}
	return room{name: r.GetName(), calendar: cal.GetName()}, nil
}

// blockOut puts a busy event on calendar for [start, end): over CalDAV when
// FREEBUSY_CALDAV_URL is set, through the Events API otherwise.
func blockOut(ctx context.Context, client *freebusy.Client, calendar, summary string, start, end time.Time) error {
	if base := os.Getenv("FREEBUSY_CALDAV_URL"); base != "" {
		id, err := calendarpb.ParseCalendarName(calendar)
		if err != nil {
			return err
		}
		uid := fmt.Sprintf("block-%d", start.Unix())
		ics := strings.Join([]string{
			"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//freebusy-go//booking example//EN",
			"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:" + time.Now().UTC().Format("20060102T150405Z"),
			"DTSTART:" + start.UTC().Format("20060102T150405Z"), "DTEND:" + end.UTC().Format("20060102T150405Z"),
			"SUMMARY:" + summary, "TRANSP:OPAQUE", "END:VEVENT", "END:VCALENDAR", "",
		}, "\r\n")
		url := strings.TrimSuffix(base, "/") + "/calendars/" + id + "/" + uid + ".ics"
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(ics))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("caldav: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("caldav: PUT %s: %s: %s", url, resp.Status, body)
		}
		fmt.Printf("CalDAV: %s on %s\n", summary, url)
		return nil
	}
	_, err := client.Events.New(ctx, &eventpb.CreateEventRequest{
		Parent: calendar,
		Event: &eventpb.Event{
			Summary:      summary,
			Start:        &eventpb.CalendarTime{Value: &eventpb.CalendarTime_DateTime{DateTime: wallClock(start)}},
			Ends:         &eventpb.Event_End{End: &eventpb.CalendarTime{Value: &eventpb.CalendarTime_DateTime{DateTime: wallClock(end)}}},
			Transparency: eventpb.Transparency_TRANSPARENCY_OPAQUE,
		},
	})
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	fmt.Printf("Events API: %s on %s\n", summary, calendar)
	return nil
}

// wallClock is t as a date-time in its own IANA zone, so 08:00 stays 08:00
// across daylight-saving changes.
func wallClock(t time.Time) *datetime.DateTime {
	return &datetime.DateTime{
		Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day()),
		Hours: int32(t.Hour()), Minutes: int32(t.Minute()),
		TimeOffset: &datetime.DateTime_TimeZone{TimeZone: &datetime.TimeZone{Id: t.Location().String()}},
	}
}

// receiver is the webhook endpoint: it verifies each delivery's signature
// and keeps the events.
type receiver struct {
	srv *http.Server
	url string

	mu     sync.Mutex
	secret string
	events []*freebusy.WebhookEvent
}

func listen() (*receiver, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &receiver{url: "http://" + l.Addr().String() + "/hooks"}
	r.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: r}
	go func() {
		if err := r.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("webhook receiver: %v", err)
		}
	}()
	return r, nil
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	secret := r.secret
	r.mu.Unlock()
	ev, err := freebusy.ParseWebhook(req, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// setSecret is the webhook's signing secret, which deliveries are verified
// with.
func (r *receiver) setSecret(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secret = secret
}

// wait returns once an event of type typ about subject has arrived.
func (r *receiver) wait(typ, subject string, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		r.mu.Lock()
		for _, ev := range r.events {
			if ev.Type == typ && ev.Subject == subject {
				r.mu.Unlock()
				return nil
			}
		}
		r.mu.Unlock()
	}
	return fmt.Errorf("no %s for %s within %s", typ, subject, timeout)
}

func (r *receiver) Close() { _ = r.srv.Close() }
