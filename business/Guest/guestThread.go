package business

// A doc's conversation with its guests (docs C5-2). A guest's comment can be
// answered in place by a member, "Visible to guest"; whoever may moderate the
// doc's comments can remove a guest's; one guest on one link is one author on
// every comment; and a client gives the doc a verdict, as on a task.
//
// A member's reply passes the very limits a guest's comment does
// (sanitizeGuestCommentBody: no markup, 4,000 characters), and the guest sees
// the member as "<name> from <workspace>", never an address.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ClientReview"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// GuestAuthorID is one guest on one link, the same on every comment (B13):
// the link and the name they gave, never their comment's own id. Pure.
func GuestAuthorID(grantID uuid.UUID, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(name))))
	return "guest-" + grantID.String()[:8] + "-" + hex.EncodeToString(sum[:4])
}

// teamLabel is how a guest sees a member who answers them. Pure.
func teamLabel(name, workspace string) string {
	name = sanitizeGuestName(name)
	if name == "" || strings.Contains(name, "@") {
		// A member named by their address (a display name never set) is
		// never shown to a guest by it.
		name = "A teammate"
	}
	if w := sanitizeGuestName(workspace); w != "" {
		return name + " from " + w
	}
	return name
}

// teamName is the member's own name in a reply's label, without " from
// <workspace>"; the whole label when the workspace has been renamed since.
// Pure.
func teamName(label, workspace string) string {
	if w := sanitizeGuestName(workspace); w != "" {
		return strings.TrimSuffix(label, " from "+w)
	}
	return label
}

// Seams for tests.
var (
	commentOf   = guestModel.GetGuestComment
	saveReply   = guestModel.CreateMemberReply
	removeOne   = guestModel.RemoveGuestComment
	docDecision = func(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	}
	workspaceName = settingsBusiness.WorkspaceName
	guestAccessOn = settingsBusiness.GuestAccessEnabled
)

// ReplyToGuest answers a guest's comment where the guest sees it, as who
// (named name): for someone who may comment on the doc, while guest access is
// on. The reply answers a guest's own comment, never another reply.
func ReplyToGuest(ctx context.Context, who accessBusiness.Subject, name, docUUID string, parentID uuid.UUID, body string) (*GuestCommentView, error) {
	if !guestAccessOn() {
		return nil, ErrGuestDisabled
	}
	d, err := docDecision(ctx, who, docUUID)
	if err != nil {
		return nil, err
	}
	if d.Deleted || d.Level < accessBusiness.LevelComment {
		if d.Level == accessBusiness.LevelNone {
			return nil, ErrNotFound
		}
		return nil, ErrForbidden
	}
	parent, err := commentOf(ctx, docUUID, parentID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.AuthorKind == guestModel.AuthorMember || parent.ParentID != nil {
		return nil, ErrNotFound
	}
	plain, err := sanitizeGuestCommentBody(body)
	if err != nil {
		return nil, err
	}
	author, err := uuid.Parse(who.UserUUID)
	if err != nil || who.AgentUUID != "" {
		// An agent never speaks to a client for someone.
		return nil, ErrForbidden
	}
	label := teamLabel(name, workspaceName())
	id, at, err := saveReply(ctx, parent, author, label, plain)
	if err != nil {
		return nil, err
	}
	return &GuestCommentView{ID: id, GuestName: label, Body: plain, CreatedAt: at, ParentID: &parent.Id, FromTeam: true, AuthorID: author.String(), AuthorName: teamName(label, workspaceName())}, nil
}

// RemoveGuestComment takes a guest's comment (and its replies) off the doc,
// for someone the caller found may moderate it; false when there was none.
func RemoveGuestComment(ctx context.Context, docUUID string, id uuid.UUID) (bool, error) {
	return removeOne(ctx, docUUID, id)
}

// DocReviewView is a client's verdict on a doc, as the team sees it.
type DocReviewView struct {
	Decision  string `json:"decision"`
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
}

func docReviewView(r *reviewModel.DocReview) *DocReviewView {
	if r == nil {
		return nil
	}
	return &DocReviewView{Decision: r.Decision, Name: r.Name, Note: r.Note, CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")}
}

// Seams for tests.
var (
	addDocReview  = reviewModel.AddDoc
	postGuestNote = CreateGuestDocComment
	latestDoc     = reviewModel.LatestForDoc
)

// ReviewDocAsGuest records a client's verdict on the doc their comment link
// opens, and says it in the doc's guest conversation, which tells its keepers.
func ReviewDocAsGuest(ctx context.Context, grant *guestModel.GuestGrant, name, decision, note string) (*DocReviewView, error) {
	if !guestAccessOn() {
		return nil, ErrGuestDisabled
	}
	if grant == nil || grant.ResourceType != guestModel.ResourceDoc {
		return nil, ErrInvalidGrant
	}
	if grant.Capability != guestModel.CapabilityComment {
		return nil, ErrForbidden
	}
	text, note, err := ReviewText(decision, note)
	if err != nil {
		return nil, err
	}
	r, err := addDocReview(grant.ResourceID, grant.Id, decision, SanitizeGuestName(name), note)
	if err != nil {
		return nil, err
	}
	// The verdict stands whatever happens to its line in the conversation,
	// which tells the doc's keepers.
	_, _ = postGuestNote(ctx, grant, name, text)
	return docReviewView(r), nil
}

// LatestDocReview is a doc's newest client verdict, for its team.
func LatestDocReview(docUUID string) (*DocReviewView, error) {
	r, err := latestDoc(docUUID)
	return docReviewView(r), err
}
