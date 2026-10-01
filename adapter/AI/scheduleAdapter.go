package adapter

// DTOs for AI scheduling (Wave 3, Requirement 4): "find a time that works for
// these people" — the assistant proposes candidate meeting times by reasoning
// over participants' free/busy windows, then (on confirm) creates a real
// calendar event AS the requester.
//
// Privacy: the propose path reads only participants' FREE/BUSY intervals
// (start/end), never event titles or descriptions, and the response exposes
// only aggregate availability per slot (how many of the invitees are free) —
// never another person's event contents.

// ScheduleProposeRequest asks for candidate times for a meeting.
type ScheduleProposeRequest struct {
	Title         string   `json:"title"`
	Participants  []string `json:"participants"`    // names or user UUIDs (requester is always included)
	DurationMins  int      `json:"duration_mins"`   // meeting length; default 30, max 480
	WindowDays    int      `json:"window_days"`     // how far ahead to search; default 7, max 30
	UTCOffsetMins int      `json:"utc_offset_mins"` // requester's local offset (local = UTC + offset)
	BusinessStart int      `json:"business_start"`  // local start hour [0-23]; default 9
	BusinessEnd   int      `json:"business_end"`    // local end hour (exclusive) [1-24]; default 18
}

// ScheduleParticipant is one resolved invitee (for display + confirm).
type ScheduleParticipant struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// ScheduleCandidate is one proposed time slot with aggregate availability.
type ScheduleCandidate struct {
	Start     string `json:"start"`      // RFC3339 (UTC)
	End       string `json:"end"`        // RFC3339 (UTC)
	AllFree   bool   `json:"all_free"`   // every invitee is free
	FreeCount int    `json:"free_count"` // how many invitees are free
	Total     int    `json:"total"`      // total invitees considered
}

// ScheduleProposeResponse powers the "find a time" picker. Enabled=false → AI
// is off, hide the surface.
type ScheduleProposeResponse struct {
	Enabled      bool                  `json:"enabled"`
	DurationMins int                   `json:"duration_mins"`
	Candidates   []ScheduleCandidate   `json:"candidates"`
	Participants []ScheduleParticipant `json:"participants"`
	Unresolved   []string              `json:"unresolved"` // names that matched no (single) user
	Note         string                `json:"note,omitempty"`
}

// ScheduleConfirmRequest creates the chosen meeting.
type ScheduleConfirmRequest struct {
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Start            string   `json:"start"` // RFC3339
	End              string   `json:"end"`   // RFC3339
	ParticipantUUIDs []string `json:"participant_uuids"`
	SyncToGoogle     bool     `json:"sync_to_google"`
}

// ScheduleConfirmResponse returns the created event.
type ScheduleConfirmResponse struct {
	EventUUID string `json:"event_uuid"`
	Title     string `json:"title"`
	Start     string `json:"start"`
	End       string `json:"end"`
}

// ── Reschedule (calendar intelligence) ─────────────────────────────────────

// ScheduleRescheduleRequest asks for better times to move an existing event to.
// The event's current duration + participants are reused; only the window /
// local business hours are tunable.
type ScheduleRescheduleRequest struct {
	EventUUID     string `json:"event_uuid"`
	WindowDays    int    `json:"window_days"`
	UTCOffsetMins int    `json:"utc_offset_mins"`
	BusinessStart int    `json:"business_start"`
	BusinessEnd   int    `json:"business_end"`
}

// ScheduleRescheduleResponse reports who currently has a conflict at the event's
// time and proposes alternative slots (aggregate availability only).
type ScheduleRescheduleResponse struct {
	Enabled       bool                  `json:"enabled"`
	EventUUID     string                `json:"event_uuid"`
	Title         string                `json:"title"`
	CurrentStart  string                `json:"current_start"`
	CurrentEnd    string                `json:"current_end"`
	DurationMins  int                   `json:"duration_mins"`
	Participants  []ScheduleParticipant `json:"participants"`
	ConflictCount int                   `json:"conflict_count"` // invitees busy at the CURRENT time
	Conflicts     []string              `json:"conflicts"`      // their names
	Candidates    []ScheduleCandidate   `json:"candidates"`     // proposed alternatives
	Note          string                `json:"note,omitempty"`
}

// ScheduleConfirmRescheduleRequest moves the event to the chosen slot.
type ScheduleConfirmRescheduleRequest struct {
	EventUUID    string `json:"event_uuid"`
	Start        string `json:"start"` // RFC3339
	End          string `json:"end"`   // RFC3339
	SyncToGoogle bool   `json:"sync_to_google"`
}

// ── Pre-meeting prep brief (calendar intelligence) ─────────────────────────

// MeetingPrepRequest asks for an AI prep brief for an upcoming event. The
// event's own title/description/participants supply the topic; relevant
// workspace context is gathered server-side (permission-scoped).
type MeetingPrepRequest struct {
	EventUUID string `json:"event_uuid"`
}

// MeetingPrepResponse returns a concise, markdown prep brief for the meeting.
// Enabled=false → AI is off, hide the surface. The brief is grounded only in
// content the requester can already see; it never leaks inaccessible material.
type MeetingPrepResponse struct {
	Enabled   bool   `json:"enabled"`
	EventUUID string `json:"event_uuid"`
	Title     string `json:"title"`
	Brief     string `json:"brief"`          // markdown
	Note      string `json:"note,omitempty"` // shown when no brief could be produced
}
