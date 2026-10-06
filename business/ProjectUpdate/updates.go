// Package business (ProjectUpdate): a project's updates. One of its admins
// says where the project stands (on track, at risk, off track, on hold, done)
// and writes a short note, drafted for them from the project's tasks. Members
// read them on the project, are emailed when one is posted, and may see it in
// a channel; one shared with the client shows on the project's client link.
// Linear's project updates and Asana's status updates, without the per-seat
// plan they sit behind.
package business

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	timeModel "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// UpdateError is a request the person can fix; its text is written for them.
type UpdateError struct{ msg string }

func (e *UpdateError) Error() string { return e.msg }

var (
	// ErrNotFound: no such update on this project.
	ErrNotFound = errors.New("project update not found")
	// ErrNotYours: only an update's author may change it.
	ErrNotYours = errors.New("only the update's author can change it")
)

// HealthLabels names each health as the app does.
var HealthLabels = map[string]string{
	model.OnTrack:  "On track",
	model.AtRisk:   "At risk",
	model.OffTrack: "Off track",
	model.OnHold:   "On hold",
	model.Done:     "Done",
}

// View is an update as people read it: who wrote it, by name.
type View struct {
	model.Update
	AuthorName string `json:"author_name"`
}

// Input is an update as written: its health, its text, whether the client
// sees it, and, optionally, a channel to post it in as well.
type Input struct {
	Health           string `json:"health"`
	Body             string `json:"body"`
	SharedWithClient bool   `json:"shared_with_client"`
	ChannelUUID      string `json:"channel_uuid,omitempty"`
}

// Check is the input as stored, or what to fix. Pure.
func Check(in Input) (Input, error) {
	if _, ok := HealthLabels[in.Health]; !ok {
		return in, &UpdateError{"Choose where the project stands: on track, at risk, off track, on hold or done."}
	}
	in.Body = helpers.NormaliseText(in.Body)
	if in.Body == "" {
		return in, &UpdateError{"Write a few words about how the project is going."}
	}
	if len([]rune(in.Body)) > MaxBody {
		return in, &UpdateError{fmt.Sprintf("Keep an update under %d characters.", MaxBody)}
	}
	if in.ChannelUUID = strings.TrimSpace(in.ChannelUUID); in.ChannelUUID != "" {
		if _, err := uuid.Parse(in.ChannelUUID); err != nil {
			return in, &UpdateError{"That channel isn't one OneCamp knows."}
		}
	}
	return in, nil
}

func views(ctx context.Context, list []model.Update) []View {
	ids := make([]string, 0, len(list))
	seen := map[uuid.UUID]bool{}
	for _, u := range list {
		if !seen[u.AuthorUUID] {
			seen[u.AuthorUUID] = true
			ids = append(ids, u.AuthorUUID.String())
		}
	}
	names, err := userDomain.GetUserDisplayMapByUUIDs(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ProjectUpdate/views names err: %+v", err)
	}
	out := make([]View, 0, len(list))
	for _, u := range list {
		v := View{Update: u, AuthorName: "Someone"}
		if p := names[u.AuthorUUID.String()]; p != nil {
			if n := nameOf(p); n != "" {
				v.AuthorName = n
			}
		}
		out = append(out, v)
	}
	return out
}

// List is a project's updates, newest first.
func List(ctx context.Context, projectID uuid.UUID, limit int, sharedOnly bool) ([]View, error) {
	list, err := model.List(projectID, limit, sharedOnly)
	if err != nil {
		return nil, err
	}
	return views(ctx, list), nil
}

// Draft is an update written from the project's tasks, to edit and post.
type Draft struct {
	Text   string    `json:"text"`
	Health string    `json:"health"`
	Since  time.Time `json:"since"`
	Facts  Facts     `json:"facts"`
}

// MakeDraft drafts the next update of a project, read as the person asking:
// what happened since the last update (or the last week), from all its tasks
// and the time tracked on it.
func MakeDraft(ctx context.Context, projectID uuid.UUID, userDgraphUID string, now time.Time) (*Draft, error) {
	var last *time.Time
	if prev, err := model.List(projectID, 1, false); err != nil {
		return nil, err
	} else if len(prev) > 0 {
		last = &prev[0].CreatedAt
	}
	from := Window(last, now)
	// Every task: a draft must see work finished long after it was created,
	// which the board's newest-200 cap on Done would hide.
	p, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, projectID.String(), userDgraphUID, "", 0)
	if err != nil || p == nil {
		return nil, fmt.Errorf("read the project's tasks: %w", err)
	}
	var logged int64
	if entries, err := timeModel.ForProject(projectID, from, now); err == nil {
		for i := range entries {
			logged += entries[i].Seconds(now)
		}
	} else {
		helpers.LogErrorWithContext(ctx, "business/ProjectUpdate/MakeDraft time err: %+v", err)
	}
	f := Gather(p, from, now, logged)
	return &Draft{Text: DraftText(f), Health: SuggestHealth(f), Since: from, Facts: f}, nil
}

// Posted is a new update, and what became of posting it in a channel too.
type Posted struct {
	Update View `json:"update"`
	// Channel is where it was also posted; ChannelError why it wasn't.
	Channel      string `json:"channel,omitempty"`
	ChannelError string `json:"channel_error,omitempty"`
}

func authorOf(user *userModels.UserInfo) (uuid.UUID, string) {
	name := user.UserDgraphInfo.UserFullName
	if name == "" {
		name = user.UserDgraphInfo.UserName
	}
	return user.UserPostgresInfo.Id, name
}

// projectLink is the update's place in the app.
func projectLink(projectID uuid.UUID) string {
	return authService.FrontendBaseURL() + "/app/project/" + projectID.String() + "?tab=updates"
}

// ChannelHTML is the message an update becomes in a channel: what and where
// it stands, the note, and a link to the project (shown as its card).
func ChannelHTML(projectID uuid.UUID, projectName string, u *model.Update) string {
	link := projectLink(projectID)
	return fmt.Sprintf("<p><strong>%s update</strong> · %s</p>%s<p><a href=\"%s\">%s</a></p>",
		html.EscapeString(projectName), HealthLabels[u.Health], helpers.PlainTextToHTML(u.Body), html.EscapeString(link), html.EscapeString(link))
}

// Post stores an update from one of the project's admins, emails the
// project's members, and posts it in a channel when asked. A channel that
// refuses it doesn't undo the update; the person is told why.
func Post(ctx context.Context, user *userModels.UserInfo, projectID uuid.UUID, projectName string, in Input) (*Posted, error) {
	in, err := Check(in)
	if err != nil {
		return nil, err
	}
	authorID, authorName := authorOf(user)
	u, err := model.Add(projectID, authorID, in.Health, in.Body, in.SharedWithClient)
	if err != nil {
		return nil, err
	}
	out := &Posted{Update: View{Update: *u, AuthorName: authorName}}
	if out.Update.AuthorName == "" {
		out.Update.AuthorName = "Someone"
	}

	if members, err := projectBusiness.GetDgraphProjectMemberInfo(ctx, projectID.String(), user.UserDgraphInfo.Uid); err == nil && members != nil {
		ids := make([]string, 0, len(members.Members))
		for _, m := range members.Members {
			if m != nil && m.Uuid != "" {
				ids = append(ids, m.Uuid)
			}
		}
		notificationBusiness.DispatchProjectUpdate(authorID.String(), out.Update.AuthorName, projectID.String(), projectName,
			HealthLabels[u.Health], helpers.OneLine(u.Body, 600), u.Id.String(), ids)
	} else {
		helpers.LogErrorWithContext(ctx, "business/ProjectUpdate/Post members err: %+v", err)
	}

	if in.ChannelUUID != "" {
		post, err := sendBusiness.PrepareChannelPost(ctx, user, &postAdapter.InputCreateOrUpdatePostInfo{
			ChannelUuid: in.ChannelUUID,
			HTMLText:    ChannelHTML(projectID, projectName, u),
		})
		if err == nil {
			_, err = post.Commit(ctx)
		}
		if err != nil {
			out.ChannelError = "Posted on the project, but not in the channel."
			if r, ok := sendBusiness.AsRejection(err); ok && r.Status < 500 {
				out.ChannelError = "Posted on the project, but not in the channel: " + r.Msg + "."
			} else {
				helpers.LogErrorWithContext(ctx, "business/ProjectUpdate/Post channel err: %+v", err)
			}
		} else {
			out.Channel = post.Channel().Name
		}
	}
	return out, nil
}

// Edit changes an update; only its author may.
func Edit(ctx context.Context, user *userModels.UserInfo, projectID, id uuid.UUID, in Input) (*View, error) {
	in, err := Check(in)
	if err != nil {
		return nil, err
	}
	cur, err := model.Get(id, projectID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrNotFound
	}
	if authorID, _ := authorOf(user); cur.AuthorUUID != authorID {
		return nil, ErrNotYours
	}
	u, err := model.Edit(id, projectID, in.Health, in.Body, in.SharedWithClient)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrNotFound
	}
	v := views(ctx, []model.Update{*u})
	return &v[0], nil
}

// Delete removes an update: its author may, and so may the project's admins.
func Delete(ctx context.Context, user *userModels.UserInfo, projectID, id uuid.UUID, isAdmin bool) error {
	cur, err := model.Get(id, projectID)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrNotFound
	}
	if authorID, _ := authorOf(user); cur.AuthorUUID != authorID && !isAdmin {
		return ErrNotYours
	}
	ok, err := model.Delete(id, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
