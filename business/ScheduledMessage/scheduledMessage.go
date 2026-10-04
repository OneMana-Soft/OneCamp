// Package business (ScheduledMessage) is "send this later": a channel post, a
// direct message or a group message, written now and delivered at a time the
// person picks. It rides the durable scheduler (claim-based, safe across
// restarts and replicas) and sends through business/Send, so the message is
// checked by the same rules when it is scheduled and again when it fires: a
// person who left the channel in between does not post into it.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	jobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Kinds of conversation a message can be scheduled into.
const (
	KindChannel = "channel"
	KindDM      = "dm"
	KindGroup   = "group"
)

// Limits: far enough out for "after the holiday", few enough that a list of
// them stays a list a person can read.
const (
	MinLead      = 30 * time.Second
	MaxLead      = 120 * 24 * time.Hour
	MaxPerPerson = 50
)

// Invalid is a request the person can fix; its message is written for them.
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

func invalid(msg string) error { return &Invalid{Msg: msg} }

var (
	ErrNotFound   = errors.New("that scheduled message no longer exists")
	ErrNotPending = errors.New("that message has already been sent or cancelled")
	ErrTooMany    = &Invalid{Msg: fmt.Sprintf("You can have up to %d scheduled messages at once.", MaxPerPerson)}
)

// payload is what the job carries: the request the composer would have sent,
// untouched, so the message goes out exactly as written (text, attachments,
// reply target).
type payload struct {
	Kind    string          `json:"kind"`
	Body    json.RawMessage `json:"body"`
	Target  string          `json:"target"`
	Preview string          `json:"preview"`
}

// View is a scheduled message as the person sees it in their list.
type View struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target"`
	SendAt    time.Time       `json:"send_at"`
	Status    string          `json:"status"`
	Preview   string          `json:"preview"`
	Body      json.RawMessage `json:"body"`
	LastError string          `json:"last_error,omitempty"`
}

func init() {
	schedulerBusiness.RegisterJobHandler(jobModel.JobTypeScheduledMessage, run)
}

// ValidateSendAt keeps the time inside the window. Pure, so it is tested.
func ValidateSendAt(sendAt, now time.Time) error {
	if sendAt.Before(now.Add(MinLead)) {
		return invalid("Pick a time at least a minute from now.")
	}
	if sendAt.After(now.Add(MaxLead)) {
		return invalid("Pick a time within the next 120 days.")
	}
	return nil
}

// Preview is the plain one-line text shown in the scheduled list. Pure.
func Preview(html string) string {
	s := strings.Join(strings.Fields(helpers.RemoveHTMLTags(html)), " ")
	if utf8.RuneCountInString(s) > 140 {
		s = string([]rune(s)[:139]) + "…"
	}
	return s
}

// prepared is a message that passed every rule, with what the list shows.
type prepared struct {
	target  string
	preview string
	commit  func(context.Context) error
}

// prepare decodes the composer's request for its kind and runs the send rules.
func prepare(ctx context.Context, user *userModels.UserInfo, kind string, body json.RawMessage) (*prepared, error) {
	switch kind {
	case KindChannel:
		var in postAdapter.InputCreateOrUpdatePostInfo
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, invalid("Couldn't read that message.")
		}
		if strings.TrimSpace(in.HTMLText) == "" && len(in.MediaObj) == 0 {
			return nil, invalid("There is nothing to send.")
		}
		in.Uuid = "" // a scheduled post is always a new post
		p, err := sendBusiness.PrepareChannelPost(ctx, user, &in)
		if err != nil {
			return nil, err
		}
		return &prepared{target: in.ChannelUuid, preview: Preview(in.HTMLText), commit: func(c context.Context) error { _, e := p.Commit(c); return e }}, nil
	case KindDM, KindGroup:
		var in chatAdapter.ChatInfo
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, invalid("Couldn't read that message.")
		}
		if strings.TrimSpace(in.TextHtml) == "" && len(in.MediaObjects) == 0 {
			return nil, invalid("There is nothing to send.")
		}
		in.Uuid = ""
		if kind == KindDM {
			m, err := sendBusiness.PrepareDirectMessage(ctx, user, &in)
			if err != nil {
				return nil, err
			}
			return &prepared{target: in.ToUuid, preview: Preview(in.TextHtml), commit: func(c context.Context) error { _, e := m.Commit(c); return e }}, nil
		}
		g, err := sendBusiness.PrepareGroupMessage(ctx, user, &in)
		if err != nil {
			return nil, err
		}
		return &prepared{target: in.GrpUuid, preview: Preview(in.TextHtml), commit: func(c context.Context) error { _, e := g.Commit(c); return e }}, nil
	}
	return nil, invalid("Unknown kind of conversation.")
}

func ownerUUID(user *userModels.UserInfo) (uuid.UUID, error) {
	return uuid.Parse(user.UserDgraphInfo.Uuid)
}

func toView(j *jobModel.ScheduledJob) *View {
	var p payload
	_ = json.Unmarshal([]byte(j.Payload), &p)
	v := &View{ID: j.Id.String(), Kind: p.Kind, Target: p.Target, SendAt: j.RunAt, Status: j.Status, Preview: p.Preview, Body: p.Body}
	if j.LastError != nil {
		v.LastError = *j.LastError
	}
	return v
}

// Schedule checks the message now and queues it for sendAt.
func Schedule(ctx context.Context, user *userModels.UserInfo, kind string, body json.RawMessage, sendAt time.Time) (*View, error) {
	if err := ValidateSendAt(sendAt, time.Now()); err != nil {
		return nil, err
	}
	owner, err := ownerUUID(user)
	if err != nil {
		return nil, err
	}
	n, err := jobDomain.CountByUser(ctx, owner, jobModel.JobTypeScheduledMessage, []string{jobModel.StatusPending})
	if err != nil {
		return nil, err
	}
	if n >= MaxPerPerson {
		return nil, ErrTooMany
	}
	p, err := prepare(ctx, user, kind, body)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(payload{Kind: kind, Body: body, Target: p.target, Preview: p.preview})
	id, err := schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{
		JobType: jobModel.JobTypeScheduledMessage, UserUUID: owner, PayloadJSON: string(raw), RunAt: sendAt.UTC(),
	})
	if err != nil {
		return nil, err
	}
	notify(owner.String(), id.String(), "scheduled")
	return &View{ID: id.String(), Kind: kind, Target: p.target, SendAt: sendAt.UTC(), Status: jobModel.StatusPending, Preview: p.preview, Body: body}, nil
}

// List is the person's scheduled messages: pending, and recent failures so a
// message that could not go is never silently lost. target narrows to one
// conversation.
func List(ctx context.Context, user *userModels.UserInfo, target string) ([]*View, error) {
	owner, err := ownerUUID(user)
	if err != nil {
		return nil, err
	}
	jobs, err := jobDomain.ListByUser(ctx, owner, jobModel.JobTypeScheduledMessage, []string{jobModel.StatusPending, jobModel.StatusRunning, jobModel.StatusFailed}, MaxPerPerson+100)
	if err != nil {
		return nil, err
	}
	out := []*View{}
	weekAgo := time.Now().Add(-7 * 24 * time.Hour)
	for _, j := range jobs {
		if j.Status == jobModel.StatusFailed && j.UpdatedAt.Before(weekAgo) {
			continue
		}
		v := toView(j)
		if target != "" && v.Target != target {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// Update moves a pending message to a new time and, if given, new text.
func Update(ctx context.Context, user *userModels.UserInfo, id uuid.UUID, sendAt time.Time, body json.RawMessage) (*View, error) {
	if err := ValidateSendAt(sendAt, time.Now()); err != nil {
		return nil, err
	}
	owner, err := ownerUUID(user)
	if err != nil {
		return nil, err
	}
	j, err := jobModel.GetUserJob(ctx, id, owner, jobModel.JobTypeScheduledMessage)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, ErrNotFound
	}
	var old payload
	_ = json.Unmarshal([]byte(j.Payload), &old)
	if len(body) == 0 {
		body = old.Body
	}
	p, err := prepare(ctx, user, old.Kind, body)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(payload{Kind: old.Kind, Body: body, Target: p.target, Preview: p.preview})
	ok, err := jobModel.UpdatePendingJob(ctx, id, owner, sendAt.UTC(), string(raw))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotPending
	}
	notify(owner.String(), id.String(), "updated")
	return &View{ID: id.String(), Kind: old.Kind, Target: p.target, SendAt: sendAt.UTC(), Status: jobModel.StatusPending, Preview: p.preview, Body: body}, nil
}

// Cancel drops a pending message.
func Cancel(ctx context.Context, user *userModels.UserInfo, id uuid.UUID) error {
	owner, err := ownerUUID(user)
	if err != nil {
		return err
	}
	ok, err := jobModel.CancelPendingJob(ctx, id, owner)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotPending
	}
	notify(owner.String(), id.String(), "cancelled")
	return nil
}

// SendNow sends a pending message immediately, through the same claim and run
// the worker uses, so it can never go twice.
func SendNow(ctx context.Context, user *userModels.UserInfo, id uuid.UUID) error {
	owner, err := ownerUUID(user)
	if err != nil {
		return err
	}
	j, err := jobModel.ClaimUserJob(ctx, id, owner, "send-now")
	if err != nil {
		return err
	}
	if j == nil {
		return ErrNotPending
	}
	schedulerBusiness.RunClaimed(ctx, j)
	return nil
}

// run is the scheduler's handler: load the person, check the message again,
// send it. A rule saying no is final (it would say no again), so the job is
// marked failed with the reason for the list; anything else is retried.
func run(ctx context.Context, j *jobModel.ScheduledJob) error {
	var p payload
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
		return nil // a payload that cannot be read will not improve on retry
	}
	user, err := loadUser(ctx, j.UserUuid)
	if err != nil {
		return err
	}
	ready, err := prepare(ctx, user, p.Kind, p.Body)
	if err == nil {
		err = ready.commit(ctx)
	}
	if err != nil {
		if rj, ok := sendBusiness.AsRejection(err); ok {
			notify(j.UserUuid.String(), j.Id.String(), "failed")
			return &schedulerBusiness.Terminal{Reason: "Not sent: " + rj.Msg}
		}
		return err
	}
	notify(j.UserUuid.String(), j.Id.String(), "sent", p.Kind, ready.target)
	return nil
}

func loadUser(ctx context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	if err != nil || pg == nil {
		return nil, fmt.Errorf("sender not found")
	}
	dg, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
	if err != nil || dg == nil {
		return nil, fmt.Errorf("sender not found")
	}
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *dg}, nil
}

// Notice tells the person's open apps a scheduled message changed state, and
// in which conversation, so a view of it can load a message that was just sent
// for it (a first message in a new DM reaches no subscription of its own).
type Notice struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Kind   string `json:"kind,omitempty"`
	Target string `json:"target,omitempty"`
}

func notify(userID, id, state string, where ...string) {
	n := Notice{ID: id, State: state}
	if len(where) == 2 {
		n.Kind, n.Target = where[0], where[1]
	}
	mqttBusiness.PublishMessageToUser(userID, mqttStruct.MESSAGE_SCHEDULED_MESSAGE, n)
}
