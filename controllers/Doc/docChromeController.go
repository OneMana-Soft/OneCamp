package docController

// A doc's page chrome over HTTP (router/docsChrome.go; docs programme B6):
// opening a doc with its look and lock, changing its look, what links to it,
// a doc in the trash as its page shows it, and inviting people to it as an
// editor. The Access Resolver decides who may do each, first; a doc the
// person can't open answers as every doc check does (404, or 410 to someone
// who could open it before it was deleted).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	business "github.com/akashc777/OneCamp/business/Doc"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetDocPage's two reads: seams, so a test can fail the second.
var (
	docForPage = business.GetDocByDocUUID
	accessNow  = business.ShowAccessNow
)

// GetDocPage opens a doc (GET /doc/getDocInfo/{doc_uuid}, the one way a doc
// is opened since GetDocInfo went): the doc with the person's counts, and its
// look (icon, cover, width, font, text size), its lock, its owner and how
// many docs link to it, all in the one read, so the page draws them with the
// doc and not a round trip later. 403 is a live doc the person can't open (the
// page offers Request access); 410 one in the trash they could open, with
// can_restore; 503 when the grants that end can't be read.
func GetDocPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := accessBusiness.RequestSubject(r)
	parsed, err := uuid.Parse(chi.URLParam(r, "doc_uuid"))
	if err != nil {
		badRequest(w, "That isn't a doc.")
		return
	}
	id := parsed.String()
	viewer := business.Viewer{UID: user.UserDgraphInfo.Uid, UUID: user.UserDgraphInfo.Uuid}
	doc, err := docForPage(ctx, id, user.UserDgraphInfo.Uid)
	if docMissing(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocPage %s err: %+v", id, err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't load the doc. Try again."})
		return
	}
	if business.IsDeleted(doc) {
		if !business.CouldRead(doc, viewer.UUID) {
			helpers.WriteDocNotFound(w)
			return
		}
		docDeleted(w, business.CanRestore(doc, viewer, user.UserPostgresInfo.IsAdmin))
		return
	}
	if !business.CanRead(doc, viewer.UUID) {
		refuseDoc(ctx, w, user, id)
		return
	}
	doc.MqttTopic = helpers.GetMqttTopicForDoc(doc.Uuid)
	// The counts the app reads are the resolver's decision, with a grant whose
	// end has passed gone before the minute sweep takes it away (the A5×A6
	// join), so opening a doc agrees with its lists, sockets and notices.
	now, err := accessNow(ctx, doc, viewer)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocPage %s: the grants that end couldn't be read: %+v", id, err)
		helpers.WriteRetryLater(w, "Couldn't open the doc just now.")
		return
	}
	if now.Level < accessBusiness.LevelView {
		refuseDoc(ctx, w, user, id)
		return
	}
	// The look, the lock and the owner's name come with the doc, in its one
	// read (GetDgraphDocByUUID); the owner answers as doc_owner.
	if doc.OwnerNamed != nil && doc.OwnerNamed.Uuid != "" {
		doc.Owner = doc.OwnerNamed
	}
	doc.OwnerNamed = nil
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got docInfo successfully!", "data": doc})
}

// lookIn is a change to a doc's look as it is asked for.
type lookIn struct {
	DocUUID string `json:"doc_uuid"`
	business.Look
}

// lookView is a doc's look as it is after a change.
func lookView(id string, look business.Look) helpers.Envolope {
	out := helpers.Envolope{"doc_uuid": id}
	if look.Icon != nil {
		out["doc_icon"] = *look.Icon
	}
	if look.Cover != nil {
		out["doc_cover_key"] = *look.Cover
	}
	if look.CoverOffset != nil {
		out["doc_cover_offset"] = *look.CoverOffset
	}
	if look.FullWidth != nil {
		out["doc_full_width"] = *look.FullWidth
	}
	if look.Font != nil {
		out["doc_font"] = *look.Font
	}
	if look.SmallText != nil {
		out["doc_small_text"] = *look.SmallText
	}
	return out
}

// SetDocLook changes a doc's look (POST /doc/look {doc_uuid, icon?, cover?,
// cover_offset?, full_width?, font?, small_text?}): for someone who may edit
// it, so a hard lock keeps an editor off it. A cover uploaded to the doc must
// be one of the doc's own files.
func SetDocLook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in lookIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't a change to how the doc looks.")
		return
	}
	if !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc.")
		return
	}
	d, ok := decideFor(ctx, w, who, in.DocUUID, "SetDocLook")
	if !ok {
		return
	}
	if d.Level < accessBusiness.LevelEdit {
		msg := "You can't change how this doc looks."
		if d.Locked && d.Unlocked >= accessBusiness.LevelEdit {
			msg = "This doc is locked. Someone with full access can unlock it."
		}
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": msg})
		return
	}
	var bad business.ErrBadLook
	if err := in.Look.Check(in.DocUUID); errors.As(err, &bad) {
		badRequest(w, bad.Why)
		return
	}
	if file, ok := in.Look.CoverFile(); ok {
		a, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, file, postgresStruct.ATTACHMENT_SRC_DOC)
		if err != nil || a == nil || !strings.EqualFold(a.SrcValue, in.DocUUID) || !a.DeletedAt.IsZero() {
			badRequest(w, "That image isn't one of this doc's files. Upload it again.")
			return
		}
	}
	err := business.SetLook(ctx, in.DocUUID, in.Look, business.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID})
	switch {
	case errors.As(err, &bad):
		badRequest(w, bad.Why)
	case errors.Is(err, business.ErrDocDeleted):
		docGone(w, true)
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteDocNotFound(w)
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/SetDocLook %s err: %+v", in.DocUUID, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "The change wasn't saved. Try again."})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved.", "data": lookView(in.DocUUID, in.Look)})
	}
}

// backlink is a doc that links to this one, as "Linked from" shows it.
type backlink struct {
	UUID      string     `json:"doc_uuid"`
	Title     string     `json:"doc_title"`
	Icon      string     `json:"doc_icon,omitempty"`
	UpdatedAt *time.Time `json:"doc_updated_at,omitempty"`
}

// GetDocBacklinks is what links to a doc, together (GET
// /doc/backlinks/{doc_uuid}): the tasks and projects that link it (in the
// person's projects) and the docs whose bodies link to it (that they may
// open; a doc they can't is never named, I12), with how many in all.
func GetDocBacklinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	id := chi.URLParam(r, "doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		badRequest(w, "That isn't a doc.")
		return
	}
	if _, ok := decideFor(ctx, w, who, id, "GetDocBacklinks"); !ok {
		return
	}
	tasks, projects, err := entityLinkBusiness.GetReverseLinksForRef(ctx, "doc", id, who.UserUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocBacklinks %s links: %+v", id, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't load what links here. Try again."})
		return
	}
	mentioners, err := business.Mentioners(ctx, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocBacklinks %s docs: %+v", id, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't load what links here. Try again."})
		return
	}
	res := make([]accessBusiness.Resource, 0, len(mentioners))
	byID := map[string]backlink{}
	for _, m := range mentioners {
		if m == nil || m.Uuid == "" || strings.EqualFold(m.Uuid, id) {
			continue
		}
		res = append(res, docRes(m.Uuid))
		byID[m.Uuid] = backlink{UUID: m.Uuid, Title: m.Title, Icon: m.Icon, UpdatedAt: m.UpdatedAt}
	}
	kept, err := accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocBacklinks %s access: %+v", id, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't load what links here. Try again."})
		return
	}
	docs := make([]backlink, 0, len(kept))
	for _, k := range kept {
		docs = append(docs, byID[k.UUID])
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"tasks": tasks, "projects": projects, "docs": docs, "count": len(tasks) + len(projects) + len(docs),
		// More docs link here than were read: the page says "200+".
		"capped": len(mentioners) >= business.BacklinkCap,
	}})
}

// GetTrashedDoc is a doc in the trash as its page shows it (GET
// /doc/trashed/{doc_uuid}): what it said and looked like, who deleted it and
// when, how many pages went with it, and whether the person may restore it or
// delete it for good. Only for someone who could open it before; anyone else
// is told nothing (404).
func GetTrashedDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := accessBusiness.RequestSubject(r)
	id := chi.URLParam(r, "doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		badRequest(w, "That isn't a doc.")
		return
	}
	p, err := business.TrashedPageOf(ctx, id, business.Viewer{UID: user.UserDgraphInfo.Uid, UUID: user.UserDgraphInfo.Uuid}, user.UserPostgresInfo.IsAdmin)
	switch {
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, business.ErrStillLive):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_in_trash", "msg": "That doc isn't in the trash."})
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/GetTrashedDoc %s err: %+v", id, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't load the doc. Try again."})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
	}
}

// InviteToDoc adds people and teams to a doc as an editor may (POST
// /doc/access/invite, the body of /doc/access/update with adds alone): at the
// inviter's own level or below, never anyone the doc names already. The
// demo's own docs keep their sharing from its shared visitor.
func InviteToDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in accessChangeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't an invite.")
		return
	}
	if in.GuestLinks != "" {
		badRequest(w, "An invite doesn't change guest links.")
		return
	}
	c, ok := changeOf(w, in, time.Now())
	if !ok {
		return
	}
	if _, ok := decideFor(ctx, w, who, in.DocUUID, "InviteToDoc"); !ok {
		return
	}
	if refuseDemo(ctx, w, user, in.DocUUID) {
		return
	}
	ex, err := accessBusiness.Invite(ctx, who, c)
	switch {
	case errors.Is(err, accessBusiness.ErrAboveYourLevel):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "above_your_level", "msg": "You can invite people at your own level of access or below."})
	case errors.Is(err, accessBusiness.ErrNotAnInvite):
		badRequest(w, "Inviting only adds people. Ask someone with full access to change anything else.")
	case errors.Is(err, accessBusiness.ErrAlreadyShared):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "already_shared", "msg": "They already have access. Someone with full access can change it."})
	case errors.Is(err, accessBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": "Only people who can edit this doc can invite others."})
	case err != nil:
		accessFailed(ctx, w, "InviteToDoc", err)
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Invited.", "data": explainedView(ex)})
	}
}
