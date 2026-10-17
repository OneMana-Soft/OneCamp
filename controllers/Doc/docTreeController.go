package docController

// The doc tree over HTTP (router/docsTree.go): a page's children and its
// path, moving a page, making one somewhere in the tree, putting a page and
// what's under it in the trash, and duplicating it. Who may see or change a
// page is the Access Resolver's to say (accessBusiness.Rules); a title it
// says no to is never sent (I12).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/go-chi/chi/v5"
)

func docRes(id string) accessBusiness.Resource {
	return accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: id}
}

func badRequest(w http.ResponseWriter, msg string) {
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": msg})
}

// treeFailed answers an error from the tree, the trash or a list. A decision
// that couldn't be made (accessBusiness.ErrUndecided) is a read that failed,
// answered as one the app may try again (helpers.WriteDocReadFailure, 503):
// never a no, and never "something went wrong".
func treeFailed(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, accessBusiness.ErrUndecided):
		helpers.WriteDocReadFailure(w, r, err, "controllers/"+where)
	case errors.Is(err, business.ErrDocDeleted):
		docGone(w, true)
	case errors.Is(err, treeBusiness.ErrNotFound), errors.Is(err, business.ErrNotFound), errors.Is(err, accessBusiness.ErrUnknownKind):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, treeBusiness.ErrForbidden), errors.Is(err, business.ErrTrashForbidden), errors.Is(err, accessBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't change that page."})
	case errors.Is(err, accessBusiness.ErrLocked):
		helpers.WriteJSON(w, http.StatusLocked, helpers.Envolope{"code": "locked", "msg": "This page is locked. Only people with full access can change it."})
	case errors.Is(err, accessBusiness.ErrSpaceNeedsFull):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "needs_full_access",
			"msg": "Moving a page to another space changes who can open it. You need full access to it and to every page under it."})
	case errors.Is(err, accessBusiness.ErrNotInSpace):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_in_space", "msg": "Pages can only go into a project or team you're in."})
	case errors.Is(err, accessBusiness.ErrNoSuchSpace):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_space", "msg": "There's no such project or team."})
	case errors.Is(err, business.ErrDemoKept):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoSeededMsg})
	case errors.Is(err, treeBusiness.ErrCycle):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "cycle", "msg": "A page can't go inside itself or one of its own subpages."})
	case errors.Is(err, treeBusiness.ErrTooDeep):
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"code": "too_deep", "msg": "Pages can only go 32 levels deep."})
	case errors.Is(err, business.ErrTooMany):
		tooManyPages(w)
	case errors.Is(err, treeBusiness.ErrNotSibling):
		badRequest(w, "The page to go after isn't there.")
	case errors.Is(err, business.ErrNotInTrash):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_in_trash", "msg": "That doc isn't in the trash."})
	case errors.Is(err, business.ErrNoTemplate):
		badRequest(w, "There's no such template.")
	case errors.Is(err, treeBusiness.ErrBusy):
		helpers.WriteRetryLater(w, "Someone else is changing these pages right now.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Something went wrong. Try again."})
	}
}

// tooManyPages answers a change to a page and everything under it when there
// is more under it than one change takes (business/Tree's ErrTooMany): it is
// refused whole, never made part of the way.
func tooManyPages(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"code": "too_many", "msg": "There are too many pages under this one to do that at once."})
}

// pageLimit reads a page size: 1 to 100, 30 when not given.
func pageLimit(r *http.Request) int { return helpers.LimitParam(r.URL.Query(), 30, 100) }

// TreeNode is a page as the tree sends it: the tree's fields, and the doc's
// own that a row shows.
type TreeNode struct {
	treeBusiness.Node
	Private   *bool  `json:"private,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func treeNodes(nodes []treeBusiness.Node) []TreeNode {
	out := make([]TreeNode, 0, len(nodes))
	for _, n := range nodes {
		t := TreeNode{Node: n}
		if d, ok := n.Data.(*dgraphStruct.DgraphDoc); ok && d != nil {
			t.Private = d.IsPrivate
			if d.UpdatedAt != nil {
				t.UpdatedAt = d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
		}
		out = append(out, t)
	}
	return out
}

// GetDocChildren is one level of the doc tree: a page's children, or with no
// parent the top of a space ("workspace", "private", "shared",
// "project:<uuid>", "team:<uuid>"). The pages the person can't open are
// counted in hidden, never named.
func GetDocChildren(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	q := r.URL.Query()
	parent := q.Get("parent")
	if parent != "" && !helpers.IsCanonicalUUID(parent) {
		badRequest(w, "That isn't a doc.")
		return
	}
	lister, ok := treeBusiness.Hierarchy.(treeBusiness.Lister)
	var page treeBusiness.Page
	var err error
	if ok {
		page, err = lister.Level(ctx, who, accessBusiness.KindDoc, treeBusiness.LevelQuery{Parent: parent, Space: q.Get("space"), Cursor: q.Get("cursor"), Limit: pageLimit(r)})
	} else {
		page, err = treeBusiness.Hierarchy.Children(ctx, who, accessBusiness.KindDoc, parent, q.Get("cursor"))
	}
	if err != nil {
		treeFailed(w, r, "GetDocChildren", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"parent": parent, "space": q.Get("space"), "nodes": treeNodes(page.Nodes),
		"hidden": page.Hidden, "count": page.Count, "next_cursor": page.Cursor,
	}})
}

// GetDocAncestors is the path down to a page, the top first: a page above
// it that the person can't open comes as {redacted: true}, with no title and
// no id.
func GetDocAncestors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	id := chi.URLParam(r, "doc_uuid")
	if !helpers.IsCanonicalUUID(id) {
		badRequest(w, "That isn't a doc.")
		return
	}
	var nodes []treeBusiness.Node
	truncated := false
	var err error
	if lister, ok := treeBusiness.Hierarchy.(treeBusiness.Lister); ok {
		nodes, truncated, err = lister.Path(ctx, who, accessBusiness.KindDoc, id)
	} else {
		nodes, err = treeBusiness.Hierarchy.Ancestors(ctx, who, accessBusiness.KindDoc, id)
	}
	if err != nil {
		treeFailed(w, r, "GetDocAncestors", err)
		return
	}
	if nodes == nil {
		nodes = []treeBusiness.Node{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"ancestors": treeNodes(nodes), "truncated": truncated}})
}

// MoveRequest is where a page goes: under parent_uuid (none: the top of a
// space), right after after_uuid or first or last, and what becomes of who can
// open it.
type MoveRequest struct {
	DocUUID    string `json:"doc_uuid"`
	ParentUUID string `json:"parent_uuid,omitempty"`
	AfterUUID  string `json:"after_uuid,omitempty"`
	Place      string `json:"place,omitempty"`
	Space      string `json:"space,omitempty"`
	Access     string `json:"access,omitempty"`
}

func readMove(w http.ResponseWriter, r *http.Request) (MoveRequest, bool) {
	var m MoveRequest
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		badRequest(w, "That isn't a move.")
		return m, false
	}
	for _, id := range []string{m.DocUUID, m.ParentUUID, m.AfterUUID} {
		if id != "" && !helpers.IsCanonicalUUID(id) {
			badRequest(w, "That isn't a doc.")
			return m, false
		}
	}
	if m.DocUUID == "" {
		badRequest(w, "Say which page moves.")
		return m, false
	}
	switch m.Access {
	case "", string(treeBusiness.KeepAccess), string(treeBusiness.AdoptAccess):
	default:
		badRequest(w, "Access is keep or adopt.")
		return m, false
	}
	return m, true
}

// spaceFor is the space a moved page goes into: its new parent's (none, if
// the parent has none and the page had one), or at the top the one named. A
// new parent is decided first, as the move decides it, so a page someone
// may not add to is refused before its space is read: the answer never says
// what space a page they can't open is in.
func spaceFor(ctx context.Context, who accessBusiness.Subject, m MoveRequest) (kind, id string, err error) {
	if m.ParentUUID != "" {
		if _, err := accessBusiness.Open(ctx, who, docRes(m.ParentUUID), accessBusiness.LevelEdit); err != nil {
			return "", "", err
		}
	}
	rows, err := business.DocRows(ctx, []string{m.DocUUID, m.ParentUUID}, who.UserUID)
	if err != nil {
		return "", "", err
	}
	var doc, parent *dgraphStruct.DgraphDoc
	for _, d := range rows {
		switch d.Uuid {
		case m.DocUUID:
			doc = d
		case m.ParentUUID:
			parent = d
		}
	}
	if doc == nil {
		return "", "", nil
	}
	target := struct{ kind, id string }{}
	switch {
	case m.ParentUUID != "" && parent != nil:
		target.kind, target.id = parent.SpaceKind, parent.SpaceUUID
	case m.ParentUUID == "" && m.Space != "":
		if _, k, s := business.TreeSection(m.Space); k != "" {
			target.kind, target.id = k, s
		}
	default:
		return "", "", nil
	}
	if target.kind == doc.SpaceKind && target.id == doc.SpaceUUID {
		return "", "", nil
	}
	if target.kind == "" {
		return treeBusiness.SpaceNone, "", nil
	}
	return target.kind, target.id, nil
}

// MoveDoc moves a page (POST /doc/move): the person needs edit rights on it
// and on its new parent. 409 for a move into itself, 422 past 32 levels. With
// access "adopt", the page takes its new parent's sharing once it lands; when
// that can't be done, the move stands and the answer says access was kept.
// A move into another space, or out of one, changes who can open the page and
// the pages under it: it needs full access to each of them, and being in the
// space it goes into (or a workspace admin), and is followed as a change to
// their sharing, its owner told (clearSpaceMove).
func MoveDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	m, ok := readMove(w, r)
	if !ok {
		return
	}
	spaceKind, spaceUUID, err := spaceFor(ctx, who, m)
	if err != nil {
		treeFailed(w, r, "MoveDoc", err)
		return
	}
	sm, ok := clearSpaceMove(w, r, who, m.DocUUID, spaceKind, spaceUUID, "MoveDoc")
	if !ok || (sm != nil && refuseDemo(ctx, w, user, m.DocUUID)) {
		return
	}
	moved, err := treeBusiness.Hierarchy.Move(ctx, who, treeBusiness.Move{
		Kind: accessBusiness.KindDoc, UUID: m.DocUUID, ParentUUID: m.ParentUUID, AfterUUID: m.AfterUUID,
		Place: m.Place, SpaceKind: spaceKind, SpaceUUID: spaceUUID, Access: treeBusiness.AccessChoice(m.Access),
	})
	if err != nil {
		treeFailed(w, r, "MoveDoc", err)
		return
	}
	var space *business.SpaceChange
	if sm != nil && len(moved.Spaced) > 0 {
		space = sm.Done(ctx, moved.Spaced)
	}
	access := string(treeBusiness.KeepAccess)
	if m.Access == string(treeBusiness.AdoptAccess) && m.ParentUUID != "" && !demoKeepsDoc(ctx, user, m.DocUUID) {
		// The page and the pages under it the mover has full access to end
		// with exactly the new parent's sharing (A5); without full access to
		// the page, it keeps its own.
		change, err := accessBusiness.AdoptChange(ctx, docRes(m.DocUUID), docRes(m.ParentUUID))
		if err == nil {
			_, err = accessBusiness.Apply(ctx, who, change)
		}
		if err == nil {
			access = string(treeBusiness.AdoptAccess)
		} else {
			helpers.LogInfoWithContext(ctx, "controllers/MoveDoc: %s kept its sharing: %v", m.DocUUID, err)
		}
	}
	business.EmitMoved(ctx, m.DocUUID, m.ParentUUID, who.UserUUID, space)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"doc_uuid": m.DocUUID, "parent_uuid": m.ParentUUID, "space": m.Space, "access": access,
	}})
}

// PreviewDocMove says what a move would do to who can open the page, before
// it is made (POST /doc/move/preview): with access "adopt", the people who'd
// lose or gain access as the page and the pages under it take the new
// parent's sharing, and whether everyone in the workspace would (the Access
// Resolver's explanation). can_adopt is false for a mover without full access
// to the page, whose move keeps its sharing. A move into another space, or
// out of one, counts the space's people who'd gain or lose the page too, says
// which space it goes into (space) and which it leaves (was), null for none,
// so the question can say why, and is refused as the move would be.
func PreviewDocMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	m, ok := readMove(w, r)
	if !ok {
		return
	}
	d, err := accessBusiness.Open(ctx, who, docRes(m.DocUUID), accessBusiness.LevelEdit)
	if err != nil {
		treeFailed(w, r, "PreviewDocMove", err)
		return
	}
	canAdopt := d.Level >= accessBusiness.LevelFull
	spaceKind, spaceUUID, err := spaceFor(ctx, who, m)
	if err != nil {
		treeFailed(w, r, "PreviewDocMove", err)
		return
	}
	adopting := m.Access == string(treeBusiness.AdoptAccess) && m.ParentUUID != ""
	if spaceKind == "" && !adopting {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
			"available": true, "can_adopt": canAdopt, "lose": 0, "gain": 0, "summary": "No one's access changes.",
		}})
		return
	}
	var adopt *accessBusiness.Change
	if adopting {
		change, err := accessBusiness.AdoptChange(ctx, docRes(m.DocUUID), docRes(m.ParentUUID))
		if err != nil {
			treeFailed(w, r, "PreviewDocMove", err)
			return
		}
		adopt = &change
	}
	var exp accessBusiness.Explanation
	var sm *accessBusiness.SpaceMove
	if spaceKind == "" {
		exp, err = accessBusiness.Rules.ExplainChange(ctx, who, *adopt)
	} else {
		if sm, ok = clearSpaceMove(w, r, who, m.DocUUID, spaceKind, spaceUUID, "PreviewDocMove"); !ok {
			return
		}
		// The move as it would be made, refused as it would be, without it.
		var moved treeBusiness.Moved
		moved, err = treeBusiness.Hierarchy.Move(ctx, who, treeBusiness.Move{
			Kind: accessBusiness.KindDoc, UUID: m.DocUUID, ParentUUID: m.ParentUUID, AfterUUID: m.AfterUUID,
			Place: m.Place, SpaceKind: spaceKind, SpaceUUID: spaceUUID, DryRun: true,
		})
		if err == nil {
			exp, err = sm.Explain(ctx, adopt, len(moved.Spaced))
		}
	}
	if err != nil {
		treeFailed(w, r, "PreviewDocMove", err)
		return
	}
	out := explainedView(exp)
	out["available"], out["can_adopt"] = true, canAdopt
	if sm != nil {
		to, from := sm.Spaces()
		out["space"], out["was"] = spaceView(to), spaceView(from)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// accessSummary is the sentence the move dialog shows.
func accessSummary(lose, gain int) string {
	people := func(n int) string {
		if n == 1 {
			return "1 person"
		}
		return strconv.Itoa(n) + " people"
	}
	switch {
	case lose == 0 && gain == 0:
		return "No one's access changes."
	case gain == 0:
		return people(lose) + " lose access."
	case lose == 0:
		return people(gain) + " gain access."
	}
	return people(lose) + " lose access, and " + people(gain) + " gain it."
}

// CreateDocInTree makes a doc (POST /doc/createDoc): blank, or from a
// template, at the top of a space or under a page the person may add to,
// which it copies the sharing of. The title may be empty. Everyone in a
// project's or a team's space can open the pages at its top, so only the
// people in it put one there, and a workspace admin (EnterSpace, as a move
// into the space takes).
func CreateDocInTree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in business.NewDoc
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't a new doc.")
		return
	}
	for _, id := range []string{in.ParentUUID, in.AfterUUID} {
		if id != "" && !helpers.IsCanonicalUUID(id) {
			badRequest(w, "That isn't a doc.")
			return
		}
	}
	if _, kind, id := business.TreeSection(in.Space); in.ParentUUID == "" && kind != "" {
		if _, err := accessBusiness.EnterSpace(ctx, who, kind, id); err != nil {
			treeFailed(w, r, "CreateDocInTree", err)
			return
		}
	}
	var parent *dgraphStruct.DgraphDoc
	if in.ParentUUID != "" {
		if _, err := accessBusiness.Open(ctx, who, docRes(in.ParentUUID), accessBusiness.LevelEdit); err != nil {
			treeFailed(w, r, "CreateDocInTree", err)
			return
		}
		rows, err := business.DocRows(ctx, []string{in.ParentUUID}, who.UserUID)
		if err != nil || len(rows) == 0 {
			treeFailed(w, r, "CreateDocInTree", errors.Join(err, business.ErrNotFound))
			return
		}
		parent = rows[0]
	}
	doc, err := business.CreateDocInTree(ctx, &user.UserDgraphInfo, in, parent)
	if err != nil {
		treeFailed(w, r, "CreateDocInTree", err)
		return
	}
	if parent != nil {
		if err := accessBusiness.Rules.CopyOnCreate(ctx, who, docRes(parent.Uuid), docRes(doc.Uuid)); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/CreateDocInTree: %s didn't take its parent's sharing: %v", doc.Uuid, err)
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created doc successfully!", "data": doc})
}

// DeleteDocTree puts a page and every page under it the person can open in
// the trash as one delete (POST /doc/delete, and /doc/deleteDoc): for someone
// who may change it. A page under it they can't open stays, moving up to its
// parent (kept counts them). The answer's batch_id undoes it (POST
// /doc/restore).
func DeleteDocTree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID string `json:"doc_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc to delete.")
		return
	}
	if _, err := accessBusiness.Open(ctx, who, docRes(in.DocUUID), accessBusiness.LevelEdit); err != nil {
		treeFailed(w, r, "DeleteDocTree", err)
		return
	}
	res, err := business.TrashSubtree(ctx, in.DocUUID, who.UserUID, who.UserUUID, user.UserPostgresInfo.EmailID)
	if err != nil {
		treeFailed(w, r, "DeleteDocTree", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Moved to the trash.", "data": res})
}

// DuplicateDoc copies a page, with its subpages if asked (POST
// /doc/duplicate): only pages the person can open, the copy theirs. It goes
// right after the page, sharing what its new parent shares, only when they may
// edit the page and add to its parent; else at the top of their private pages.
// A copy of a page they may only view is theirs alone, wherever it sits
// (business/Doc DuplicateDocs).
func DuplicateDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID      string `json:"doc_uuid"`
		WithChildren bool   `json:"with_children"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc to copy.")
		return
	}
	d, err := accessBusiness.Open(ctx, who, docRes(in.DocUUID), accessBusiness.LevelView)
	if err != nil {
		treeFailed(w, r, "DuplicateDoc", err)
		return
	}
	ids := []string{in.DocUUID}
	editable := map[string]bool{in.DocUUID: d.Level >= accessBusiness.LevelEdit}
	if in.WithChildren {
		ids = nil
		err := treeBusiness.Hierarchy.Walk(ctx, accessBusiness.KindDoc, in.DocUUID, func(n treeBusiness.Node) error {
			if len(ids) > business.DuplicateCap {
				return business.ErrTooMany
			}
			ids = append(ids, n.UUID)
			return nil
		})
		if err != nil {
			treeFailed(w, r, "DuplicateDoc", err)
			return
		}
		res := make([]accessBusiness.Resource, len(ids))
		for i, id := range ids {
			res[i] = docRes(id)
		}
		decided, err := accessBusiness.ResolveAll(ctx, who, res)
		if err != nil {
			treeFailed(w, r, "DuplicateDoc", err)
			return
		}
		ids = ids[:0]
		for _, one := range res {
			if sub, ok := decided[one]; ok && sub.Level >= accessBusiness.LevelView {
				ids = append(ids, one.UUID)
				editable[one.UUID] = sub.Level >= accessBusiness.LevelEdit
			}
		}
	}
	under, err := duplicateBeside(ctx, who, in.DocUUID, editable[in.DocUUID])
	if err != nil {
		treeFailed(w, r, "DuplicateDoc", err)
		return
	}
	dup, err := business.DuplicateDocs(ctx, &user.UserDgraphInfo, ids, editable, under)
	if err != nil {
		treeFailed(w, r, "DuplicateDoc", err)
		return
	}
	// Every shared copy takes the sharing of the page the copy goes beside,
	// as any new subpage does: one read of it, and one write for them all.
	// Any other copy stays its maker's alone.
	var shared []accessBusiness.Resource
	for _, c := range dup.Copies {
		if c.Shared {
			shared = append(shared, docRes(c.UUID))
		}
	}
	if len(shared) > 0 {
		if err := accessBusiness.Rules.CopyOnCreate(ctx, who, docRes(under.Uuid), shared...); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/DuplicateDoc: the copies of %s didn't all take their parent's sharing: %v", in.DocUUID, err)
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Duplicated.", "data": dup})
}

// duplicateBeside is the page a copy of docUUID goes right after the page
// under: its parent, when the person may edit the page and add to the parent
// (copying a page is publishing it to whoever the parent is open to, which
// only someone who may edit it may do). nil for the top of their own private
// pages. A read that fails is an error, never a quiet change of place.
func duplicateBeside(ctx context.Context, who accessBusiness.Subject, docUUID string, editable bool) (*dgraphStruct.DgraphDoc, error) {
	if !editable {
		return nil, nil
	}
	rows, err := business.DocRows(ctx, []string{docUUID}, who.UserUID)
	if err != nil || len(rows) == 0 || rows[0].Parent == nil {
		return nil, err
	}
	parent := rows[0].Parent.Uuid
	if _, err := accessBusiness.Open(ctx, who, docRes(parent), accessBusiness.LevelEdit); err != nil {
		if accessBusiness.Refused(err) {
			return nil, nil
		}
		return nil, err
	}
	prow, err := business.DocRows(ctx, []string{parent}, who.UserUID)
	if err != nil || len(prow) == 0 {
		return nil, err
	}
	return prow[0], nil
}
