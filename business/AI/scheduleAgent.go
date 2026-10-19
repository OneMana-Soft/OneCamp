package business

// AI scheduling — Wave 3, Requirement 4. "Find a time that works for these
// people."
//
// ProposeSchedule resolves the named participants, reads each one's FREE/BUSY
// windows from the existing calendar model (no parallel store), and computes
// candidate meeting slots deterministically — business hours, weekdays, and
// the free/busy intersection. The slot math is intentionally deterministic
// rather than an LLM guess: scheduling must be correct, and a calendar
// intersection is exactly computable. The feature is gated by and surfaced
// through the AI assistant (it respects AI enablement) and reuses the AI
// service as its on/off chokepoint.
//
// ConfirmSchedule creates the chosen event AS the requester through the same
// business.CreateEvent path the calendar UI uses, so permissions, Google sync,
// and the data model are identical to a hand-created event.
//
// Privacy: only free/busy intervals (start/end) are read for other
// participants — never event titles/descriptions — and the response exposes
// only aggregate availability per slot, so scheduling never leaks what someone
// else's meetings are about.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	scheduleSlotStepMins    = 30
	scheduleMaxCandidates   = 6
	scheduleMaxParticipants = 20
	scheduleDefaultDuration = 30
	scheduleMaxDuration     = 480
	scheduleDefaultWindow   = 7
	scheduleMaxWindow       = 30
	scheduleDefaultBHStart  = 9
	scheduleDefaultBHEnd    = 18
)

// busyInterval is a single occupied [start,end) window (UTC). Pure value type.
type busyInterval struct {
	start time.Time
	end   time.Time
}

func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}

// readBusy is the calendar's one busy engine (business/Calendar.ReadBusy):
// OneCamp events and Google calendars, the same answer Find a time and
// booking pages give. A seam for tests.
var readBusy = calendarBusiness.ReadBusy

// gatherFreeBusy reads each user's busy windows in [from,to) through the
// calendar's engine, keeping only start and end, never an event's content.
// It read OneCamp events alone, never Google, and took a calendar it couldn't
// read for a free one. Now someone whose calendar can't be read, wholly or in
// part, is in unread: their time is unknown, and no slot counts them free.
// except, when set, is the event being moved, which is no clash with itself.
// Shared by propose and reschedule.
func gatherFreeBusy(ctx context.Context, userIDs []string, from, to time.Time, except *dgraphStruct.DgraphEvent) (map[string][]busyInterval, map[string]bool) {
	busyByUser := make(map[string][]busyInterval, len(userIDs))
	unread := map[string]bool{}
	for _, uid := range userIDs {
		b, err := readBusy(ctx, uid, from, to, calendarBusiness.BusyOptions{Except: except})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/AI/gatherFreeBusy: a calendar couldn't be read: %v", err)
			unread[uid] = true
			continue
		}
		if b.GoogleUnread {
			unread[uid] = true
		}
		intervals := make([]busyInterval, 0, len(b.Intervals))
		for _, iv := range b.Intervals {
			intervals = append(intervals, busyInterval{start: iv.Start.UTC(), end: iv.End.UTC()})
		}
		busyByUser[uid] = intervals
	}
	return busyByUser, unread
}

// scheduleZone is the zone business hours are read in: the request's IANA
// zone, else the one the assistant was told, else the request's fixed offset.
// An offset is wrong on the far side of a daylight-saving change, so it is
// only for an app that sends nothing better.
func scheduleZone(ctx context.Context, timezone string, utcOffsetMins int) *time.Location {
	if loc, ok := helpers.FirstLocation(timezone, ai.GetLocalization(ctx)["timezone"]); ok {
		return loc
	}
	return time.FixedZone("", utcOffsetMins*60)
}

// unreadNote says whose calendars couldn't be read, by the names the answer
// shows ("You" for the asker). Pure.
func unreadNote(names []string) string {
	var out []string
	for _, n := range names {
		if n == "You" {
			out = append(out, "Couldn't read your calendar, so no time counts as free for you.")
			continue
		}
		if n = strings.TrimSpace(n); n == "" {
			n = "Someone"
		}
		out = append(out, fmt.Sprintf("Couldn't read %s's calendar, so no time counts as free for %s.", n, n))
	}
	return strings.Join(out, " ")
}

// joinNotes joins the sentences an answer's note carries.
func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// ProposeSchedule produces candidate meeting times for the requester plus the
// named participants. Best-effort and permission-safe; never errors on a
// participant whose calendar can't be read: the note names them, and no slot
// counts them free.
func ProposeSchedule(ctx context.Context, userInfo *userModels.UserInfo, req adapter.ScheduleProposeRequest) (*adapter.ScheduleProposeResponse, error) {
	resp := &adapter.ScheduleProposeResponse{
		Enabled:      false,
		Candidates:   []adapter.ScheduleCandidate{},
		Participants: []adapter.ScheduleParticipant{},
		Unresolved:   []string{},
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	duration := req.DurationMins
	if duration <= 0 || duration > scheduleMaxDuration {
		duration = scheduleDefaultDuration
	}
	windowDays := req.WindowDays
	if windowDays <= 0 || windowDays > scheduleMaxWindow {
		windowDays = scheduleDefaultWindow
	}
	bhStart := req.BusinessStart
	if bhStart < 0 || bhStart > 23 {
		bhStart = scheduleDefaultBHStart
	}
	bhEnd := req.BusinessEnd
	if bhEnd <= bhStart || bhEnd > 24 {
		bhEnd = scheduleDefaultBHEnd
	}
	resp.DurationMins = duration

	// Resolve invitees. The requester is always included so the result is a
	// time that works for them too.
	requesterUUID := userInfo.UserDgraphInfo.Uuid
	userIDs := []string{requesterUUID}
	seen := map[string]bool{requesterUUID: true}
	resp.Participants = append(resp.Participants, adapter.ScheduleParticipant{UUID: requesterUUID, Name: "You"})

	for _, ref := range req.Participants {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if len(userIDs) >= scheduleMaxParticipants {
			break
		}
		u, err := resolveAssigneeRef(ctx, requesterUUID, ref, false)
		if err != nil || u == nil {
			resp.Unresolved = append(resp.Unresolved, ref)
			continue
		}
		if seen[u.Uuid] {
			continue
		}
		seen[u.Uuid] = true
		userIDs = append(userIDs, u.Uuid)
		name := strings.TrimSpace(u.UserName)
		if name == "" {
			name = strings.TrimSpace(u.UserFullName)
		}
		resp.Participants = append(resp.Participants, adapter.ScheduleParticipant{UUID: u.Uuid, Name: name})
	}

	proposeSlots(ctx, resp, userIDs, time.Now().UTC(), windowDays, duration, scheduleZone(ctx, req.Timezone, req.UTCOffsetMins), bhStart, bhEnd)
	return resp, nil
}

// proposeSlots fills an answer's candidates and note for the people in
// resp.Participants (userIDs, in the same order), from their busy times over
// the next windowDays.
func proposeSlots(ctx context.Context, resp *adapter.ScheduleProposeResponse, userIDs []string, now time.Time, windowDays, duration int, loc *time.Location, bhStart, bhEnd int) {
	busyByUser, unread := gatherFreeBusy(ctx, userIDs, now, now.AddDate(0, 0, windowDays), nil)

	resp.Candidates = computeCandidateSlots(now, windowDays, duration, loc, bhStart, bhEnd, scheduleMaxCandidates, busyByUser, unread, userIDs)
	noSlots := ""
	if len(resp.Candidates) == 0 {
		noSlots = fmt.Sprintf("No open slots in the next %d days during business hours. Try a longer window or a shorter meeting.", windowDays)
	}
	var unreadNames []string
	for _, p := range resp.Participants {
		if unread[p.UUID] {
			unreadNames = append(unreadNames, p.Name)
		}
	}
	resp.Note = joinNotes(noSlots, unreadNote(unreadNames))
}

// computeCandidateSlots is the pure scheduling core: it walks the search window
// in fixed steps, keeps slots that fall on a weekday within business hours on
// loc's wall clock, scores each by how many invitees are free, and returns the
// best (all-free first, then most-free, then earliest). UTC in, UTC out. Someone
// in unknown (a calendar that couldn't be read) is never counted free.
// Deterministic + unit tested.
func computeCandidateSlots(now time.Time, windowDays, durationMins int, loc *time.Location, bhStart, bhEnd, maxCandidates int, busyByUser map[string][]busyInterval, unknown map[string]bool, userIDs []string) []adapter.ScheduleCandidate {
	out := []adapter.ScheduleCandidate{}
	if len(userIDs) == 0 {
		return out
	}
	now = now.UTC()
	step := time.Duration(scheduleSlotStepMins) * time.Minute
	dur := time.Duration(durationMins) * time.Minute

	// Earliest start = next step boundary at/after now.
	start := now.Truncate(step)
	if start.Before(now) {
		start = start.Add(step)
	}
	horizon := now.AddDate(0, 0, windowDays)
	if loc == nil {
		loc = time.UTC
	}

	for t := start; !t.After(horizon); t = t.Add(step) {
		slotEnd := t.Add(dur)
		if slotEnd.After(horizon) {
			break
		}
		// The wall clock in the asker's zone, for the business-hours and
		// weekday gate: the zone's rules, so a daylight-saving change moves
		// nothing.
		local := t.In(loc)
		if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		// Whole slot must sit within local business hours on the start day.
		// Since bhEnd <= 24, start+duration <= bhEnd*60 also forbids crossing
		// local midnight, so no day-boundary special-casing is needed.
		startMin := local.Hour()*60 + local.Minute()
		if startMin < bhStart*60 || startMin+durationMins > bhEnd*60 {
			continue
		}

		freeCount := 0
		for _, uid := range userIDs {
			if unknown[uid] {
				continue
			}
			busy := false
			for _, iv := range busyByUser[uid] {
				if overlaps(t, slotEnd, iv.start, iv.end) {
					busy = true
					break
				}
			}
			if !busy {
				freeCount++
			}
		}
		out = append(out, adapter.ScheduleCandidate{
			Start:     t.Format(time.RFC3339),
			End:       slotEnd.Format(time.RFC3339),
			AllFree:   freeCount == len(userIDs),
			FreeCount: freeCount,
			Total:     len(userIDs),
		})
	}

	// Rank: all-free first, then most invitees free, then earliest.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AllFree != out[j].AllFree {
			return out[i].AllFree
		}
		if out[i].FreeCount != out[j].FreeCount {
			return out[i].FreeCount > out[j].FreeCount
		}
		return out[i].Start < out[j].Start
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// ConfirmSchedule creates the chosen meeting AS the requester through the
// normal calendar create path (permissions, Google sync, and data model are
// identical to a hand-created event).
func ConfirmSchedule(ctx context.Context, userInfo *userModels.UserInfo, req adapter.ScheduleConfirmRequest) (*adapter.ScheduleConfirmResponse, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	st, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Start))
	if err != nil {
		return nil, fmt.Errorf("invalid start time (expected RFC3339)")
	}
	et, err := time.Parse(time.RFC3339, strings.TrimSpace(req.End))
	if err != nil {
		return nil, fmt.Errorf("invalid end time (expected RFC3339)")
	}
	if !et.After(st) {
		return nil, fmt.Errorf("end must be after start")
	}

	input := calendarAdapter.CreateOrUpdateEventInput{
		Title:                title,
		Description:          strings.TrimSpace(req.Description),
		StartTime:            st.Format(time.RFC3339),
		EndTime:              et.Format(time.RFC3339),
		Participants:         req.ParticipantUUIDs,
		SyncToGoogleCalendar: req.SyncToGoogle,
	}
	out, cerr := calendarBusiness.CreateEvent(ctx, userInfo, input)
	if cerr != nil || out == nil {
		return nil, fmt.Errorf("failed to create the event")
	}
	return &adapter.ScheduleConfirmResponse{
		EventUUID: out.EventUuid,
		Title:     out.Title,
		Start:     req.Start,
		End:       req.End,
	}, nil
}

// RescheduleOptions reports who currently has a conflict at an event's time and
// proposes alternative slots that work across its participants. Read-only.
// Visible to the event's creator or a participant; only the creator may apply
// (ConfirmReschedule enforces that). Reuses the same free/busy + slot core as
// ProposeSchedule, excluding the event itself so its own slot isn't a conflict.
func RescheduleOptions(ctx context.Context, userInfo *userModels.UserInfo, req adapter.ScheduleRescheduleRequest) (*adapter.ScheduleRescheduleResponse, error) {
	resp := &adapter.ScheduleRescheduleResponse{
		Enabled:      false,
		EventUUID:    strings.TrimSpace(req.EventUUID),
		Participants: []adapter.ScheduleParticipant{},
		Conflicts:    []string{},
		Candidates:   []adapter.ScheduleCandidate{},
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil
	}
	resp.Enabled = true

	eventUUID := strings.TrimSpace(req.EventUUID)
	if eventUUID == "" {
		return nil, fmt.Errorf("event_uuid is required")
	}
	event, err := calendarEventFor(ctx, userInfo, eventUUID)
	if err != nil || event.StartTime == nil || event.EndTime == nil {
		return nil, fmt.Errorf("event not found")
	}

	// Authorization: only people in the meeting can see its reschedule options.
	me := userInfo.UserDgraphInfo.Uuid
	isCreator := event.CreatedBy != nil && event.CreatedBy.Uuid == me
	allowed := isCreator
	for _, p := range event.Participants {
		if p != nil && p.Uuid == me {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("you are not on this event")
	}

	curStart := event.StartTime.UTC()
	curEnd := event.EndTime.UTC()
	duration := int(curEnd.Sub(curStart).Minutes())
	if duration <= 0 || duration > scheduleMaxDuration {
		duration = scheduleDefaultDuration
	}
	resp.Title = strings.TrimSpace(event.Title)
	resp.CurrentStart = curStart.Format(time.RFC3339)
	resp.CurrentEnd = curEnd.Format(time.RFC3339)
	resp.DurationMins = duration

	windowDays := req.WindowDays
	if windowDays <= 0 || windowDays > scheduleMaxWindow {
		windowDays = scheduleDefaultWindow
	}
	bhStart := req.BusinessStart
	if bhStart < 0 || bhStart > 23 {
		bhStart = scheduleDefaultBHStart
	}
	bhEnd := req.BusinessEnd
	if bhEnd <= bhStart || bhEnd > 24 {
		bhEnd = scheduleDefaultBHEnd
	}

	// Build the invitee set (creator + participants) and a name lookup.
	userIDs := make([]string, 0, len(event.Participants)+1)
	nameByID := map[string]string{}
	addUser := func(uuid, name string) {
		if uuid == "" {
			return
		}
		if _, ok := nameByID[uuid]; ok {
			return
		}
		display := strings.TrimSpace(name)
		nameByID[uuid] = display
		userIDs = append(userIDs, uuid)
		label := display
		if uuid == me {
			label = "You"
		}
		resp.Participants = append(resp.Participants, adapter.ScheduleParticipant{UUID: uuid, Name: label})
	}
	if event.CreatedBy != nil {
		addUser(event.CreatedBy.Uuid, displayName(event.CreatedBy.UserName, event.CreatedBy.UserFullName))
	}
	for _, p := range event.Participants {
		if p != nil {
			addUser(p.Uuid, displayName(p.UserName, p.UserFullName))
		}
	}

	now := time.Now().UTC()
	windowEnd := now.AddDate(0, 0, windowDays)
	// Free/busy EXCLUDING this event, so its own slot is never a conflict and
	// the current time can be re-proposed if everyone is actually free.
	busyByUser, unread := gatherFreeBusy(ctx, userIDs, minTime(now, curStart), windowEnd, event)

	// Conflict detection at the CURRENT time. Someone whose calendar couldn't
	// be read has no known conflict; the note names them.
	var unreadNames []string
	for _, p := range resp.Participants {
		if unread[p.UUID] {
			unreadNames = append(unreadNames, p.Name)
		}
	}
	for _, uid := range userIDs {
		for _, iv := range busyByUser[uid] {
			if overlaps(curStart, curEnd, iv.start, iv.end) {
				name := nameByID[uid]
				if uid == me {
					name = "You"
				}
				if name == "" {
					name = "Someone"
				}
				resp.Conflicts = append(resp.Conflicts, name)
				break
			}
		}
	}
	resp.ConflictCount = len(resp.Conflicts)

	resp.Candidates = computeCandidateSlots(now, windowDays, duration, scheduleZone(ctx, req.Timezone, req.UTCOffsetMins), bhStart, bhEnd, scheduleMaxCandidates, busyByUser, unread, userIDs)
	noSlots := ""
	if len(resp.Candidates) == 0 {
		noSlots = fmt.Sprintf("No open slots in the next %d days during business hours.", windowDays)
	}
	resp.Note = joinNotes(noSlots, unreadNote(unreadNames))
	return resp, nil
}

// ConfirmReschedule moves the event to the chosen slot through the normal
// calendar update path (creator-only, enforced by business.UpdateEvent), so
// permissions + Google sync are identical to a hand edit. Title, description,
// and participants are preserved; only the time changes.
func ConfirmReschedule(ctx context.Context, userInfo *userModels.UserInfo, req adapter.ScheduleConfirmRescheduleRequest) (*adapter.ScheduleConfirmResponse, error) {
	eventUUID := strings.TrimSpace(req.EventUUID)
	// One instance of a repeating event moves on its own.
	instance := calendarBusiness.IsInstanceID(eventUUID)
	id, perr := uuid.Parse(eventUUID)
	if perr != nil && !instance {
		return nil, fmt.Errorf("invalid event id")
	}
	st, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Start))
	if err != nil {
		return nil, fmt.Errorf("invalid start time (expected RFC3339)")
	}
	et, err := time.Parse(time.RFC3339, strings.TrimSpace(req.End))
	if err != nil {
		return nil, fmt.Errorf("invalid end time (expected RFC3339)")
	}
	if !et.After(st) {
		return nil, fmt.Errorf("end must be after start")
	}

	event, gerr := calendarEventFor(ctx, userInfo, eventUUID)
	if gerr != nil {
		return nil, gerr
	}

	// Preserve everything but the time. Participant UUIDs from the existing
	// event so the invite list is unchanged.
	var participantUUIDs []string
	for _, p := range event.Participants {
		if p != nil && p.Uuid != "" {
			participantUUIDs = append(participantUUIDs, p.Uuid)
		}
	}
	input := calendarAdapter.CreateOrUpdateEventInput{
		Title:                strings.TrimSpace(event.Title),
		Description:          strings.TrimSpace(event.Description),
		StartTime:            st.Format(time.RFC3339),
		EndTime:              et.Format(time.RFC3339),
		Participants:         participantUUIDs,
		SyncToGoogleCalendar: req.SyncToGoogle,
	}
	update := func() error { return calendarBusiness.UpdateEvent(ctx, id, input, userInfo) }
	if instance {
		update = func() error { return calendarBusiness.UpdateEventInstance(ctx, eventUUID, input, userInfo) }
	}
	if uerr := update(); uerr != nil {
		// UpdateEvent returns a friendly "only the creator can edit" error.
		return nil, uerr
	}
	return &adapter.ScheduleConfirmResponse{
		EventUUID: eventUUID,
		Title:     strings.TrimSpace(event.Title),
		Start:     req.Start,
		End:       req.End,
	}, nil
}

// displayName is the one name rule (helpers.PersonDisplayName) for a person
// whose address isn't at hand: display name, else full name.
func displayName(userName, fullName string) string {
	return helpers.PersonDisplayName(userName, fullName, "")
}

// minTime returns the earlier of two times.
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
