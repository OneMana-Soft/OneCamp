package Calendar

// The calendar's own preferences (calendar A3a; design #37): a person's
// working hours, each weekday's ranges in their zone (the person-wide
// `zone`), the length a new meeting starts at, and the other zones their
// day and week show beside their own. No hours until they set some: until
// then any time is theirs to be asked for, as before.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	"github.com/google/uuid"
)

const (
	PrefWorkingHours  = "calendar.working_hours"
	PrefDefaultLength = "calendar.default_length"
	// PrefZones is the zones a person's day and week show beside their own
	// (calendar head 6, gcal-3), each with its label: [{zone, label}].
	PrefZones = "calendar.zones"
)

// MaxOtherZones is how many zones a person keeps beside their own: three
// columns with theirs, as Google's day and week show.
const MaxOtherZones = 2

// maxZoneLabel is the longest label a zone's column takes, in characters.
const maxZoneLabel = 24

// OtherZone is a zone a person's calendar shows beside their own, with the
// label its column says (its city when none).
type OtherZone struct {
	Zone  string `json:"zone"`
	Label string `json:"label,omitempty"`
}

// zonesProblem is why a list of other zones can't be kept, or nil: up to
// MaxOtherZones zones this server knows, each once, each label short. Pure.
func zonesProblem(v json.RawMessage) error {
	var zones []OtherZone
	if json.Unmarshal(v, &zones) != nil {
		return &preferencesBusiness.Refused{Msg: "Other time zones are a list of zones, each with an optional label."}
	}
	if len(zones) > MaxOtherZones {
		return &preferencesBusiness.Refused{Msg: fmt.Sprintf("Show up to %d other time zones.", MaxOtherZones)}
	}
	seen := map[string]bool{}
	for _, z := range zones {
		if err := preferencesBusiness.ZoneProblem(z.Zone); err != nil {
			return err
		}
		if seen[z.Zone] {
			return &preferencesBusiness.Refused{Msg: "Each time zone once."}
		}
		seen[z.Zone] = true
		if utf8.RuneCountInString(z.Label) > maxZoneLabel {
			return &preferencesBusiness.Refused{Msg: fmt.Sprintf("Keep a label to %d characters.", maxZoneLabel)}
		}
	}
	return nil
}

func init() {
	preferencesBusiness.Register(PrefWorkingHours, preferencesBusiness.Key{Default: json.RawMessage("null"), Check: func(v json.RawMessage) error {
		var week map[string][]Span
		if json.Unmarshal(v, &week) != nil {
			return &preferencesBusiness.Refused{Msg: "Hours look like 09:00 to 17:00, for each day."}
		}
		for _, spans := range week {
			if len(spans) > 6 {
				return &preferencesBusiness.Refused{Msg: "Up to six ranges a day."}
			}
		}
		if _, err := (WorkingHours{TZ: "UTC", Week: week}).Check(); err != nil {
			return &preferencesBusiness.Refused{Msg: err.Error()}
		}
		return nil
	}})
	preferencesBusiness.Register(PrefDefaultLength, preferencesBusiness.Key{Default: json.RawMessage("60"), Check: func(v json.RawMessage) error {
		var m int
		if json.Unmarshal(v, &m) != nil || m < 15 || m > 8*60 || m%5 != 0 {
			return &preferencesBusiness.Refused{Msg: "A meeting runs from 15 minutes to 8 hours, in steps of five minutes."}
		}
		return nil
	}})
	preferencesBusiness.Register(PrefZones, preferencesBusiness.Key{Default: json.RawMessage("[]"), Check: zonesProblem})
}

// HoursOf is each of these people's working hours in their zone, for those
// who set both (the others have none: any time is in hours), in two reads.
func HoursOf(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]WorkingHours, error) {
	weeks, err := preferencesBusiness.OfPeople(ctx, people, PrefWorkingHours)
	if err != nil {
		return nil, err
	}
	zones, err := preferencesBusiness.OfPeople(ctx, people, "zone")
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]WorkingHours{}
	for p, raw := range weeks {
		var week map[string][]Span
		var zone string
		if json.Unmarshal(raw, &week) != nil || json.Unmarshal(zones[p], &zone) != nil || zone == "" {
			continue
		}
		h := WorkingHours{TZ: zone, Week: week}
		if _, err := h.Check(); err == nil {
			out[p] = h
		}
	}
	return out, nil
}

// hoursOfPeople is HoursOf; swapped in tests.
var hoursOfPeople = HoursOf

// HoursSeenBy is HoursOf for the people the asker may see when they're busy
// (PeopleSeenBy), keyed by their ids: someone's hours tell when their day is,
// so a workspace that keeps busy times private keeps these too. Private is
// who was left out, unread.
func HoursSeenBy(ctx context.Context, askerUUID string, people []uuid.UUID) (map[string]WorkingHours, []string, error) {
	asked := make([]string, len(people))
	for i, p := range people {
		asked[i] = p.String()
	}
	seen, private, err := PeopleSeenBy(ctx, askerUUID, asked)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]uuid.UUID, len(seen))
	for i, p := range seen {
		ids[i] = uuid.MustParse(p)
	}
	hours, err := hoursOfPeople(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]WorkingHours, len(hours))
	for id, h := range hours {
		out[id.String()] = h
	}
	return out, private, nil
}

// ReaderClock is the zone and the clock a person reads times in: their kept
// zone (UTC without one) and whether they keep a 12-hour clock ("auto" reads
// as 24-hour on the server, which has no device to ask).
func ReaderClock(ctx context.Context, person uuid.UUID) (*time.Location, bool) {
	loc := time.UTC
	zones, err := preferencesBusiness.OfPeople(ctx, []uuid.UUID{person}, "zone")
	if err == nil {
		var z string
		if json.Unmarshal(zones[person], &z) == nil {
			if l, err := time.LoadLocation(z); err == nil {
				loc = l
			}
		}
	}
	clocks, err := preferencesBusiness.OfPeople(ctx, []uuid.UUID{person}, "clock")
	var c string
	twelve := err == nil && json.Unmarshal(clocks[person], &c) == nil && c == "12"
	return loc, twelve
}
