package business

// Who is told about an event, and what they're told: the notification sink's
// rules, one per kind of event that tells anyone. A comment on a doc is told by
// business/Comment's own sink (the participants rule); the rest are here.
//
//	doc.mentioned      the people a save newly @mentions, once a day per doc and
//	                   person, if they may read it. Someone mentioned who can't
//	                   read it is told nothing, and whoever wrote the mention is
//	                   asked in their inbox to share it.
//	doc.shared         the people who gained access: "Akash shared ‘Launch
//	                   notes’ with you".
//	doc.proposal.*     a new proposal tells the doc's owner (unless they chose
//	                   "none") and whoever follows it all; a decision tells the
//	                   proposer, or the person who asked the agent that made it.
//	thread.replied     a reply in a thread tells the thread's people (its
//	                   participants and anyone newly mentioned) and whoever
//	                   follows the doc all. Its comment isn't told doc-wide
//	                   (business/Comment ThreadReplyKey), and its notice is keyed
//	                   by the comment, as a doc-wide one is: one per comment.
//	thread.resolved,   the thread's people, in the app only.
//	thread.reopened
//	doc.access_requested
//	                   the people who can answer a request for access (the
//	                   doc's owner and its full-access people): "Sam asked to
//	                   edit Launch notes", with their note.
//	doc.access_answered
//	                   whoever asked. Approved, they hear who approved it and
//	                   what they may do; otherwise nothing about the doc beyond
//	                   what the page they asked from showed them, never who
//	                   answered. The approval's own doc.shared tells nobody.
//	doc.moved          a move into a project's or team's space, or out of one,
//	                   by someone else tells the page's owner (moved.go): its
//	                   space's people can open it now, or can't. Any other move
//	                   tells nobody.
//
// A thread's first comment is told doc-wide, as any comment (followers too);
// a thread re-attached to its words tells nobody (open tabs and the audit hear).
//
// Every rule tells only people who may read the doc now, as the Access
// Resolver decides it (business/Access ResolvePeople, asked once an event):
// through their own grant, a team's, the doc's space or the workspace, and
// never through a grant whose end has passed. Never the person who acted (for
// an agent, the person who asked it), and nothing about a doc in the trash.
// Telling (in the inbox, by push and by email, each once) is
// business/Notification's Tell.

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

// Kinds a rule writes that no Resource Event carries.
const (
	// kindMentionUnshared asks whoever mentioned someone in a doc they can't
	// open to share it with them.
	kindMentionUnshared = "doc.mention.unshared"
)

// Seams for tests: the doc's people, people by id, follows, telling, what the
// Access Resolver decides for many people at once, and a request for access.
var (
	people = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		return userDomain.GetPeopleByUUIDs(ctx, ids)
	}
	followModes = followModels.FollowModes
	tell        = notificationBusiness.Tell
	commentTold = commentDomain.GetCommentForTelling
	decide      = func(ctx context.Context, docUUID string, ids []string) (map[string]accessBusiness.Decision, error) {
		return accessBusiness.ResolvePeople(ctx, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID}, ids)
	}
	requestOf = func(ctx context.Context, id string) (*accessModels.Request, error) {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, accessModels.ErrNotFound
		}
		return accessModels.GetRequest(ctx, parsed)
	}
)

// notifies says whether an event is one the rules tell anyone about.
func notifies(e resourceEventBusiness.Event) bool {
	if e.ResourceKind != "doc" {
		return false
	}
	switch e.Kind {
	case resourceEventBusiness.DocMentioned, resourceEventBusiness.DocShared,
		resourceEventBusiness.DocProposalCreated, resourceEventBusiness.DocProposalAccepted,
		resourceEventBusiness.DocProposalDeclined, resourceEventBusiness.DocProposalWithdrawn,
		resourceEventBusiness.ThreadReplied, resourceEventBusiness.ThreadResolved, resourceEventBusiness.ThreadReopened,
		resourceEventBusiness.DocAccessRequested, resourceEventBusiness.DocAccessAnswered:
		return true
	case resourceEventBusiness.DocMoved:
		return tellsOfSpaceMove(e)
	}
	return false
}

// notify is the sink: the doc read once, then the event's rule.
func notify(ctx context.Context, e resourceEventBusiness.Event) {
	doc, err := docPeople(ctx, e.ResourceUUID)
	if err != nil || doc == nil {
		helpers.LogErrorWithContext(ctx, "business/Follow notify %s: doc %s couldn't be read, so nobody is told: %+v", e.Kind, e.ResourceUUID, err)
		return
	}
	if helpers.IsSoftDeleted(doc.DeletedAt) {
		return
	}
	cast := castOf(ctx, e)
	switch e.Kind {
	case resourceEventBusiness.DocMentioned:
		tellMentioned(ctx, e, doc, cast)
	case resourceEventBusiness.DocShared:
		tellShared(ctx, e, doc, cast)
	case resourceEventBusiness.DocProposalCreated, resourceEventBusiness.ThreadReplied:
		follows, err := followModes(ctx, "doc", doc.Uuid)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Follow notify: follows of doc %s couldn't be read: %+v", doc.Uuid, err)
		}
		if e.Kind == resourceEventBusiness.ThreadReplied {
			tellReplied(ctx, e, doc, cast, follows)
		} else {
			tellProposed(ctx, e, doc, cast, follows)
		}
	case resourceEventBusiness.ThreadResolved, resourceEventBusiness.ThreadReopened:
		tellThreadStatus(ctx, e, doc, cast)
	case resourceEventBusiness.DocAccessRequested:
		tellRequested(ctx, e, doc, cast)
	case resourceEventBusiness.DocAccessAnswered:
		tellAnswered(ctx, e, doc, cast)
	case resourceEventBusiness.DocMoved:
		tellSpaceMoved(ctx, e, doc, cast)
	default:
		tellDecided(ctx, e, doc, cast)
	}
}

// readersOf is, of ids, who may read the doc now, by the Access Resolver,
// asked once. ok is false when it couldn't say, and then nobody is told.
func readersOf(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, ids []string) (map[string]bool, bool) {
	decided, ok := decisionsOf(ctx, e, doc, ids)
	if !ok {
		return nil, false
	}
	out := make(map[string]bool, len(decided))
	for id, d := range decided {
		out[id] = d.Level >= accessBusiness.LevelView
	}
	return out, true
}

// decisionsOf is each of ids' decision on the doc, by the Access Resolver,
// asked once. ok is false when it couldn't say, and then nobody is told.
func decisionsOf(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, ids []string) (map[string]accessBusiness.Decision, bool) {
	decided, err := decide(ctx, doc.Uuid, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Follow notify %s: who may read doc %s couldn't be decided, so nobody is told: %+v", e.Kind, doc.Uuid, err)
		return nil, false
	}
	return decided, true
}

// cast is who an event names: its actor, its agent, and its subjects.
type cast struct {
	actor, agent *dgraphStruct.DgraphUser
	byID         map[string]*dgraphStruct.DgraphUser
}

func (c cast) name(id string) string {
	if u := c.byID[id]; u != nil {
		return u.DisplayName()
	}
	return "Someone"
}

// castOf reads everyone the event names, in one query.
func castOf(ctx context.Context, e resourceEventBusiness.Event) cast {
	ids := append([]string{e.Actor.UserUUID, e.Actor.AgentUUID}, e.Subjects...)
	c := cast{byID: map[string]*dgraphStruct.DgraphUser{}}
	found, err := people(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Follow notify %s: the people in it couldn't be read: %+v", e.Kind, err)
	}
	for _, u := range found {
		if u != nil {
			c.byID[u.Uuid] = u
		}
	}
	c.actor, c.agent = c.byID[e.Actor.UserUUID], c.byID[e.Actor.AgentUUID]
	return c
}

// who is the actor as a sentence opens: "Akash", or for an agent "Release
// Captain" (the person who asked it follows: "for Akash").
func (c cast) who() string {
	switch {
	case c.agent != nil:
		return c.agent.DisplayName()
	case c.actor != nil:
		return c.actor.DisplayName()
	}
	return "Someone"
}

func (c cast) forWhom() string {
	if c.agent != nil && c.actor != nil {
		return " for " + c.actor.DisplayName()
	}
	return ""
}

func titleOf(doc *dgraphStruct.DgraphDoc) string {
	if t := strings.TrimSpace(doc.Title); t != "" {
		return t
	}
	return "Untitled"
}

func day(at time.Time) string { return at.UTC().Format("2006-01-02") }

func baseNotice(e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) notificationBusiness.Notice {
	return notificationBusiness.Notice{
		Kind:          string(e.Kind),
		ResourceKind:  "doc",
		ResourceUUID:  doc.Uuid,
		ResourceTitle: doc.Title,
		Actor:         c.actor,
		Agent:         c.agent,
		Path:          "/app/doc/" + doc.Uuid,
	}
}

// tellMentioned tells readers newly mentioned, and asks the mention's author
// to share the doc with anyone mentioned who can't open it.
func tellMentioned(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	can, ok := readersOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var readers []notificationBusiness.NoticeRecipient
	var outside []string
	for _, id := range e.Subjects {
		if can[id] {
			readers = append(readers, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "mention"})
		} else {
			outside = append(outside, id)
		}
	}
	n := baseNotice(e, doc, c)
	n.DedupKey = "doc.mentioned|" + doc.Uuid + "|" + day(e.At)
	n.Email = notificationBusiness.EventDocMention
	if c.actor != nil {
		n.Title = c.who() + " mentioned you in " + titleOf(doc)
	} else {
		n.Title = "You were mentioned in " + titleOf(doc)
	}
	tell(ctx, n, readers)

	authors := authorsOf(e)
	if len(outside) == 0 || len(authors) == 0 {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range authors {
		to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "author", InboxOnly: true})
	}
	for _, id := range outside {
		ask := baseNotice(e, doc, c)
		ask.Kind = kindMentionUnshared
		// The author is told about their own mention: nobody is "the actor".
		ask.Actor, ask.Agent = nil, nil
		ask.Data = map[string]string{"person": id, "person_name": c.name(id)}
		ask.DedupKey = kindMentionUnshared + "|" + doc.Uuid + "|" + id + "|" + day(e.At)
		ask.Title = c.name(id) + " can't open " + titleOf(doc)
		ask.Body = "You mentioned them, but the doc isn't shared with them, so they weren't told. Share it to let them see it."
		tell(ctx, ask, to)
	}
}

// authorsOf is who wrote an event's mentions: its actor, or with several
// people in one save, each of them ("contributors").
func authorsOf(e resourceEventBusiness.Event) []string {
	if e.Actor.UserUUID != "" {
		return []string{e.Actor.UserUUID}
	}
	var out []string
	for _, id := range strings.Split(e.Data["contributors"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// tellShared tells the people who gained access, at the level they were
// given. A share that approves a request for access is told by the answer
// (tellAnswered), so its asker hears once.
func tellShared(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	if e.Data["request"] != "" {
		return
	}
	can, ok := readersOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if can[id] {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "subject"})
		}
	}
	level := e.Data["level"]
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"level": level}
	n.DedupKey = "doc.shared|" + doc.Uuid + "|" + e.At.UTC().Format(time.RFC3339Nano)
	n.Email = notificationBusiness.EventDocShared
	n.Title = c.who() + " shared ‘" + titleOf(doc) + "’ with you" + c.forWhom()
	n.Body = levelLine(level)
	tell(ctx, n, to)
}

// levelLine is what someone may do at a level, as a notice's line says it.
func levelLine(level string) string {
	switch level {
	case "edit", "full":
		return "You can edit it."
	case "comment":
		return "You can read it and comment on it."
	}
	return "You can read it."
}

// tellProposed tells the doc's owner, unless they chose "none", and whoever
// follows it all, about a new proposal.
func tellProposed(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast, follows map[string]string) {
	owner := ""
	if o := docDomain.OwnerOf(doc); o != nil {
		owner = o.Uuid
	}
	followers := sortedFollowers(follows, ModeAll)
	can, ok := readersOf(ctx, e, doc, append([]string{owner}, followers...))
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	seen := map[string]bool{}
	add := func(id, reason string) {
		if id != "" && !seen[id] && can[id] {
			seen[id] = true
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: reason})
		}
	}
	if owner != "" && follows[owner] != ModeNone {
		add(owner, "owner")
	}
	for _, id := range followers {
		add(id, "follower")
	}
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"proposal_id": e.Data["proposal_id"]}
	n.DedupKey = "doc.proposal.created|" + e.Data["proposal_id"]
	n.Email = notificationBusiness.EventDocProposal
	n.Title = c.who() + " proposed a change to " + titleOf(doc) + c.forWhom()
	n.Body = "Review it to accept or decline it."
	tell(ctx, n, to)
}

// tellDecided tells the proposer (or whoever asked the agent that proposed it)
// what became of their proposal.
func tellDecided(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	can, ok := readersOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if can[id] {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "subject"})
		}
	}
	verb := map[resourceEventBusiness.Kind]string{
		resourceEventBusiness.DocProposalAccepted:  "accepted",
		resourceEventBusiness.DocProposalDeclined:  "declined",
		resourceEventBusiness.DocProposalWithdrawn: "withdrew",
	}[e.Kind]
	what := "your proposed change to "
	if e.Data["author_kind"] == "agent" {
		what = "the change proposed for you to "
	}
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"proposal_id": e.Data["proposal_id"], "status": e.Data["status"]}
	n.DedupKey = string(e.Kind) + "|" + e.Data["proposal_id"] + "|" + e.At.UTC().Format(time.RFC3339Nano)
	n.Email = notificationBusiness.EventDocProposal
	n.Title = c.who() + " " + verb + " " + what + titleOf(doc)
	tell(ctx, n, to)
}

// tellReplied tells a thread's people, and whoever follows the doc all, of a
// reply: in the inbox, by push and by email. Someone whose feed lists the
// comment already (the doc's owner, anyone it mentions) is pushed and emailed
// without a second inbox item. A participant who chose "none" hears only when
// the reply mentions them.
func tellReplied(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast, follows map[string]string) {
	commentID := e.Data["comment_id"]
	if commentID == "" {
		return
	}
	text, mentionedList, err := commentTold(ctx, commentID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Follow notify: reply %s couldn't be read, so it's told without its words: %+v", commentID, err)
	}
	mentioned := map[string]bool{}
	for _, id := range mentionedList {
		mentioned[id] = true
	}
	// The feed lists a comment on what someone made, so the maker (not an
	// owner it was handed to) has it listed already.
	maker := ""
	if doc.CreatedBy != nil {
		maker = doc.CreatedBy.Uuid
	}
	can, ok := readersOf(ctx, e, doc, append(append([]string{}, e.Subjects...), sortedFollowers(follows, ModeAll)...))
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	seen := map[string]bool{}
	add := func(id, reason string) {
		if id == "" || seen[id] || !can[id] {
			return
		}
		seen[id] = true
		to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: reason, Listed: id == maker || mentioned[id]})
	}
	for _, id := range e.Subjects {
		if follows[id] == ModeNone && !mentioned[id] {
			continue
		}
		add(id, "participant")
	}
	for _, id := range sortedFollowers(follows, ModeAll) {
		add(id, "follower")
	}
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"thread_id": e.Data["thread_id"], "comment_id": commentID}
	// The key a doc-wide comment notice has: one inbox item per comment.
	n.DedupKey = "comment|" + commentID
	n.Email = notificationBusiness.EventDocComment
	n.Title = c.who() + " replied in a thread on " + titleOf(doc)
	n.Body = helpers.TruncateRunes(helpers.HTMLToPlainText(text), 300)
	n.Path = "/app/doc/" + doc.Uuid + "/comment"
	tell(ctx, n, to)
}

// tellThreadStatus tells a thread's people, in the app, that it was resolved
// or reopened.
func tellThreadStatus(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	can, ok := readersOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if can[id] {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "participant", InboxOnly: true})
		}
	}
	verb := "resolved"
	if e.Kind == resourceEventBusiness.ThreadReopened {
		verb = "reopened"
	}
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"thread_id": e.Data["thread_id"], "status": e.Data["status"]}
	n.DedupKey = string(e.Kind) + "|" + e.Data["thread_id"] + "|" + e.At.UTC().Format(time.RFC3339Nano)
	n.Title = c.who() + " " + verb + " a thread on " + titleOf(doc)
	n.Path = "/app/doc/" + doc.Uuid + "/comment"
	tell(ctx, n, to)
}

// askVerb is what someone asks to do at a level: "edit", "comment on", "view".
func askVerb(level string) string {
	switch level {
	case "edit":
		return "edit"
	case "comment":
		return "comment on"
	}
	return "view"
}

// tellRequested tells the people who can answer a request for access, the
// doc's owner and its full-access people as the resolver decides now (the
// request's subjects, read when it was made), who asked and for what: "Sam
// Rivera asked to edit Launch notes", their note under it. They can read the
// doc, so it is named. Asking again for another level tells them again; a
// changed note alone doesn't, and a request answered already tells nobody.
func tellRequested(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	request := e.Data["request"]
	if request == "" {
		return
	}
	note := ""
	switch req, err := requestOf(ctx, request); {
	case err != nil:
		// Told without the note, rather than not at all.
		helpers.LogErrorWithContext(ctx, "business/Follow notify: request %s couldn't be read, so it's told without its note: %+v", request, err)
	case req == nil || req.Status != accessModels.StatusPending:
		return
	default:
		note = strings.TrimSpace(req.Note)
	}
	decided, ok := decisionsOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if decided[id].Level == accessBusiness.LevelFull {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "subject"})
		}
	}
	level := e.Data["level"]
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"request": request, "level": level}
	n.DedupKey = string(e.Kind) + "|" + request + "|" + level
	n.Email = notificationBusiness.EventDocShared
	n.Title = c.who() + " asked to " + askVerb(level) + " " + titleOf(doc)
	n.Body = "Approve or decline it from Share, on the page."
	if note != "" {
		n.Body = "“" + helpers.TruncateRunes(note, 300) + "” " + n.Body
	}
	tell(ctx, n, to)
}

// tellAnswered tells whoever asked for access what became of it. Approved and
// able to read the doc now, they hear who approved it, the doc's title and
// what they may do. Otherwise (declined, or taken off it again since) they
// hear nothing about the doc beyond what the page they asked from showed
// them: that their request was, or wasn't, approved. Who declined it is never
// said, and a doc they can't open is never named, in the inbox, a push or an
// email.
func tellAnswered(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	request := e.Data["request"]
	if request == "" || len(e.Subjects) == 0 {
		return
	}
	asker := e.Subjects[0]
	status, level := e.Data["status"], e.Data["level"]
	readable := false
	if decided, ok := decisionsOf(ctx, e, doc, []string{asker}); ok {
		readable = decided[asker].Level >= accessBusiness.LevelView
	}
	approved := status == accessModels.StatusApproved
	n := notificationBusiness.Notice{
		Kind:         string(e.Kind),
		ResourceKind: "doc",
		ResourceUUID: doc.Uuid,
		Data:         map[string]string{"request": request, "status": status, "level": level},
		DedupKey:     string(e.Kind) + "|" + request,
		Email:        notificationBusiness.EventDocShared,
		Path:         "/app/doc/" + doc.Uuid,
	}
	switch {
	case approved && readable:
		n.ResourceTitle, n.Actor, n.Agent = doc.Title, c.actor, c.agent
		n.Title = c.who() + " approved your request for ‘" + titleOf(doc) + "’" + c.forWhom()
		n.Body = levelLine(level)
	case readable:
		n.ResourceTitle = doc.Title
		n.Title = "Your request to " + askVerb(level) + " ‘" + titleOf(doc) + "’ wasn't approved"
		n.Body = "You can ask again, with a note that says why you need it."
	case approved:
		n.Closed = true
		n.Title = "Your request for access was approved"
	default:
		n.Closed = true
		n.Title = "Your request for access wasn't approved"
		n.Body = "You can ask again, with a note that says why you need it."
	}
	tell(ctx, n, []notificationBusiness.NoticeRecipient{{UserUUID: asker, Reason: "subject"}})
}
