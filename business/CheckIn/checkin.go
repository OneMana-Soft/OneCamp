// Package business (CheckIn): automatic check-ins. A channel asks its people
// a question on chosen days at a time of day ("What did you work on today?",
// weekdays at 17:00 in their zone): OneCamp posts it in the channel, everyone
// answers in its thread, so the answers are read together, and people who are
// away hear about it by email. Basecamp's automatic check-ins, and what Slack
// teams buy Geekbot or Standuply for, built in.
//
// Each check-in keeps one next time. A sweeper on every worker asks the ones
// that are due, each claimed by moving its next time on in one UPDATE, so of
// any number of workers exactly one asks it, and a check-in can't be left
// without a next time by a failure halfway: there is nothing else to break.
package business

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CheckIn"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// CheckInError is a request the person can fix; its text is written for them.
type CheckInError struct{ msg string }

func (e *CheckInError) Error() string { return e.msg }

func refuse(format string, args ...any) error { return &CheckInError{fmt.Sprintf(format, args...)} }

var (
	// ErrNotFound: no such check-in, or a channel the person can't see.
	ErrNotFound = errors.New("check-in not found")
	// ErrNotAllowed: only a channel's moderators and workspace admins set its check-ins.
	ErrNotAllowed = errors.New("only the channel's moderators or a workspace admin can change its check-ins")
)

const (
	// MaxQuestion is the longest question, in characters.
	MaxQuestion = 300
	// lateLimit is how late a due time is still asked, after the workers were
	// down: a question about today asked tomorrow is worse than none.
	lateLimit = 2 * time.Hour
	// sweepEvery is how often a worker looks for due check-ins.
	sweepEvery = 20 * time.Second
	// askAgainAfter is how soon "Ask now" may ask the same check-in again.
	askAgainAfter = time.Minute
)

// The world outside the package, as variables a test can stand in for: the
// channel (members and moderators), posting as the check-in bot, and the emails.
var (
	channelInfo = channelDomain.GetDgraphChannelInfoByUUID
	postAs      = func(ctx context.Context, channel uuid.UUID, text string) (*botpost.Result, error) {
		bot, err := userBusiness.EnsureCheckInBot(ctx)
		if err != nil {
			return nil, err
		}
		return botpost.PostToChannelAsBot(ctx, channel, text, bot)
	}
	notify = notificationBusiness.DispatchCheckIn
)

// Input is a check-in as written: the question, the days it's asked (1 is
// Monday, 7 Sunday), the time of day ("17:00"), and the zone that time is in.
type Input struct {
	Question string `json:"question"`
	Days     []int  `json:"days"`
	Time     string `json:"time"`
	TZ       string `json:"tz"`
}

// Schedule is when a check-in asks: days as a bitmask (Monday 1 to Sunday 64),
// the minute of the day, and the zone.
type Schedule struct {
	Days     int
	AtMinute int
	Loc      *time.Location
}

// Check is the question and schedule as stored, or what to fix. Pure.
func Check(in Input) (string, Schedule, error) {
	q := strings.Join(strings.Fields(in.Question), " ")
	switch n := utf8.RuneCountInString(q); {
	case n == 0:
		return "", Schedule{}, refuse("Write the question to ask, like \"What did you work on today?\"")
	case n > MaxQuestion:
		return "", Schedule{}, refuse("Keep the question under %d characters.", MaxQuestion)
	}
	s := Schedule{}
	for _, d := range in.Days {
		if d < 1 || d > 7 {
			return "", Schedule{}, refuse("Choose the days to ask from Monday to Sunday.")
		}
		s.Days |= 1 << (d - 1)
	}
	if s.Days == 0 {
		return "", Schedule{}, refuse("Choose at least one day to ask.")
	}
	t, err := time.Parse("15:04", strings.TrimSpace(in.Time))
	if err != nil {
		return "", Schedule{}, refuse("Choose the time to ask, like 17:00.")
	}
	s.AtMinute = t.Hour()*60 + t.Minute()
	tz := strings.TrimSpace(in.TZ)
	if tz == "" {
		tz = "UTC"
	}
	if s.Loc, err = time.LoadLocation(tz); err != nil {
		return "", Schedule{}, refuse("That time zone isn't one OneCamp knows.")
	}
	return q, s, nil
}

// errNoTime is a schedule that never asks, which Check doesn't let through.
var errNoTime = errors.New("check-in schedule has no time")

// Next is the first time after `after` that a schedule asks: on one of its
// days, at its time, in its zone, so 17:00 stays 17:00 when the clocks change.
// Days are calendar days (read at noon, which every day has), so a zone whose
// clocks change at midnight still asks on the right day; a time the clocks
// skip is asked at the first minute after the gap. Pure.
func Next(s Schedule, after time.Time) (time.Time, error) {
	a := after.In(s.Loc)
	for i := 0; i < 15; i++ {
		day := time.Date(a.Year(), a.Month(), a.Day()+i, 12, 0, 0, 0, s.Loc)
		if s.Days&(1<<((int(day.Weekday())+6)%7)) == 0 {
			continue
		}
		if t := wallClock(day, s.AtMinute, s.Loc); t.After(after) {
			return t, nil
		}
	}
	return time.Time{}, errNoTime
}

// wallClock is minute m of day's date in loc, or the first minute after it
// that the clocks show, when a spring-forward skips it. Pure.
func wallClock(day time.Time, m int, loc *time.Location) time.Time {
	y, mo, d := day.Date()
	for k := 0; k <= 180; k++ {
		t := time.Date(y, mo, d, 0, m+k, 0, 0, loc)
		if ly, lmo, ld := t.Date(); ly == y && lmo == mo && ld == d && t.Hour()*60+t.Minute() == m+k {
			return t
		}
	}
	return time.Date(y, mo, d, 0, m, 0, 0, loc)
}

func scheduleOf(c *model.CheckIn) (Schedule, error) {
	loc, err := time.LoadLocation(c.TZ)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Days: c.Days, AtMinute: c.AtMinute, Loc: loc}, nil
}

// View is a check-in as the channel's settings show it.
type View struct {
	Id           string     `json:"id"`
	Question     string     `json:"question"`
	Days         []int      `json:"days"`
	Time         string     `json:"time"`
	TZ           string     `json:"tz"`
	Paused       bool       `json:"paused"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	LastAskedAt  *time.Time `json:"last_asked_at,omitempty"`
	LastPostUUID string     `json:"last_post_uuid,omitempty"`
}

func viewOf(c *model.CheckIn) View {
	v := View{Id: c.Id.String(), Question: c.Question, Time: fmt.Sprintf("%02d:%02d", c.AtMinute/60, c.AtMinute%60), TZ: c.TZ,
		Paused: c.Paused, NextRunAt: c.NextRunAt, LastAskedAt: c.LastAskedAt, Days: []int{}}
	for d := 1; d <= 7; d++ {
		if c.Days&(1<<(d-1)) != 0 {
			v.Days = append(v.Days, d)
		}
	}
	if c.LastPostUUID != nil {
		v.LastPostUUID = c.LastPostUUID.String()
	}
	return v
}

// Reader is who is asking, as check-ins need to know them.
type Reader struct {
	User    *userModels.UserInfo
	IsAdmin bool
}

func gone(ch *dgraphStruct.DgraphChannel) bool {
	return ch == nil || ch.Uuid == "" || (ch.DeletedAt != nil && ch.DeletedAt.After(time.Unix(0, 0)))
}

// channelFor is the channel as the reader sees it: one they're in (or any, for
// a workspace admin), live. edit asks for the right to change its check-ins.
func channelFor(ctx context.Context, r Reader, channelUUID uuid.UUID, edit bool) (*dgraphStruct.DgraphChannel, error) {
	ch, err := channelInfo(ctx, channelUUID.String(), r.User.UserDgraphInfo.Uid)
	if err != nil || gone(ch) {
		return nil, ErrNotFound
	}
	if ch.IsMember == 0 && !r.IsAdmin {
		return nil, ErrNotFound
	}
	if edit && ch.IsAdmin == 0 && !r.IsAdmin {
		return nil, ErrNotAllowed
	}
	return ch, nil
}

// List is a channel's check-ins, and whether the reader may change them. A
// channel the reader isn't in, or that's archived, has none to show them.
func List(ctx context.Context, r Reader, channelUUID uuid.UUID) ([]View, bool, error) {
	ch, err := channelFor(ctx, r, channelUUID, false)
	if errors.Is(err, ErrNotFound) {
		return []View{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	list, err := model.ForChannel(channelUUID)
	if err != nil {
		return nil, false, err
	}
	out := make([]View, 0, len(list))
	for i := range list {
		out = append(out, viewOf(&list[i]))
	}
	return out, ch.IsAdmin > 0 || r.IsAdmin, nil
}

// Create sets up a check-in on a channel, with its first time.
func Create(ctx context.Context, r Reader, channelUUID uuid.UUID, in Input, now time.Time) (*View, error) {
	if _, err := channelFor(ctx, r, channelUUID, true); err != nil {
		return nil, err
	}
	q, s, err := Check(in)
	if err != nil {
		return nil, err
	}
	next, err := Next(s, now)
	if err != nil {
		return nil, err
	}
	c, err := model.Create(model.CheckIn{ChannelUUID: channelUUID, Question: q, Days: s.Days, AtMinute: s.AtMinute,
		TZ: s.Loc.String(), CreatedBy: r.User.UserPostgresInfo.Id, NextRunAt: &next})
	if errors.Is(err, model.ErrTooMany) {
		return nil, refuse("A channel can have up to %d check-ins.", model.MaxPerChannel)
	}
	if err != nil {
		return nil, err
	}
	v := viewOf(c)
	return &v, nil
}

// editable is the live check-in, if the reader may change it.
func editable(ctx context.Context, r Reader, id uuid.UUID) (*model.CheckIn, *dgraphStruct.DgraphChannel, error) {
	c, err := model.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, ErrNotFound
	}
	ch, err := channelFor(ctx, r, c.ChannelUUID, true)
	if err != nil {
		return nil, nil, err
	}
	return c, ch, nil
}

func done(c *model.CheckIn, err error) (*View, error) {
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	v := viewOf(c)
	return &v, nil
}

// Edit changes what a check-in asks and when; its next time follows.
func Edit(ctx context.Context, r Reader, id uuid.UUID, in Input, now time.Time) (*View, error) {
	if _, _, err := editable(ctx, r, id); err != nil {
		return nil, err
	}
	q, s, err := Check(in)
	if err != nil {
		return nil, err
	}
	next, err := Next(s, now)
	if err != nil {
		return nil, err
	}
	return done(model.Update(id, q, s.Days, s.AtMinute, s.Loc.String(), next))
}

// SetPaused pauses a check-in (it asks nothing until resumed) or resumes it
// from its next time after now.
func SetPaused(ctx context.Context, r Reader, id uuid.UUID, paused bool, now time.Time) (*View, error) {
	c, _, err := editable(ctx, r, id)
	if err != nil {
		return nil, err
	}
	var next *time.Time
	if !paused {
		s, err := scheduleOf(c)
		if err != nil {
			return nil, err
		}
		t, err := Next(s, now)
		if err != nil {
			return nil, err
		}
		next = &t
	}
	return done(model.SetPaused(id, paused, next))
}

// Delete removes a check-in. Its past questions and answers stay in the channel.
func Delete(ctx context.Context, r Reader, id uuid.UUID) error {
	if _, _, err := editable(ctx, r, id); err != nil {
		return err
	}
	_, err := model.Delete(id)
	return err
}

// AskNow asks a check-in's question at once, outside its schedule. Not twice
// within a minute: a double click mustn't ask everyone twice.
func AskNow(ctx context.Context, r Reader, id uuid.UUID, now time.Time) (string, error) {
	c, ch, err := editable(ctx, r, id)
	if err != nil {
		return "", err
	}
	if c.LastAskedAt != nil && now.Sub(*c.LastAskedAt) < askAgainAfter {
		return "", refuse("It was asked a moment ago. Its answers are in that thread.")
	}
	return ask(ctx, c, ch, now, now)
}

// QuestionHTML is the post a check-in asks with: the question, the day it is
// for in the check-in's zone, and where to answer. Pure.
func QuestionHTML(question string, at time.Time) string {
	return fmt.Sprintf("<p><strong>%s</strong></p><p>Check-in for %s. Answer in this thread; everyone's answers are together here.</p>",
		html.EscapeString(question), at.Format("Monday 2 January"))
}

// ask posts the question for the time at in the channel as the check-in bot,
// tells the people who are away, and records the post as asked at now.
func ask(ctx context.Context, c *model.CheckIn, ch *dgraphStruct.DgraphChannel, at, now time.Time) (string, error) {
	if s, err := scheduleOf(c); err == nil {
		at = at.In(s.Loc)
	}
	res, err := postAs(ctx, c.ChannelUUID, QuestionHTML(c.Question, at))
	if err != nil {
		return "", err
	}
	var people []string
	for _, m := range ch.Members {
		if m != nil && m.Uuid != "" && !m.IsBot {
			people = append(people, m.Uuid)
		}
	}
	notify(c.Id.String(), res.PostUUID, c.ChannelUUID.String(), ch.Name, c.Question, people)
	if postID, err := uuid.Parse(res.PostUUID); err == nil {
		if err := model.Asked(c.Id, postID, now); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CheckIn/ask record err: %+v", err)
		}
	}
	return res.PostUUID, nil
}

// Sweep asks every check-in that is due. Each is claimed first, by moving its
// next time on in one UPDATE, so of all the workers sweeping at once exactly
// one asks it, and the next time is set before anything can fail. A time
// more than lateLimit past (the workers were down) is moved on, not asked. A
// channel that's gone pauses its check-in.
func Sweep(ctx context.Context, now time.Time) {
	due, err := model.Due(now, 100)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep err: %+v", err)
		return
	}
	for i := range due {
		c := &due[i]
		s, err := scheduleOf(c)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep schedule %s err: %+v", c.Id, err)
			continue
		}
		next, err := Next(s, now)
		if err != nil {
			continue
		}
		won, err := model.Claim(c.Id, *c.NextRunAt, next)
		if err != nil || !won {
			continue
		}
		if now.Sub(*c.NextRunAt) > lateLimit {
			helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep skipped %s, due %s, %s late", c.Id, c.NextRunAt, now.Sub(*c.NextRunAt))
			continue
		}
		ch, err := channelInfo(ctx, c.ChannelUUID.String(), "")
		switch {
		case err == nil && gone(ch):
			if _, err := model.SetPaused(c.Id, true, nil); err != nil {
				helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep pause err: %+v", err)
			}
		case err != nil:
			helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep channel err: %+v", err)
		default:
			if _, err := ask(ctx, c, ch, *c.NextRunAt, now); err != nil {
				helpers.LogErrorWithContext(ctx, "business/CheckIn/Sweep ask err: %+v", err)
			}
		}
	}
}

// StartWorker sweeps for due check-ins every sweepEvery until ctx ends. Called
// where the other background workers start.
func StartWorker(ctx context.Context) {
	helpers.GoSafeNamed("checkin-sweeper", func() {
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			Sweep(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}
