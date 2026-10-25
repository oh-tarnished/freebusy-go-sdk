// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package freebusy

import (
	"time"

	"google.golang.org/genproto/googleapis/type/interval"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Window is the interval [start, end), the google.type.Interval the API's
// windows are.
func Window(start, end time.Time) *interval.Interval {
	return &interval.Interval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}
}

// NewTimestamp is t as a protobuf Timestamp.
func NewTimestamp(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}

// NewDuration is d as a protobuf Duration.
func NewDuration(d time.Duration) *durationpb.Duration {
	return durationpb.New(d)
}

// NewAttributes is m as a Struct, for a reservation's attributes. Values may
// be nil, bool, numbers, strings, []any and map[string]any.
func NewAttributes(m map[string]any) (*structpb.Struct, error) {
	return structpb.NewStruct(m)
}

// NewUpdateMask names the fields an Update call changes, such as "attributes".
func NewUpdateMask(paths ...string) *fieldmaskpb.FieldMask {
	return &fieldmaskpb.FieldMask{Paths: paths}
}
