// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package freebusy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shokki-engineering/freebusy-go/pb/allocationpb"
	"github.com/shokki-engineering/freebusy-go/pb/reservationpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// WebhookEvent types a webhook can ask for in Webhook.EventTypes. Each but
// EventReservationUpdated is the state a record reached; the event's data is
// the whole record.
const (
	EventHoldActive    = "freebusy.hold.active"
	EventHoldReleased  = "freebusy.hold.released"
	EventHoldExpired   = "freebusy.hold.expired"
	EventHoldConverted = "freebusy.hold.converted"

	EventReservationPending      = "freebusy.reservation.pending"
	EventReservationConfirmed    = "freebusy.reservation.confirmed"
	EventReservationUpdated      = "freebusy.reservation.updated"
	EventReservationCancelling   = "freebusy.reservation.cancelling"
	EventReservationCancelled    = "freebusy.reservation.cancelled"
	EventReservationFailed       = "freebusy.reservation.failed"
	EventReservationRescheduling = "freebusy.reservation.rescheduling"
	EventReservationRescheduled  = "freebusy.reservation.rescheduled"

	EventResourceCreated     = "freebusy.resource.created"
	EventResourceUpdated     = "freebusy.resource.updated"
	EventResourceDeleted     = "freebusy.resource.deleted"
	EventCalendarCreated     = "freebusy.calendar.created"
	EventCalendarUpdated     = "freebusy.calendar.updated"
	EventCalendarDeleted     = "freebusy.calendar.deleted"
	EventEventCreated        = "freebusy.event.created"
	EventEventUpdated        = "freebusy.event.updated"
	EventEventDeleted        = "freebusy.event.deleted"
	EventAvailabilityCreated = "freebusy.availability.created"
	EventAvailabilityUpdated = "freebusy.availability.updated"
	EventAvailabilityDeleted = "freebusy.availability.deleted"
)

// WebhookTolerance is how far a delivery's webhook-timestamp may be from now
// before VerifyWebhook refuses it as a possible replay.
const WebhookTolerance = 5 * time.Minute

// ErrWebhookSignature is returned when a delivery's signature does not match
// any of the secrets given, or its timestamp is outside WebhookTolerance.
var ErrWebhookSignature = errors.New("freebusy: webhook signature does not verify")

// WebhookEvent is one webhook delivery: a CloudEvents 1.0 document whose Data is the
// changed record in its JSON form.
type WebhookEvent struct {
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"` // the record's name
	Time            time.Time       `json:"time"`
	SpecVersion     string          `json:"specversion"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

// Decode reads the event's data into m, such as a *Reservation or a *Hold.
// Fields this SDK does not know yet are skipped.
func (e *WebhookEvent) Decode(m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(e.Data, m)
}

// Reservation is the data of a freebusy.reservation.* event.
func (e *WebhookEvent) Reservation() (*reservationpb.Reservation, error) {
	r := &reservationpb.Reservation{}
	return r, e.Decode(r)
}

// Hold is the data of a freebusy.hold.* event.
func (e *WebhookEvent) Hold() (*allocationpb.Hold, error) {
	h := &allocationpb.Hold{}
	return h, e.Decode(h)
}

// ParseWebhook reads a delivery from r, verifies it with VerifyWebhook, and
// decodes it. secrets are the webhook's whsec_ secrets: during a rotation,
// pass the new one and the old one.
func ParseWebhook(r *http.Request, secrets ...string) (*WebhookEvent, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if err := VerifyWebhook(r.Header, body, secrets...); err != nil {
		return nil, err
	}
	var e WebhookEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("freebusy: webhook body: %w", err)
	}
	return &e, nil
}

// VerifyWebhook checks a delivery's Standard Webhooks signature
// (https://www.standardwebhooks.com): webhook-signature holds one or more
// "v1,<base64 HMAC-SHA256>" of "<webhook-id>.<webhook-timestamp>.<body>", and
// one must match under one of secrets. The timestamp must be within
// WebhookTolerance of now.
//
// Every secret is tried. One that is not valid base64 is skipped, so a bad
// secret configured beside a good one (mid-rotation, say) never blocks a
// delivery the good one verifies. When none verifies, the error is
// ErrWebhookSignature, joined with what was wrong with each bad secret.
func VerifyWebhook(header http.Header, body []byte, secrets ...string) error {
	id, ts := header.Get("webhook-id"), header.Get("webhook-timestamp")
	sent, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || err != nil {
		return ErrWebhookSignature
	}
	if d := time.Since(time.Unix(sent, 0)); d > WebhookTolerance || d < -WebhookTolerance {
		return ErrWebhookSignature
	}
	signed := []byte(id + "." + ts + ".")
	signed = append(signed, body...)
	var bad []error
	for i, secret := range secrets {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
		if err != nil {
			bad = append(bad, fmt.Errorf("freebusy: webhook secret #%d: %w", i, err))
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(signed)
		want := mac.Sum(nil)
		for sig := range strings.FieldsSeq(header.Get("webhook-signature")) {
			v, b64, ok := strings.Cut(sig, ",")
			if !ok || v != "v1" {
				continue
			}
			got, err := base64.StdEncoding.DecodeString(b64)
			if err == nil && hmac.Equal(got, want) {
				return nil
			}
		}
	}
	if len(bad) > 0 {
		return errors.Join(append([]error{ErrWebhookSignature}, bad...)...)
	}
	return ErrWebhookSignature
}
