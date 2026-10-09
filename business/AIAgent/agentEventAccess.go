package business

// What reaches an agent that nobody asked.
//
// An agent acts as the person it works for (its sponsor, created_by), so what
// sets it off on its own (an event it's bound to, a message it may answer
// unprompted) is held to what that person can see. It wasn't: an agent bound
// to a message event had every channel's messages quoted into its prompt,
// private channels its sponsor wasn't in included, and could repeat them
// anywhere its tools reach; one bound to a task event heard about every
// project's tasks.
//
// So an event reaches an agent only when its sponsor can see where it
// happened: a channel they can read, a project they're in, a table they can
// view (sees). And only the events the agent form offers can be bound at all
// (agentEvents). The rest either carry conversations (chat.created is a
// direct message) or exist to drive OneCamp's own dispatchers: agent.message
// carries delegation lineage, and only dispatchMentionAgents knows how to
// check it (hop budget, cycles, AuthorizeDelegation); bound as a plain event
// it would have run an agent on another agent's message with none of that.
// An unlisted event, an internal one added later included, is refused.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// eventScope is the place an event happened, which its payload names.
type eventScope int

const (
	inChannel eventScope = iota + 1 // channel_id
	inProject                       // project_id
	inTable                         // table_id
)

// agentEvents are the events an agent bound to one runs on (the agent form's
// EVENT_TRIGGER_OPTIONS, post.comment.created, and the GitHub events agents
// were bound to before they were withdrawn), with the place each happens in.
// Every one's payload names that place.
var agentEvents = map[string]eventScope{
	"post.created":         inChannel,
	"post.comment.created": inChannel,
	"channel.created":      inChannel,
	"user.joined":          inChannel,
	"task.created":         inProject,
	"task.status_changed":  inProject,
	"task.deleted":         inProject,
	"table.row.created":    inTable,
	"table.row.updated":    inTable,
	// A GitHub event is about the repo linked to a project.
	"github.pr.opened":           inProject,
	"github.pr.review_submitted": inProject,
	"github.check_run.completed": inProject,
	"github.issue.opened":        inProject,
}

// withdrawnEvents are events an agent can no longer be set up, or changed, to
// run on. A GitHub event carries what anyone who can write on the repository
// wrote, so its run would be asked for by nobody identified and refuse every
// tool (eventAsker): nothing useful is left for it to do, and anyone who can
// open an issue could still spend the agent's token budget. An agent bound to
// one before is not run on it at all (the trigger cache skips it); it can still
// be paused, deleted, or edited, keeping the event or choosing another trigger.
var withdrawnEvents = map[string]bool{
	"github.pr.opened":           true,
	"github.pr.review_submitted": true,
	"github.check_run.completed": true,
	"github.issue.opened":        true,
}

// bindableEvent reports whether an agent may be bound to the event ev: set up,
// or changed, to run on it.
func bindableEvent(ev string) bool {
	return boundEvent(ev) && !withdrawnEvents[strings.TrimSpace(ev)]
}

// boundEvent reports whether ev is an event agents are bound to, now or
// before. The trigger cache admits only these, and of them runs none that is
// withdrawn.
func boundEvent(ev string) bool {
	_, ok := agentEvents[strings.TrimSpace(ev)]
	return ok
}

// messageEvents are the bindable events whose payload is a person's message,
// quoted into the run as what it acts on (synthEventPrompt's "text").
var messageEvents = map[string]bool{
	"post.created":         true,
	"post.comment.created": true,
}

// eventAsker returns who an event agent's run is asked for by. An event that is
// a person's message is asked for by its author: their words drive the run as
// surely as a mention's do, and an unasked run would carry them out with the
// sponsor's whole reach. asker is "" for a message that names no author, such
// as an incoming webhook's post, whose words are whoever holds the webhook's
// URL: nobody identified, and never the webhook's creator, who wrote none of
// them. A GitHub event is asked for by nobody identified too: its title, body,
// review or check text is anyone's on a public repository. So is whatever the
// GitHub sync does in OneCamp (ctx carries helpers.WithGitHubOrigin): a task it
// creates from an issue, or closes with one, carries the same text. And so is a
// task a public form files (writtenOutside). asked is false for an event nobody
// wrote to the agent (a task moved, a row changed), which runs for the sponsor
// alone.
func eventAsker(ctx context.Context, eventType string, data map[string]interface{}) (asker string, asked bool) {
	switch {
	case writtenOutside(ctx), strings.HasPrefix(eventType, "github."):
		return "", true
	case messageEvents[eventType]:
		author, _ := data["author_id"].(string)
		return strings.TrimSpace(author), true
	}
	return "", false
}

// writtenOutside reports whether the work behind an event files words nobody in
// the workspace wrote: a change from GitHub, or what a visitor sent through a
// public form. Whoever it is filed as, nobody identified asked for it.
func writtenOutside(ctx context.Context) bool {
	return helpers.IsGitHubOrigin(ctx) || helpers.IsPublicSubmission(ctx)
}

// sponsorReach answers what the person an agent works for can see: the graph
// and the tables in production, fakes in tests.
type sponsorReach struct {
	readsChannel func(ctx context.Context, userUUID, channelID string) (bool, error)
	inProject    func(ctx context.Context, userUUID, projectID string) (bool, error)
	viewsTable   func(ctx context.Context, userUUID, tableID string) (bool, error)
}

var reach = sponsorReach{
	readsChannel: channelBusiness.ReadableBy,
	inProject:    projectBusiness.HasMember,
	viewsTable:   tableViewableBy,
}

// sees reports whether the person with sponsorUUID could see this occurrence
// of eventType: where it happened is a place they can see. An event that
// isn't bindable, an occurrence that doesn't name its place, and a lookup
// that fails are all a no.
func (r sponsorReach) sees(ctx context.Context, sponsorUUID, eventType string, data map[string]interface{}) bool {
	scope, ok := agentEvents[eventType]
	if !ok {
		return false
	}
	var key string
	var check func(ctx context.Context, userUUID, id string) (bool, error)
	switch scope {
	case inChannel:
		key, check = "channel_id", r.readsChannel
	case inProject:
		key, check = "project_id", r.inProject
	case inTable:
		key, check = "table_id", r.viewsTable
	default:
		return false
	}
	id, _ := data[key].(string)
	if id = strings.TrimSpace(id); id == "" {
		return false
	}
	yes, err := check(ctx, sponsorUUID, id)
	return err == nil && yes
}

var (
	errEventNotBindable = errors.New("an agent can be set off only by one of the events offered")
	errScopeChannel     = errors.New("choose channels the person this agent works for can read")
	errScopeProject     = errors.New("choose projects the person this agent works for is in")
)

// errGitHubEvent is what someone setting an agent up to run on a GitHub event
// is told (withdrawnEvents).
var errGitHubEvent = errors.New("GitHub events can't set off an agent: anyone who can write on the repository " +
	"writes what they carry, so the agent can't safely use its tools on them. Mention the agent, or run it on a schedule, instead")

// checkReach refuses to save an agent pointed somewhere the person it works
// for can't go: bound to an event it can't be, scoped to a channel they can't
// read or a project they aren't in, or watching moves in such a project. It
// would only ever be refused at run time, and it isn't theirs to name.
func (r sponsorReach) checkReach(ctx context.Context, sponsorUUID, triggerType, triggerConfigJSON, scopeJSON string) error {
	return r.checkReachKeeping(ctx, sponsorUUID, "", triggerType, triggerConfigJSON, scopeJSON)
}

// checkReachKeeping is checkReach for an agent being edited that is already
// bound to keptEvent: an edit that keeps a withdrawn event goes through (the
// agent still never runs on it), while choosing one anew is refused.
func (r sponsorReach) checkReachKeeping(ctx context.Context, sponsorUUID, keptEvent, triggerType, triggerConfigJSON, scopeJSON string) error {
	if triggerType == model.TriggerEvent {
		var cfg triggerConfig
		_ = json.Unmarshal([]byte(triggerConfigJSON), &cfg)
		switch ev := strings.TrimSpace(cfg.Event); {
		case withdrawnEvents[ev] && ev != strings.TrimSpace(keptEvent):
			return errGitHubEvent
		case withdrawnEvents[ev]:
			// Kept as it was by an edit to something else.
		case !bindableEvent(ev):
			return errEventNotBindable
		default:
			if p := strings.TrimSpace(cfg.MoveFilter.ProjectID); p != "" {
				if ok, err := r.inProject(ctx, sponsorUUID, p); err != nil || !ok {
					return errScopeProject
				}
			}
		}
	}
	var scope model.AgentScope
	if strings.TrimSpace(scopeJSON) != "" {
		if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
			return err
		}
	}
	for _, id := range scope.ChannelIDs {
		if ok, err := r.readsChannel(ctx, sponsorUUID, strings.TrimSpace(id)); err != nil || !ok {
			return errScopeChannel
		}
	}
	for _, id := range scope.ProjectIDs {
		if ok, err := r.inProject(ctx, sponsorUUID, strings.TrimSpace(id)); err != nil || !ok {
			return errScopeProject
		}
	}
	return nil
}

// tableViewableBy reports whether the person with userUUID can view a table
// (tableBusiness.ViewableBy), as a member or as an admin.
func tableViewableBy(ctx context.Context, userUUID, tableID string) (bool, error) {
	uid, err := uuid.Parse(userUUID)
	if err != nil {
		return false, nil
	}
	tid, err := uuid.Parse(tableID)
	if err != nil {
		return false, nil
	}
	user, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uid)
	if err != nil || user == nil || user.Id != uid {
		return false, err
	}
	return tableBusiness.ViewableBy(ctx, tableBusiness.Actor{UserID: uid, IsAdmin: user.IsAdmin}, tid)
}
