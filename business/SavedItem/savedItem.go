// Package business (SavedItem) is Save for later: a member keeps a message, a
// task or a doc to come back to, and can ask to be reminded of it. When the
// time comes the item bubbles up: it moves to the top of their Later list, an
// open app shows it, and a push reaches a closed one.
//
// Reminders ride the durable scheduler (business/Scheduler), so they survive
// restarts and fire once across replicas. A job carries the reminder time it
// was queued for; when a member changes or clears the reminder, the old job
// finds a different time on the item and does nothing, so jobs never need to
// be found and cancelled.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/SavedItem"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
)

// ItemTypes are the things that can be saved.
var ItemTypes = map[string]bool{
	"post":    true, // a channel message
	"comment": true, // a reply in a thread
	"chat":    true, // a direct message
	"task":    true,
	"doc":     true,
	"project": true,
	"board":   true,
}

const (
	maxOpenItems = 1000
	maxTitle     = 300
	maxContext   = 200
	listLimit    = 200
	// A reminder may not be further away than this.
	maxAhead = 366 * 24 * time.Hour
	// A reminder a little in the past (a slow click on "in 1 minute") still
	// counts; anything older is a mistake.
	pastSlack = 2 * time.Minute
)

// A link must be a path inside the app. It is followed by the member's own
// browser, so an outside URL here would be an open redirect.
var linkRe = regexp.MustCompile(`^/app/[A-Za-z0-9/_\-]+(\?[A-Za-z0-9_\-=&%.]*)?$`)

// Errors a member can act on.
var (
	ErrInvalid  = errors.New("invalid saved item")
	ErrTooMany  = errors.New("too many saved items")
	ErrNotFound = model.ErrNotFound
)

// SaveInput is what the app sends to save something.
type SaveInput struct {
	ItemType string     `json:"item_type"`
	ItemID   string     `json:"item_id"`
	Link     string     `json:"link"`
	Title    string     `json:"title"`
	Context  string     `json:"context"`
	RemindAt *time.Time `json:"remind_at"`
}

func init() {
	schedulerBusiness.RegisterJobHandler(jobModel.JobTypeSavedItem, runReminder)
}

// Validate checks and tidies an input. now is passed in for tests.
func (in *SaveInput) Validate(now time.Time) error {
	in.ItemType = strings.TrimSpace(in.ItemType)
	in.ItemID = strings.TrimSpace(in.ItemID)
	in.Link = strings.TrimSpace(in.Link)
	if !ItemTypes[in.ItemType] || in.ItemID == "" || len(in.ItemID) > 128 {
		return ErrInvalid
	}
	if len(in.Link) > 512 || !linkRe.MatchString(in.Link) {
		return ErrInvalid
	}
	in.Title = clip(oneLine(in.Title), maxTitle)
	in.Context = clip(oneLine(in.Context), maxContext)
	return ValidateRemindAt(in.RemindAt, now)
}

// ValidateRemindAt accepts no reminder, or one from about now to a year ahead.
func ValidateRemindAt(t *time.Time, now time.Time) error {
	if t == nil {
		return nil
	}
	if t.Before(now.Add(-pastSlack)) || t.After(now.Add(maxAhead)) {
		return ErrInvalid
	}
	return nil
}

// Save stores something for the member, or refreshes it if already saved.
func Save(ctx context.Context, userID uuid.UUID, in SaveInput) (*model.SavedItem, error) {
	now := time.Now()
	if err := in.Validate(now); err != nil {
		return nil, err
	}
	open, _, err := model.Counts(ctx, userID, now)
	if err != nil {
		return nil, err
	}
	if open >= maxOpenItems {
		return nil, ErrTooMany
	}
	item, err := model.Save(ctx, &model.SavedItem{
		UserID: userID, ItemType: in.ItemType, ItemID: in.ItemID,
		Link: in.Link, Title: in.Title, Context: in.Context, RemindAt: in.RemindAt,
	})
	if err != nil {
		return nil, err
	}
	schedule(ctx, item)
	return item, nil
}

// SetReminder replaces the reminder on one of the member's items.
func SetReminder(ctx context.Context, userID, id uuid.UUID, remindAt *time.Time) (*model.SavedItem, error) {
	if err := ValidateRemindAt(remindAt, time.Now()); err != nil {
		return nil, err
	}
	item, err := model.SetReminder(ctx, userID, id, remindAt)
	if err != nil {
		return nil, err
	}
	schedule(ctx, item)
	return item, nil
}

// SetDone finishes or reopens one of the member's items.
func SetDone(ctx context.Context, userID, id uuid.UUID, done bool) (*model.SavedItem, error) {
	return model.SetDone(ctx, userID, id, done)
}

// Delete removes one of the member's items.
func Delete(ctx context.Context, userID, id uuid.UUID) error {
	return model.Delete(ctx, userID, id)
}

// ListResult is the Later list with its counts.
type ListResult struct {
	Items []*model.SavedItem `json:"items"`
	Open  int                `json:"open"`
	Due   int                `json:"due"`
}

// List returns the member's open or finished items and the open/due counts.
func List(ctx context.Context, userID uuid.UUID, done bool) (*ListResult, error) {
	now := time.Now()
	items, err := model.List(ctx, userID, done, now, listLimit)
	if err != nil {
		return nil, err
	}
	open, due, err := model.Counts(ctx, userID, now)
	if err != nil {
		return nil, err
	}
	return &ListResult{Items: items, Open: open, Due: due}, nil
}

type reminderPayload struct {
	ID       string    `json:"id"`
	RemindAt time.Time `json:"remind_at"`
}

// schedule queues the reminder for an item, if it has one. A failure to queue
// is logged, not returned: the item is saved, and it still bubbles to the top
// of Later when due because the list orders by remind_at.
func schedule(ctx context.Context, item *model.SavedItem) {
	if item == nil || item.RemindAt == nil || item.DoneAt != nil {
		return
	}
	payload, _ := json.Marshal(reminderPayload{ID: item.ID.String(), RemindAt: *item.RemindAt})
	if _, err := schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{
		JobType:     jobModel.JobTypeSavedItem,
		UserUUID:    item.UserID,
		PayloadJSON: string(payload),
		RunAt:       item.RemindAt.UTC(),
		MaxAttempts: 3,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SavedItem schedule err: %+v", err)
	}
}

// DueNotice is what an open app receives when an item comes due.
type DueNotice struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Link  string `json:"link"`
}

func runReminder(ctx context.Context, job *jobModel.ScheduledJob) error {
	var p reminderPayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return nil // a bad payload will not get better on retry
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil
	}
	item, err := model.GetByID(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil // removed since
	}
	if err != nil {
		return err
	}
	if item.UserID != job.UserUuid || item.DoneAt != nil || item.RemindAt == nil || !item.RemindAt.Equal(p.RemindAt) {
		return nil // finished, or its reminder was changed after this job was queued
	}
	sent, err := model.MarkReminded(ctx, item.ID, *item.RemindAt)
	if err != nil {
		return err
	}
	if !sent {
		return nil // another worker already delivered it
	}

	title := item.Title
	if title == "" {
		title = "Something you saved for later"
	}
	mqttBusiness.PublishMessageToUser(item.UserID.String(), mqttStruct.MESSAGE_SAVED_ITEM_DUE, DueNotice{
		ID: item.ID.String(), Title: title, Link: item.Link,
	})
	push(ctx, item.UserID.String(), title, item.Link)
	return nil
}

// push wakes a closed or backgrounded app. Best effort: a member without push
// still finds the item at the top of Later.
func push(ctx context.Context, userID, title, link string) {
	if firebaseInit.FirebaseApp.Messaging() == nil {
		return
	}
	tokens, err := fcmDomain.GetFCMTokenByUserId(ctx, userID)
	if err != nil || len(tokens) == 0 {
		return
	}
	data := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:    firebaseInit.FIREBASE_PUSH_DATA_TYPE_LATER,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: link,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:   "Saved for later",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:    title,
	}
	for i := 0; i < len(tokens); i += 500 {
		end := min(i+500, len(tokens))
		if err := firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens[i:end]); err != nil {
			helpers.LogErrorWithContext(ctx, "business/SavedItem push err: %+v", err)
		}
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
