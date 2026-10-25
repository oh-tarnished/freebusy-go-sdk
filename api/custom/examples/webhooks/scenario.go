// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package main

// scenario.go sets up what the scenario books — an organization, a site, a
// calendar with opening hours, resources — through the SDK's RFC services,
// and narrates and checks the scenario.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shokki-engineering/freebusy-go"
	"github.com/shokki-engineering/freebusy-go/pb/availabilitypb"
	"github.com/shokki-engineering/freebusy-go/pb/calendarpb"
	"github.com/shokki-engineering/freebusy-go/pb/organizationalunitpb"
	"github.com/shokki-engineering/freebusy-go/pb/organizationpb"
	"github.com/shokki-engineering/freebusy-go/pb/resourcepb"
	"google.golang.org/genproto/googleapis/type/datetime"
)

// profileSchemaURI names the STRUCTURED-DATA payload on a resource that
// freebusy reads its booking policy from.
const profileSchemaURI = "https://freebusy.shokki.engineering/schema/booking-profile"

// profile is the booking profile a resource carries under profileSchemaURI.
// Durations are RFC 5545 DURATION values such as "PT1H"; empty imposes nothing.
type profile struct {
	MaxOccupancy int32  `json:"max_occupancy"`
	MinDuration  string `json:"min_duration,omitempty"`
	MaxDuration  string `json:"max_duration,omitempty"`
	MinNotice    string `json:"min_notice,omitempty"`
	MaxAdvance   string `json:"max_advance,omitempty"`
}

// connect opens the client: FREEBUSY_ENDPOINT, FREEBUSY_INSECURE and
// FREEBUSY_CA_FILE configure it.
func connect(ctx context.Context) (*freebusy.Client, error) {
	c, err := freebusy.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("freebusy at %s\n", envOr("FREEBUSY_ENDPOINT", "freebusy.shokki.engineering:443"))
	return c, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// newSite creates an organization and one site under it.
func newSite(ctx context.Context, c *freebusy.Client, orgID, orgName, siteID, siteName string) (org, site string, err error) {
	o, err := c.Organizations.New(ctx, &organizationpb.CreateOrganizationRequest{
		OrganizationId: orgID,
		Organization:   &organizationpb.Organization{DisplayNames: []string{orgName}},
	})
	if err != nil {
		return "", "", fmt.Errorf("create organization: %w", err)
	}
	u, err := c.Sites.New(ctx, &organizationalunitpb.CreateOrganizationalUnitRequest{
		OrganizationalUnitId: siteID,
		OrganizationalUnit:   &organizationalunitpb.OrganizationalUnit{DisplayNames: []string{siteName}, Organization: o.GetName()},
	})
	if err != nil {
		return "", "", fmt.Errorf("create site: %w", err)
	}
	return o.GetName(), u.GetName(), nil
}

// newCalendar creates a calendar keeping time in zone.
func newCalendar(ctx context.Context, c *freebusy.Client, id, displayName, zone string) (string, error) {
	cal, err := c.Calendars.New(ctx, &calendarpb.CreateCalendarRequest{
		CalendarId: id,
		Calendar:   &calendarpb.Calendar{DisplayName: displayName, TimeZone: &datetime.TimeZone{Id: zone}},
	})
	if err != nil {
		return "", fmt.Errorf("create calendar: %w", err)
	}
	return cal.GetName(), nil
}

// openDaily publishes RFC 7953 availability on calendar: offered for open,
// from first and again every day at the same wall-clock time.
func openDaily(ctx context.Context, c *freebusy.Client, calendar, summary string, first time.Time, open time.Duration, period string) error {
	start := &availabilitypb.CalendarTime{Value: &availabilitypb.CalendarTime_DateTime{DateTime: local(first)}}
	_, err := c.Availabilities.New(ctx, &availabilitypb.CreateAvailabilityRequest{
		Parent: calendar,
		Availability: &availabilitypb.Availability{
			Summary: summary,
			Start:   start,
			AvailablePeriods: []*availabilitypb.AvailablePeriod{{
				Summary:        period,
				Start:          start,
				EndForm:        &availabilitypb.AvailablePeriod_Duration{Duration: freebusy.NewDuration(open)},
				RecurrenceRule: "FREQ=DAILY",
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("create availability: %w", err)
	}
	return nil
}

// resourceSpec is a bookable RFC 9073 resource to create with newResource.
type resourceSpec struct {
	ID          string
	DisplayName string
	Types       []string
	Site        string
	Calendar    string
	Profile     profile
	// Attributes, when set, are kept as JSON under AttributesSchema.
	Attributes       any
	AttributesSchema string
}

// newResource creates r and returns its name.
func newResource(ctx context.Context, c *freebusy.Client, r resourceSpec) (string, error) {
	p, err := json.Marshal(r.Profile)
	if err != nil {
		return "", err
	}
	data := []*resourcepb.StructuredData{
		{Schema: profileSchemaURI, FormatType: "application/json", Value: &resourcepb.StructuredData_Text{Text: string(p)}},
	}
	if r.Attributes != nil {
		attrs, err := json.Marshal(r.Attributes)
		if err != nil {
			return "", err
		}
		data = append(data, &resourcepb.StructuredData{Schema: r.AttributesSchema, FormatType: "application/json", Value: &resourcepb.StructuredData_Text{Text: string(attrs)}})
	}
	res, err := c.Resources.New(ctx, &resourcepb.CreateResourceRequest{
		ResourceId: r.ID,
		Resource: &resourcepb.Resource{
			DisplayName: r.DisplayName, ResourceTypes: r.Types,
			OrganizationalUnit: r.Site, Calendar: r.Calendar, StructuredData: data,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create resource %s: %w", r.ID, err)
	}
	return res.GetName(), nil
}

// suffix is a short id fragment unique to this run, so every run creates
// its own organization and the example can run again against one server.
func suffix() string {
	return strings.ToLower(fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffffff))
}

// local is t as a wall-clock date-time in its own IANA zone, how RFC 7953
// availability keeps "06:00" at 06:00 across daylight-saving changes.
func local(t time.Time) *datetime.DateTime {
	return &datetime.DateTime{
		Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day()),
		Hours: int32(t.Hour()), Minutes: int32(t.Minute()),
		TimeOffset: &datetime.DateTime_TimeZone{TimeZone: &datetime.TimeZone{Id: t.Location().String()}},
	}
}

// checker narrates a scenario and records every expectation that failed.
type checker struct {
	failures []string
	notes    []string
}

func (c *checker) Step(format string, args ...any) {
	fmt.Printf("\n== %s\n", fmt.Sprintf(format, args...))
}

func (c *checker) Say(format string, args ...any) {
	fmt.Printf("   %s\n", fmt.Sprintf(format, args...))
}

func (c *checker) Expect(cond bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if cond {
		fmt.Printf("   [ok]   %s\n", msg)
		return true
	}
	fmt.Printf("   [FAIL] %s\n", msg)
	c.failures = append(c.failures, msg)
	return false
}

func (c *checker) NoErr(err error, what string) bool {
	if err == nil {
		return true
	}
	return c.Expect(false, "%s: %v", what, err)
}

func (c *checker) Note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("   [note] %s\n", msg)
	c.notes = append(c.notes, msg)
}

func (c *checker) Must(err error, what string) {
	if err != nil {
		c.failures = append(c.failures, what+": "+err.Error())
		c.Finish()
	}
}

func (c *checker) Finish() {
	fmt.Println()
	if len(c.notes) > 0 {
		fmt.Printf("Notes (%d):\n", len(c.notes))
		for _, n := range c.notes {
			fmt.Printf("  - %s\n", n)
		}
	}
	if len(c.failures) > 0 {
		fmt.Printf("FAILED: %d expectation(s):\n", len(c.failures))
		for _, f := range c.failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("PASS: every expectation held.")
	os.Exit(0)
}
