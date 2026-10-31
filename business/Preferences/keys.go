package business

// The person-wide keys (agreed with CHAT-LEAD, 18 Oct): a person's zone, the
// day their week starts and their clock. A surface's own keys are registered
// by that surface (calendar.working_hours in business/Calendar; chat's in A7).

import (
	"encoding/json"
	"time"
)

func init() {
	// An IANA zone; none until the person confirms one (the web then asks,
	// in one line, with the browser's zone as its suggestion).
	Register("zone", Key{Default: json.RawMessage("null"), Check: func(v json.RawMessage) error {
		var z string
		if json.Unmarshal(v, &z) != nil {
			return &Refused{"Pick a time zone."}
		}
		return ZoneProblem(z)
	}})
	// The day a week starts: 0 Sunday to 6 Saturday.
	Register("week_start", Key{Default: json.RawMessage("1"), Check: func(v json.RawMessage) error {
		var d int
		if json.Unmarshal(v, &d) != nil || d < 0 || d > 6 {
			return &Refused{"A week starts on a day from Sunday (0) to Saturday (6)."}
		}
		return nil
	}})
	// The clock times are written in: the device's way, 12-hour or 24-hour.
	Register("clock", Key{Default: json.RawMessage(`"auto"`), Check: func(v json.RawMessage) error {
		var c string
		if json.Unmarshal(v, &c) != nil || (c != "auto" && c != "12" && c != "24") {
			return &Refused{`The clock is "auto", "12" or "24".`}
		}
		return nil
	}})
}

// ZoneProblem is why z can't be kept as a zone a person reads times in, or
// nil: an IANA zone this server knows. "Local", which Go reads as the
// server's own zone, is no zone a person's device can show. Pure.
func ZoneProblem(z string) error {
	if z == "" || len(z) > 64 {
		return &Refused{"Pick a time zone."}
	}
	if _, err := time.LoadLocation(z); err != nil || z == "Local" {
		return &Refused{"That isn't a time zone this server knows."}
	}
	return nil
}
