package business

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// CheckInInput is a check-in as written: where the goal stands (or how it
// ended), a note, for a number goal the value it has moved to, and,
// optionally, a channel to post it in as well.
type CheckInInput struct {
	Health      string   `json:"health"`
	Body        string   `json:"body"`
	Value       *float64 `json:"value,omitempty"`
	ChannelUUID string   `json:"channel_uuid,omitempty"`
}

// CheckCheckIn is the check-in as stored, or what to fix. Pure.
func CheckCheckIn(g *goalModel.Goal, in CheckInInput) (CheckInInput, error) {
	if _, ok := Healths[in.Health]; !ok {
		return in, refuse("Choose where the goal stands (on track, at risk, off track, on hold), or close it as achieved, missed or dropped.")
	}
	if g.Status != goalModel.StatusOpen {
		return in, refuse("This goal is closed. Reopen it to check in.")
	}
	in.Body = helpers.NormaliseText(in.Body)
	if len([]rune(in.Body)) > MaxBody {
		return in, refuse("Keep a check-in under %s characters.", helpers.Thousands(MaxBody))
	}
	if in.Value != nil {
		if g.Measure != goalModel.MeasureNumber {
			return in, refuse("Only a goal measured by a number takes a value.")
		}
		if !finite(in.Value) {
			return in, refuse("That number is too large.")
		}
	}
	if in.Body == "" && in.Value == nil && !Closing(in.Health) {
		return in, refuse("Write a few words about how the goal is going.")
	}
	if in.ChannelUUID = strings.TrimSpace(in.ChannelUUID); in.ChannelUUID != "" {
		if _, err := uuid.Parse(in.ChannelUUID); err != nil {
			return in, refuse("That channel isn't one OneCamp knows.")
		}
	}
	return in, nil
}

// Posted is a new check-in, and what became of posting it in a channel too.
type Posted struct {
	CheckIn CheckInView `json:"checkin"`
	// Channel is where it was also posted; ChannelError why it wasn't.
	Channel      string `json:"channel,omitempty"`
	ChannelError string `json:"channel_error,omitempty"`
}

// GoalLink is the goal's page in the app.
func GoalLink(id uuid.UUID) string {
	return authService.FrontendBaseURL() + "/app/goals/" + id.String()
}

// ChannelHTML is the message a check-in becomes in a channel: the goal, where
// it stands and how far along it is, the note, and a link to the goal.
func ChannelHTML(g *goalModel.Goal, c *goalModel.CheckIn) string {
	head := fmt.Sprintf("<strong>Goal: %s</strong> · %s", html.EscapeString(g.Title), Healths[c.Health])
	if c.Progress != nil {
		head += " · " + Percent(*c.Progress)
	}
	link := html.EscapeString(GoalLink(g.Id))
	return fmt.Sprintf("<p>%s</p>%s<p><a href=\"%s\">%s</a></p>", head, helpers.PlainTextToHTML(c.Body), link, link)
}

// PostCheckIn records where a goal stands, as one of the people who may
// change it. A closing health closes the goal, keeping the progress it ends
// at; a number goal's value moves the number. A channel that refuses it
// doesn't undo the check-in; the person is told why.
func PostCheckIn(ctx context.Context, user *userModels.UserInfo, r Reader, id uuid.UUID, in CheckInInput, now time.Time) (*Posted, error) {
	g, err := editable(r, id)
	if err != nil {
		return nil, err
	}
	if in, err = CheckCheckIn(g, in); err != nil {
		return nil, err
	}
	// The progress it was posted at: with the new value for a number goal.
	var progress *float64
	if g.Measure == goalModel.MeasureNumber && g.StartValue != nil && g.TargetValue != nil && g.CurrentValue != nil {
		cur := *g.CurrentValue
		if in.Value != nil {
			cur = *in.Value
		}
		v := NumberProgress(*g.StartValue, *g.TargetValue, cur)
		progress = &v
	} else {
		b, err := load(ctx, r, now)
		if err != nil {
			return nil, err
		}
		progress = b.progressOf(id)
	}
	closeAs := ""
	if Closing(in.Health) {
		closeAs = in.Health
	}
	c, err := goalModel.AddCheckIn(goalModel.CheckIn{GoalId: id, AuthorUUID: r.UUID, Health: in.Health, Body: in.Body, Progress: progress}, in.Value, closeAs)
	if errors.Is(err, goalModel.ErrClosed) {
		return nil, refuse("This goal was closed a moment ago. Reopen it to check in.")
	}
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	out := &Posted{CheckIn: checkInViews(ctx, []goalModel.CheckIn{*c})[0]}
	if in.ChannelUUID != "" {
		out.Channel, out.ChannelError = sendBusiness.AlsoInChannel(ctx, user, in.ChannelUUID, ChannelHTML(g, c), "Checked in on the goal")
	}
	return out, nil
}

// EditCheckIn changes a check-in's health and note; only its author may. A
// check-in that closed the goal keeps how it ended, and an open one can't
// become an ending: reopening and closing are their own moves.
func EditCheckIn(ctx context.Context, r Reader, goalID, id uuid.UUID, health, body string) (*CheckInView, error) {
	cur, err := goalModel.GetCheckIn(id, goalID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrCheckInNotFound
	}
	if cur.AuthorUUID != r.UUID {
		return nil, ErrCheckInNotYours
	}
	if _, ok := Healths[health]; !ok || Closing(cur.Health) != Closing(health) || (Closing(health) && health != cur.Health) {
		return nil, refuse("A check-in keeps whether it closed the goal. Reopen or close the goal to change that.")
	}
	body = helpers.NormaliseText(body)
	if len([]rune(body)) > MaxBody {
		return nil, refuse("Keep a check-in under %s characters.", helpers.Thousands(MaxBody))
	}
	if body == "" && cur.Value == nil && !Closing(health) {
		return nil, refuse("Write a few words about how the goal is going.")
	}
	c, err := goalModel.EditCheckIn(id, goalID, health, body)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrCheckInNotFound
	}
	v := checkInViews(ctx, []goalModel.CheckIn{*c})[0]
	return &v, nil
}

// DeleteCheckIn removes a check-in: its author may, and so may whoever can
// change the goal. The goal's number and status stay as they are.
func DeleteCheckIn(r Reader, goalID, id uuid.UUID) error {
	g, err := goalModel.Get(goalID)
	if err != nil {
		return err
	}
	if g == nil {
		return ErrNotFound
	}
	cur, err := goalModel.GetCheckIn(id, goalID)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrCheckInNotFound
	}
	if cur.AuthorUUID != r.UUID && !r.CanEdit(g) {
		return ErrCheckInNotYours
	}
	ok, err := goalModel.DeleteCheckIn(id, goalID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCheckInNotFound
	}
	return nil
}
