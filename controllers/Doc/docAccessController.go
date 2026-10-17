package docController

// Who may do what with a doc, over HTTP (router/docsAccess.go; docs programme
// A5): my level and why, the sharing panel, changing it (with what a change
// would do first), handing a doc on, an admin's recovery, the lock, and asking
// for access. The Access Resolver (business/Access) decides; nothing here does.
//
// Also the handlers that take over three older routes at wiring: the sharing
// list and its change in the shape the current web app sends and reads
// (/doc/getDocPermissions, /doc/updateDocPermissions), removing a comment,
// which a doc's owner, full-access people and admins may now do to anyone's,
// and the live socket's authorisation (/docColab/authorize), which answers
// with the person's level.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Access/level"
	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	business "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxGrantsPerChange bounds one change's additions and removals.
const maxGrantsPerChange = 200

// accessFailed answers an error from the Access Resolver.
func accessFailed(ctx context.Context, w http.ResponseWriter, where string, err error) {
	switch {
	case errors.Is(err, business.ErrDocDeleted):
		docGone(w, true)
	case errors.Is(err, accessBusiness.ErrNotFound), errors.Is(err, accessBusiness.ErrUnknownKind):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, accessBusiness.ErrNotOwner):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_owner", "msg": "Only the doc's owner can do that."})
	case errors.Is(err, accessBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": "You can't change who has access to this doc."})
	case errors.Is(err, accessBusiness.ErrNoSuchPrincipal):
		badRequest(w, "That person or team isn't in this workspace.")
	case errors.Is(err, accessBusiness.ErrAlreadyHas):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "already", "msg": "You can already do that."})
	case errors.Is(err, accessBusiness.ErrNoRequest):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_request", "msg": "That request isn't open any more."})
	case errors.Is(err, accessBusiness.ErrBadLevel):
		badRequest(w, "Access is view, comment, edit or full.")
	case errors.Is(err, accessBusiness.ErrNobodyAnswers):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "nobody_answers", "msg": "Nobody can answer a request for this doc."})
	case errors.Is(err, business.ErrTooMany):
		// A change applied to the pages under one, past what one change takes.
		tooManyPages(w)
	default:
		helpers.LogErrorWithContext(ctx, "controllers/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Something went wrong. Try again."})
	}
}

// decideFor is the person's decision on a doc, answering for them when it
// isn't one they may know about: 404 for none, 410 or 404 for a deleted doc
// (410 when they could open it before). ok is false when it answered.
func decideFor(ctx context.Context, w http.ResponseWriter, who accessBusiness.Subject, docUUID, where string) (accessBusiness.Decision, bool) {
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(docUUID))
	if err != nil {
		accessFailed(ctx, w, where, err)
		return d, false
	}
	switch {
	case d.Deleted:
		docGone(w, d.Was >= accessBusiness.LevelView)
		return d, false
	case d.Level == accessBusiness.LevelNone:
		helpers.WriteDocNotFound(w)
		return d, false
	}
	return d, true
}

// demoAccessMsg is why the demo keeps who may open its own docs, and their
// owners and locks, from its shared visitor.
const demoAccessMsg = "Everyone who opens the demo shares this doc, so who can open it can't be changed here. " +
	"Anything you make yourself, you can share."

// demoRequestMsg is why the demo's shared visitor can't ask for access.
const demoRequestMsg = "Everyone who opens the demo shares this account, so it can't ask for access to anyone's doc. " +
	"Install OneCamp free to try sharing with your own team."

// demoKeepsDoc says whether the demo keeps a doc's sharing from the person:
// they are the demo's shared visitor, and it is one of the demo's own docs
// (business/DemoGuard), which every visitor after them opens.
func demoKeepsDoc(ctx context.Context, user userModels.UserInfo, docUUID string) bool {
	if !helpers.IsDemoVisitor(user.UserPostgresInfo.EmailID) {
		return false
	}
	var created *time.Time
	if rows, err := business.DocRows(ctx, []string{docUUID}, user.UserDgraphInfo.Uid); err == nil && len(rows) > 0 {
		created = rows[0].CreatedAt
	}
	return demoGuard.KeepsFromVisitor(user.UserPostgresInfo.EmailID, created)
}

// refuseDemo answers, and reports, a change to one of the demo's own docs by
// its shared visitor.
func refuseDemo(ctx context.Context, w http.ResponseWriter, user userModels.UserInfo, docUUID string) bool {
	if !demoKeepsDoc(ctx, user, docUUID) {
		return false
	}
	helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": demoAccessMsg})
	return true
}

// canRequest says whether a request from the person for more access to a
// doc than d gives would reach someone who can answer it
// (accessBusiness.CanAsk): the pages offer Request access by it, and only
// then (docs incident, 14 Oct 2026, D3). Never for the demo's shared visitor:
// a request from everyone who opens the demo can't be answered for one of
// them, and approving it would open the doc to all of them. A failure to tell
// is no.
func canRequest(ctx context.Context, user userModels.UserInfo, who accessBusiness.Subject, docUUID string, d accessBusiness.Decision) bool {
	if helpers.IsDemoVisitor(user.UserPostgresInfo.EmailID) {
		return false
	}
	ok, err := accessBusiness.CanAsk(ctx, who, docRes(docUUID), d)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/canRequest %s: whether anyone can answer a request couldn't be told: %+v", docUUID, err)
		return false
	}
	return ok
}

// refuseDoc answers a live doc the person can't open: 403, with the code the
// page reads (no_access) and whether a request for access would reach someone
// who can answer it (canRequest), and nothing else of the doc.
func refuseDoc(ctx context.Context, w http.ResponseWriter, user userModels.UserInfo, docUUID string) {
	who := accessBusiness.SubjectOf(&user)
	helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
		"msg": "You don't have access to this doc.", "code": "no_access",
		"can_request": canRequest(ctx, user, who, docUUID, accessBusiness.Decision{Level: accessBusiness.LevelNone}),
	})
}

// accessPerson is someone the doc is shared with by name.
type accessPerson struct {
	*business.Person
	Level accessBusiness.Level `json:"level"`
	Until *time.Time           `json:"until,omitempty"`
}

// accessTeam is a team the doc is shared with.
type accessTeam struct {
	UUID    string               `json:"team_uuid"`
	Name    string               `json:"team_name"`
	Level   accessBusiness.Level `json:"level"`
	Members int                  `json:"members"`
	Until   *time.Time           `json:"until,omitempty"`
}

// accessSpace is a project's or a team's space as the sharing panel and the
// move preview show it: which one, its name, and how many people are in it.
type accessSpace struct {
	Kind    string `json:"kind"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Members int    `json:"members"`
}

// spaceView is sp as the sharing panel and the move preview show it; nil for
// no space.
func spaceView(sp business.Space) *accessSpace {
	if sp.UUID == "" {
		return nil
	}
	return &accessSpace{Kind: sp.Kind, UUID: sp.UUID, Name: sp.Name, Members: len(sp.Members)}
}

// sharingView is the sharing panel: who has the doc and how, besides its owner.
type sharingView struct {
	Owner  *business.Person `json:"owner,omitempty"`
	People []accessPerson   `json:"people"`
	Teams  []accessTeam     `json:"teams"`
	// General is the general access: the audience (the workspace, or with
	// private the doc's space) and its level.
	General struct {
		Private bool                 `json:"private"`
		Level   accessBusiness.Level `json:"level"`
	} `json:"general"`
	// Everyone is what someone in the workspace with no grant of their own gets.
	Everyone accessBusiness.Level `json:"everyone"`
	Space    *accessSpace         `json:"space,omitempty"`
	Lock     *struct {
		Level string           `json:"level"`
		By    *business.Person `json:"by,omitempty"`
		At    *time.Time       `json:"at,omitempty"`
	} `json:"lock,omitempty"`
	// Recoveries and OpenRequests are for the owner and full-access people.
	Recoveries   []*accessModels.Recovery `json:"recoveries,omitempty"`
	OpenRequests int                      `json:"open_requests,omitempty"`
}

// viewOfSharing is a doc's sharing as the panel shows it, with when grants end.
func viewOfSharing(ctx context.Context, s *business.Sharing, full bool) (sharingView, error) {
	v := sharingView{People: []accessPerson{}, Teams: []accessTeam{}}
	v.Owner = business.PersonOf(s.Owner)
	v.General.Private, v.General.Level = s.General.Private, s.General.Level
	v.Everyone = s.EveryoneLevel()
	ends := map[string]*time.Time{}
	if id, err := uuid.Parse(s.DocUUID); err == nil {
		rows, err := accessModels.ListExpiry(ctx, accessBusiness.KindDoc, id)
		if err != nil {
			return v, err
		}
		for _, e := range rows {
			at := e.ExpiresAt
			ends[e.PrincipalKind+":"+strings.ToLower(e.PrincipalUUID.String())] = &at
		}
		if full {
			if v.Recoveries, err = accessModels.ListRecoveries(ctx, accessBusiness.KindDoc, id, 20); err != nil {
				return v, err
			}
			open, err := accessModels.ListRequests(ctx, accessModels.RequestQuery{ResourceKind: accessBusiness.KindDoc, ResourceUUID: &id, OpenOnly: true})
			if err != nil {
				return v, err
			}
			v.OpenRequests = len(open)
		}
	}
	ids := make([]string, 0, len(s.People))
	for id := range s.People {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if s.People[ids[i]] != s.People[ids[j]] {
			return s.People[ids[i]] > s.People[ids[j]]
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		p := business.PersonOf(s.Names[id])
		if p == nil {
			p = &business.Person{UUID: id}
		}
		v.People = append(v.People, accessPerson{Person: p, Level: s.People[id], Until: ends["user:"+id]})
	}
	tids := make([]string, 0, len(s.Teams))
	for id := range s.Teams {
		tids = append(tids, id)
	}
	sort.Strings(tids)
	for _, id := range tids {
		t := s.Teams[id]
		v.Teams = append(v.Teams, accessTeam{UUID: t.UUID, Name: t.Name, Level: t.Level, Members: len(t.Members), Until: ends["team:"+id]})
	}
	v.Space = spaceView(s.Space)
	if s.Lock != "" {
		v.Lock = &struct {
			Level string           `json:"level"`
			By    *business.Person `json:"by,omitempty"`
			At    *time.Time       `json:"at,omitempty"`
		}{s.Lock, business.PersonOf(s.LockedBy), s.LockedAt}
	}
	return v, nil
}

// GetDocAccess is the person's level on a doc and why (GET
// /doc/access/{doc_uuid}), and for someone who may edit it, its sharing.
func GetDocAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	id := chi.URLParam(r, "doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		badRequest(w, "That isn't a doc.")
		return
	}
	d, ok := decideFor(ctx, w, who, id, "GetDocAccess")
	if !ok {
		return
	}
	full := d.Level == accessBusiness.LevelFull
	out := helpers.Envolope{
		"doc_uuid": id, "level": d.Level, "owner": d.Owner, "lock": d.Lock, "locked": d.Locked, "reasons": d.Reasons,
		"can_share":     full,
		"can_lock_soft": d.Level >= accessBusiness.LevelEdit,
		"can_lock_hard": full,
		// Whether Request access would reach someone who can answer it.
		"can_request": canRequest(ctx, user, who, id, d),
	}
	// Who else has it is for the people who may edit it, a hard lock or not.
	if d.Unlocked >= accessBusiness.LevelEdit {
		s, err := business.ReadSharing(ctx, id)
		if err != nil {
			accessFailed(ctx, w, "GetDocAccess", err)
			return
		}
		v, err := viewOfSharing(ctx, s, full)
		if err != nil {
			accessFailed(ctx, w, "GetDocAccess", err)
			return
		}
		out["sharing"] = v
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// grantIn is one grant as a change asks for it.
type grantIn struct {
	Kind  string     `json:"kind"`
	UUID  string     `json:"uuid"`
	Level string     `json:"level"`
	Until *time.Time `json:"until,omitempty"`
}

// accessChangeIn is a change to a doc's sharing as it is asked for.
type accessChangeIn struct {
	DocUUID string    `json:"doc_uuid"`
	Adds    []grantIn `json:"adds"`
	Removes []grantIn `json:"removes"`
	General *struct {
		Private *bool  `json:"private"`
		Level   string `json:"level"`
	} `json:"general"`
	ApplyToSubpages bool `json:"apply_to_subpages"`
	// GuestLinks is what becomes of the doc's links for people outside the
	// workspace when the change makes it private: "keep", or "revoke" (the
	// default, D-8).
	GuestLinks string `json:"guest_links"`
}

// changeOf reads a change as asked for, answering a bad one (ok false).
func changeOf(w http.ResponseWriter, in accessChangeIn, at time.Time) (accessBusiness.Change, bool) {
	c := accessBusiness.Change{Resource: docRes(in.DocUUID), ApplyToSubpages: in.ApplyToSubpages}
	if !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc.")
		return c, false
	}
	if len(in.Adds) > maxGrantsPerChange || len(in.Removes) > maxGrantsPerChange {
		badRequest(w, "That's too many people at once. Share with a team instead.")
		return c, false
	}
	kindOf := func(k string) (accessBusiness.PrincipalKind, bool) {
		switch k {
		case "", "user":
			return accessBusiness.PrincipalUser, true
		case "team":
			return accessBusiness.PrincipalTeam, true
		}
		return "", false
	}
	for _, g := range in.Adds {
		kind, ok := kindOf(g.Kind)
		lvl, lok := level.Named(g.Level)
		switch {
		case !ok || !helpers.IsCanonicalUUID(strings.ToLower(g.UUID)):
			badRequest(w, "Share with a person or a team.")
			return c, false
		case !lok || lvl == accessBusiness.LevelNone || (kind == accessBusiness.PrincipalTeam && lvl > accessBusiness.LevelEdit):
			badRequest(w, "A person can view, comment, edit or have full access; a team can view, comment or edit.")
			return c, false
		case g.Until != nil && !g.Until.After(at):
			badRequest(w, "That date has passed.")
			return c, false
		}
		c.Add = append(c.Add, accessBusiness.Grant{Kind: kind, UUID: strings.ToLower(g.UUID), Level: lvl, Until: g.Until})
	}
	for _, g := range in.Removes {
		kind, ok := kindOf(g.Kind)
		if !ok || !helpers.IsCanonicalUUID(strings.ToLower(g.UUID)) {
			badRequest(w, "Take access from a person or a team.")
			return c, false
		}
		c.Remove = append(c.Remove, accessBusiness.Grant{Kind: kind, UUID: strings.ToLower(g.UUID)})
	}
	if in.General != nil {
		if in.General.Level != "" {
			lvl, ok := level.Named(in.General.Level)
			if !ok || lvl == accessBusiness.LevelFull {
				badRequest(w, "Everyone can view, comment or edit, or nobody can.")
				return c, false
			}
			c.General = &lvl
		}
		c.Private = in.General.Private
	}
	return c, true
}

// makesPrivate is whether a change asks for the doc to be private.
func makesPrivate(c accessBusiness.Change) bool {
	return (c.Private != nil && *c.Private) || (c.General != nil && *c.General == accessBusiness.LevelNone && c.Private == nil)
}

// explainedView is what a change would do, as the share dialog says it.
func explainedView(ex accessBusiness.Explanation) helpers.Envolope {
	lose, gain, change := 0, 0, 0
	for _, e := range ex.Effects {
		switch {
		case e.To < accessBusiness.LevelView && e.From >= accessBusiness.LevelView:
			lose++
		case e.To >= accessBusiness.LevelView && e.From < accessBusiness.LevelView:
			gain++
		default:
			change++
		}
	}
	summary := accessSummary(lose, gain)
	if e := ex.Everyone; e != nil {
		switch {
		case e.To < accessBusiness.LevelView:
			summary = "Everyone in the workspace who isn't shared with it loses access."
		case e.From < accessBusiness.LevelView:
			summary = "Everyone in the workspace can open it."
		default:
			summary = "Everyone in the workspace can " + e.To.String() + " it."
		}
	}
	return helpers.Envolope{"effects": ex.Effects, "everyone": ex.Everyone, "pages": ex.Pages, "lose": lose, "gain": gain, "change": change, "summary": summary}
}

// PreviewDocAccess says what a change to a doc's sharing would do, without
// making it (POST /doc/access/preview): for someone who may make it.
func PreviewDocAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in accessChangeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't a change to who has access.")
		return
	}
	c, ok := changeOf(w, in, time.Now())
	if !ok {
		return
	}
	d, ok := decideFor(ctx, w, who, in.DocUUID, "PreviewDocAccess")
	if !ok {
		return
	}
	if d.Level < accessBusiness.LevelFull {
		accessFailed(ctx, w, "PreviewDocAccess", accessBusiness.ErrForbidden)
		return
	}
	ex, err := accessBusiness.Rules.ExplainChange(ctx, who, c)
	if err != nil {
		accessFailed(ctx, w, "PreviewDocAccess", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": explainedView(ex)})
}

// UpdateDocAccess changes a doc's sharing (POST /doc/access/update): people
// and teams added at a level (until a date, or for good) or taken off, its
// general access, and with apply_to_subpages every page under it the person
// has full access to, in one transaction. For its owner and full-access
// people. Making it private revokes its links for people outside the
// workspace unless guest_links is "keep" (D-8).
func UpdateDocAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in accessChangeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't a change to who has access.")
		return
	}
	switch in.GuestLinks {
	case "", "keep", "revoke":
	default:
		badRequest(w, "Guest links are kept or revoked.")
		return
	}
	c, ok := changeOf(w, in, time.Now())
	if !ok {
		return
	}
	if _, ok := decideFor(ctx, w, who, in.DocUUID, "UpdateDocAccess"); !ok {
		return
	}
	if refuseDemo(ctx, w, user, in.DocUUID) {
		return
	}
	ex, err := accessBusiness.Apply(ctx, who, c)
	if err != nil {
		accessFailed(ctx, w, "UpdateDocAccess", err)
		return
	}
	revoked := 0
	if makesPrivate(c) && in.GuestLinks != "keep" {
		revoked = revokeGuestLinks(ctx, in.DocUUID)
	}
	out := explainedView(ex)
	out["guest_links_revoked"] = revoked
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Access changed.", "data": out})
}

// revokeGuestLinks turns off a doc's links for people outside the workspace,
// and asks its live sockets again so a guest's closes. Answers how many it
// turned off.
func revokeGuestLinks(ctx context.Context, docUUID string) int {
	links, err := guestModel.ListActiveForResource(ctx, "doc", docUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/revokeGuestLinks %s: %+v", docUUID, err)
		return 0
	}
	n := 0
	for _, l := range links {
		if err := guestModel.Revoke(ctx, l.Id); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/revokeGuestLinks %s link %s: %+v", docUUID, l.Id, err)
			continue
		}
		n++
	}
	if n > 0 {
		business.ReauthLater([]string{docUUID}, nil)
	}
	return n
}

// activeMember says whether a person is someone in the workspace a doc can be
// handed to: an active member, not a guest or a bot.
func activeMember(ctx context.Context, userUUID string) (bool, error) {
	id, err := uuid.Parse(userUUID)
	if err != nil {
		return false, nil
	}
	u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return u != nil && u.Id == id && u.IsMember(), nil
}

// TransferDoc hands a doc to someone else in the workspace (POST
// /doc/access/transfer {doc_uuid, to_user_uuid}): for its owner, who keeps
// full access.
func TransferDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID    string `json:"doc_uuid"`
		ToUserUUID string `json:"to_user_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) || !helpers.IsCanonicalUUID(strings.ToLower(in.ToUserUUID)) {
		badRequest(w, "Say which doc, and who to hand it to.")
		return
	}
	if _, ok := decideFor(ctx, w, who, in.DocUUID, "TransferDoc"); !ok {
		return
	}
	if refuseDemo(ctx, w, user, in.DocUUID) {
		return
	}
	member, err := activeMember(ctx, in.ToUserUUID)
	if err != nil {
		accessFailed(ctx, w, "TransferDoc", err)
		return
	}
	if !member {
		badRequest(w, "A doc can be handed only to someone in this workspace.")
		return
	}
	if err := accessBusiness.Rules.Transfer(ctx, who, docRes(in.DocUUID), strings.ToLower(in.ToUserUUID)); err != nil {
		accessFailed(ctx, w, "TransferDoc", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Handed over.", "data": helpers.Envolope{"doc_uuid": in.DocUUID, "owner": strings.ToLower(in.ToUserUUID)}})
}

// RecoverDoc has a workspace admin take over a doc, or hand it to someone
// (POST /admin/docs/recover {doc_uuid, to_user_uuid?, reason}), when its owner
// has left or can't be reached. Loud: recorded in the admin audit log and on
// the doc, where its owner and full-access people see it, and told to the
// owner before.
func RecoverDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID    string `json:"doc_uuid"`
		ToUserUUID string `json:"to_user_uuid"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) || (in.ToUserUUID != "" && !helpers.IsCanonicalUUID(strings.ToLower(in.ToUserUUID))) {
		badRequest(w, "Say which doc to recover, and who to hand it to.")
		return
	}
	if !who.ActingAdmin() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a workspace admin can recover a doc."})
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		badRequest(w, "Say why the doc is being recovered: its owner will see it.")
		return
	}
	to := strings.ToLower(in.ToUserUUID)
	if to != "" {
		member, err := activeMember(ctx, to)
		if err != nil {
			accessFailed(ctx, w, "RecoverDoc", err)
			return
		}
		if !member {
			badRequest(w, "A doc can be handed only to someone in this workspace.")
			return
		}
	}
	rec, err := accessBusiness.Recover(ctx, who, docRes(in.DocUUID), to, in.Reason)
	if err != nil {
		accessFailed(ctx, w, "RecoverDoc", err)
		return
	}
	from := ""
	if rec.FromOwnerUUID != nil {
		from = rec.FromOwnerUUID.String()
	}
	adminAuditBusiness.Record(r, "doc.recover", adminAuditBusiness.CategorySecurity,
		"Recovered a doc from its owner", map[string]interface{}{
			"doc_uuid": in.DocUUID, "from_owner_uuid": from, "to_owner_uuid": rec.ToOwnerUUID.String(), "reason": rec.Reason,
		})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Recovered.", "data": rec})
}

// LockDoc sets or lifts a doc's lock (POST /doc/lock {doc_uuid, lock:
// soft|hard|none}). A soft lock is anyone's who may edit; a hard lock, set or
// lifted, is the owner's or full access's (D-12).
func LockDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID string `json:"doc_uuid"`
		Lock    string `json:"lock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc to lock.")
		return
	}
	lock := in.Lock
	switch lock {
	case "none":
		lock = level.LockNone
	case level.LockSoft, level.LockHard:
	default:
		badRequest(w, "A lock is soft, hard or none.")
		return
	}
	if _, ok := decideFor(ctx, w, who, in.DocUUID, "LockDoc"); !ok {
		return
	}
	if refuseDemo(ctx, w, user, in.DocUUID) {
		return
	}
	d, err := accessBusiness.Lock(ctx, who, docRes(in.DocUUID), lock)
	if err != nil {
		if errors.Is(err, accessBusiness.ErrForbidden) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": "You can't change this doc's lock."})
			return
		}
		accessFailed(ctx, w, "LockDoc", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Lock changed.", "data": helpers.Envolope{"doc_uuid": in.DocUUID, "lock": d.Lock, "locked": d.Locked, "level": d.Level}})
}

// RequestDocAccess asks a doc's owner for access (POST /doc/access/request
// {doc_uuid, level: view|comment|edit, note}). It names nothing of the doc.
func RequestDocAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID string `json:"doc_uuid"`
		Level   string `json:"level"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc.")
		return
	}
	want, ok := level.Named(in.Level)
	if in.Level == "" {
		want, ok = accessBusiness.LevelView, true
	}
	if !ok || want < accessBusiness.LevelView || want > accessBusiness.LevelEdit {
		badRequest(w, "Ask to view, comment or edit.")
		return
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(in.DocUUID))
	if err != nil {
		accessFailed(ctx, w, "RequestDocAccess", err)
		return
	}
	if d.Deleted {
		docGone(w, d.Was >= accessBusiness.LevelView)
		return
	}
	if helpers.IsDemoVisitor(user.UserPostgresInfo.EmailID) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": demoRequestMsg})
		return
	}
	req, err := accessBusiness.Ask(ctx, who, docRes(in.DocUUID), want, in.Note)
	if err != nil {
		accessFailed(ctx, w, "RequestDocAccess", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Asked.", "data": helpers.Envolope{
		"id": req.ID, "doc_uuid": in.DocUUID, "level_wanted": req.LevelWanted, "status": req.Status, "created_at": req.CreatedAt,
	}})
}

// AnswerDocAccess approves or declines a request for access (POST
// /doc/access/answer {request_id, approve, level?, until?}): for the doc's
// owner and full-access people. It answers with the request as the requests
// list shows it to them (requestView), so the web app reads one shape.
func AnswerDocAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in struct {
		RequestID string     `json:"request_id"`
		Approve   bool       `json:"approve"`
		Level     string     `json:"level"`
		Until     *time.Time `json:"until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(strings.ToLower(in.RequestID)) {
		badRequest(w, "Say which request.")
		return
	}
	var grant *accessBusiness.Level
	if in.Level != "" {
		l, ok := level.Named(in.Level)
		if !ok || l == accessBusiness.LevelNone {
			badRequest(w, "Give view, comment, edit or full access.")
			return
		}
		grant = &l
	}
	if in.Until != nil && !in.Until.After(time.Now()) {
		badRequest(w, "That date has passed.")
		return
	}
	req, err := accessBusiness.Answer(ctx, who, uuid.MustParse(in.RequestID), in.Approve, grant, in.Until)
	if err != nil {
		accessFailed(ctx, w, "AnswerDocAccess", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Answered.", "data": requestView{
		ID: req.ID.String(), DocUUID: req.ResourceUUID.String(), LevelWanted: req.LevelWanted, Note: req.Note,
		Status: req.Status, LevelGranted: req.LevelGranted, CreatedAt: req.CreatedAt, DecidedAt: req.DecidedAt,
	}})
}

// requestView is a request as a list shows it: the doc is named only to the
// people who answer it, and the person asking only to them.
type requestView struct {
	ID           string           `json:"id"`
	DocUUID      string           `json:"doc_uuid"`
	DocTitle     string           `json:"doc_title,omitempty"`
	Requester    *business.Person `json:"requester,omitempty"`
	LevelWanted  string           `json:"level_wanted"`
	Note         string           `json:"note,omitempty"`
	Status       string           `json:"status"`
	LevelGranted *string          `json:"level_granted,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	DecidedAt    *time.Time       `json:"decided_at,omitempty"`
}

// ListDocAccessRequests is the requests the person answers (open ones, on the
// doc named or on every doc they have full access to) and their own (GET
// /doc/access/requests?doc_uuid=).
func ListDocAccessRequests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var res *accessBusiness.Resource
	if id := r.URL.Query().Get("doc_uuid"); id != "" {
		if !helpers.IsCanonicalUUID(id) {
			badRequest(w, "That isn't a doc.")
			return
		}
		one := docRes(id)
		res = &one
	}
	incoming, outgoing, err := accessBusiness.Requests(ctx, who, res)
	if err != nil {
		accessFailed(ctx, w, "ListDocAccessRequests", err)
		return
	}
	var docs, people []string
	for _, q := range incoming {
		docs = append(docs, q.ResourceUUID.String())
		people = append(people, q.RequesterUUID.String())
	}
	titles, err := domain.DocTitles(ctx, docs)
	if err != nil {
		accessFailed(ctx, w, "ListDocAccessRequests", err)
		return
	}
	names, err := domain.PeopleByUUID(ctx, people)
	if err != nil {
		accessFailed(ctx, w, "ListDocAccessRequests", err)
		return
	}
	view := func(q *accessModels.Request, full bool) requestView {
		v := requestView{
			ID: q.ID.String(), DocUUID: q.ResourceUUID.String(), LevelWanted: q.LevelWanted, Status: q.Status,
			LevelGranted: q.LevelGranted, CreatedAt: q.CreatedAt, DecidedAt: q.DecidedAt,
		}
		if full {
			v.DocTitle = titles[strings.ToLower(v.DocUUID)]
			v.Requester = business.PersonOf(names[strings.ToLower(q.RequesterUUID.String())])
			v.Note = q.Note
		}
		return v
	}
	in, out := []requestView{}, []requestView{}
	for _, q := range incoming {
		in = append(in, view(q, true))
	}
	for _, q := range outgoing {
		v := view(q, false)
		v.Note = q.Note
		out = append(out, v)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"incoming": in, "outgoing": out}})
}

// legacyUser is a person as the current share dialog reads one.
type legacyUser struct {
	Uid        string  `json:"uid,omitempty"`
	UUID       string  `json:"user_uuid"`
	Name       string  `json:"user_name,omitempty"`
	FullName   string  `json:"user_full_name,omitempty"`
	ProfileKey *string `json:"user_profile_object_key,omitempty"`
}

func legacyOf(u *dgraphStruct.DgraphUser, id string) legacyUser {
	if u == nil {
		return legacyUser{UUID: id}
	}
	return legacyUser{Uid: u.Uid, UUID: u.Uuid, Name: u.UserName, FullName: u.UserFullName, ProfileKey: u.ProfileKey}
}

// GetDocSharing is a doc's sharing in the shape the current share dialog
// reads (GET /doc/getDocPermissions?doc_uuid=; it takes over the route at
// wiring): for the people who may edit it. doc_created_by is its owner, whom
// the dialog shows as the owner; full-access people are listed with the
// editors, as the dialog has no list of its own for them, and again in
// doc_full_access_users.
func GetDocSharing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	id := r.URL.Query().Get("doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "doc_uuid is required"})
		return
	}
	d, ok := decideFor(ctx, w, who, id, "GetDocSharing")
	if !ok {
		return
	}
	if d.Unlocked < accessBusiness.LevelEdit {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	s, err := business.ReadSharing(ctx, id)
	if err != nil {
		accessFailed(ctx, w, "GetDocSharing", err)
		return
	}
	lists := map[accessBusiness.Level][]legacyUser{}
	ids := make([]string, 0, len(s.People))
	for pid := range s.People {
		ids = append(ids, pid)
	}
	sort.Strings(ids)
	for _, pid := range ids {
		lists[s.People[pid]] = append(lists[s.People[pid]], legacyOf(s.Names[pid], pid))
	}
	editors := append(append([]legacyUser{}, lists[accessBusiness.LevelFull]...), lists[accessBusiness.LevelEdit]...)
	var owner *legacyUser
	if s.Owner != nil {
		o := legacyOf(s.Owner, s.Owner.Uuid)
		owner = &o
	}
	empty := func(l []legacyUser) []legacyUser {
		if l == nil {
			return []legacyUser{}
		}
		return l
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": helpers.Envolope{
		"doc_uuid":              id,
		"doc_private":           s.General.Private,
		"doc_public_comment":    s.General.Level >= accessBusiness.LevelComment,
		"doc_public_edit":       s.General.Level >= accessBusiness.LevelEdit,
		"doc_created_by":        owner,
		"doc_owner":             owner,
		"doc_editing_users":     empty(editors),
		"doc_commenting_users":  empty(lists[accessBusiness.LevelComment]),
		"doc_reading_users":     empty(lists[accessBusiness.LevelView]),
		"doc_full_access_users": empty(lists[accessBusiness.LevelFull]),
		"level":                 d.Level,
	}})
}

// UpdateDocSharing changes a doc's sharing as the current share dialog asks
// (POST /doc/updateDocPermissions {doc_uuid, add_editors, remove_editors,
// add_viewers, ...}; it takes over the route at wiring): for its owner and
// full-access people, through the same change as /doc/access/update.
func UpdateDocSharing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in adapter.InputUpdateDocPermissions
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocId) {
		badRequest(w, "Say which doc.")
		return
	}
	var c accessChangeIn
	c.DocUUID = in.DocId
	for _, l := range []struct {
		ids []string
		lvl string
	}{{in.AddViewers, "view"}, {in.AddCommenters, "comment"}, {in.AddEditors, "edit"}} {
		for _, id := range l.ids {
			c.Adds = append(c.Adds, grantIn{UUID: id, Level: l.lvl})
		}
	}
	added := map[string]bool{}
	for _, g := range c.Adds {
		added[strings.ToLower(g.UUID)] = true
	}
	// Taking someone off one list to put them on another is the add's work.
	for _, ids := range [][]string{in.RemoveViewers, in.RemoveCommenters, in.RemoveEditors} {
		for _, id := range ids {
			if !added[strings.ToLower(id)] {
				c.Removes = append(c.Removes, grantIn{UUID: id})
			}
		}
	}
	change, ok := changeOf(w, c, time.Now())
	if !ok {
		return
	}
	if _, ok := decideFor(ctx, w, who, in.DocId, "UpdateDocSharing"); !ok {
		return
	}
	if refuseDemo(ctx, w, user, in.DocId) {
		return
	}
	if _, err := accessBusiness.Apply(ctx, who, change); err != nil {
		if errors.Is(err, accessBusiness.ErrForbidden) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
			return
		}
		accessFailed(ctx, w, "UpdateDocSharing", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated doc permissions successfully!"})
}

// docCommentRemoved is a comment a moderator removed (A6 tells its author).
const docCommentRemoved resourceEventBusiness.Kind = "doc.comment.removed"

// RemoveDocComment removes a comment on a doc (POST /doc/removeComment; it
// takes over the route at wiring): its author's, while they can read the doc,
// or anyone's for the doc's owner, its full-access people and workspace
// admins, who can take an abusive comment down.
func RemoveDocComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in adapter.CreateOrUpdateDocCommentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "Say which comment to remove.")
		return
	}
	commentUUID, err := uuid.Parse(in.UUID)
	if err != nil {
		badRequest(w, "Say which comment to remove.")
		return
	}
	c, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, commentUUID.String(), who.UserUID)
	if err != nil {
		// The read says the same for a comment that isn't there and a graph
		// that didn't answer, so this answers as the route always has.
		helpers.LogErrorWithContext(ctx, "controllers/RemoveDocComment %s: %+v", commentUUID, err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't remove the comment. Try again."})
		return
	}
	if c == nil || c.Doc == nil || c.Doc.Uuid == "" || c.CommentBy == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such comment."})
		return
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(c.Doc.Uuid))
	if err != nil {
		accessFailed(ctx, w, "RemoveDocComment", err)
		return
	}
	if d.Deleted {
		docGone(w, d.Was >= accessBusiness.LevelView)
		return
	}
	if d.Level == accessBusiness.LevelNone && !who.ActingAdmin() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such comment."})
		return
	}
	mod := commentBusiness.Moderator{
		Owner: d.Owner, FullAccess: d.Level == accessBusiness.LevelFull,
		Admin: who.ActingAdmin(), Deleted: d.Deleted,
	}
	author := strings.EqualFold(c.CommentBy.Uuid, who.UserUUID)
	if !commentBusiness.CanRemoveComment(author, d.Level >= accessBusiness.LevelView, mod) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't remove that comment."})
		return
	}
	// The demo's own comments stay for every visitor after this one.
	if !author && demoGuard.KeepsFromVisitor(user.UserPostgresInfo.EmailID, c.CreatedAt) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": demoAccessMsg})
		return
	}
	if err := business.DeleteDocComment(ctx, commentUUID, c.Doc.Uuid, c); err != nil {
		accessFailed(ctx, w, "RemoveDocComment", err)
		return
	}
	if !author {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: docCommentRemoved, ResourceKind: accessBusiness.KindDoc, ResourceUUID: c.Doc.Uuid,
			Actor: resourceEventBusiness.Actor{UserUUID: who.UserUUID}, At: time.Now(),
			Subjects: []string{c.CommentBy.Uuid}, Data: map[string]string{"comment": commentUUID.String(), "by": "moderator"},
		})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted doc commment successfully!"})
}

// AuthorizeDocSocket lets a person's live socket into a doc (POST
// /docColab/authorize: the route a collaboration service from before the Doc
// Session asks; DocCollabAuthorize delegates here until the integrator swaps
// it in). Such a service opens every socket it is let in for writing, so only
// someone the Access Resolver lets edit is let in, and a hard lock that holds
// an editor at comment keeps them out; it answers {level, canEdit, locked,
// lock}. The Doc Session (/docColab/session, DocCollabSession) is what lets
// readers in, read-only. Nobody joins a deleted doc live.
func AuthorizeDocSocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in struct {
		DocUUID string `json:"doc_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc.")
		return
	}
	who := accessBusiness.SubjectOf(&userInfo)
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(in.DocUUID))
	if errors.Is(err, accessBusiness.ErrNotFound) {
		helpers.WriteDocNotFound(w)
		return
	}
	if err != nil {
		// The graph didn't answer: the service retries a connection later.
		helpers.LogErrorWithContext(ctx, "controllers/AuthorizeDocSocket %s: %+v", in.DocUUID, err)
		helpers.WriteRetryLater(w, "Couldn't open the doc just now.")
		return
	}
	if d.Deleted {
		docGone(w, d.Was >= accessBusiness.LevelView)
		return
	}
	if d.Level < accessBusiness.LevelEdit {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Unauthorized doc access"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Authorised", "level": d.Level, "canEdit": true, "locked": d.Locked, "lock": d.Lock,
	})
}
