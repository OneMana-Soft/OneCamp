package docController

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	business "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/controllers/httpx"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
)

// docGone answers for a doc in the trash: 410 to someone it was there for,
// so the app can say it was deleted, and to anyone else the 404 any missing
// doc gets, which tells them nothing.
func docGone(w http.ResponseWriter, couldSee bool) {
	if couldSee {
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true})
		return
	}
	helpers.WriteDocNotFound(w)
}

// docListPage bounds a list's paging as asked of the graph: a page of 1 to
// maxDocListPage docs (defaultDocListPage when none, or less than one, was
// asked for) from a page index of 0 or more (docPageIndex). Neither was
// bounded, so a huge size read every doc at once, and a negative one reached
// the graph's first: argument.
func docListPage(pageIndex, pageSize int) (int, int) {
	if pageSize < 1 {
		pageSize = defaultDocListPage
	}
	pageSize = min(pageSize, maxDocListPage)
	return docPageIndex(pageIndex, pageSize), pageSize
}

// docPageIndex bounds the index of a page of pageSize (1 or more) to 0 or
// more, and to the pages that start before maxDocListOffset. The page starts
// at pageIndex*pageSize, an int: a huge index overflowed it to a negative
// offset, which the graph and Postgres refuse.
func docPageIndex(pageIndex, pageSize int) int {
	return min(max(pageIndex, 0), (maxDocListOffset-1)/pageSize)
}

// docViewersPage is the paging GET /doc/getViewers asks for, as a limit and an
// offset: pages of 50 viewers, or of the 1 to 200 asked for, from page 0 or
// the one asked for (docPageIndex).
func docViewersPage(q url.Values) (limit, offset int) {
	pageSize := helpers.SizeParam(q, "pageSize", 50, 200)
	pageIndex := 0
	if v := q.Get("pageIndex"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			pageIndex = n
		}
	}
	return pageSize, docPageIndex(pageIndex, pageSize) * pageSize
}

const (
	defaultDocListPage = 20
	maxDocListPage     = 100
	// maxDocListOffset is further than anyone pages through a list of docs or
	// of a doc's viewers.
	maxDocListOffset = 1_000_000
)

// docDeleted is the doc page's 410 for a doc in the trash, for someone it was
// there for. It also says whether they may restore it (its owner, full
// access, its editors or an admin: business.CanRestore, the trash's rule,
// which the restore checks again), so the page offers Restore only to them.
func docDeleted(w http.ResponseWriter, canRestore bool) {
	helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true, "can_restore": canRestore})
}

// docMissing answers an error that says the doc isn't there for the person
// (business.ErrDocDeleted: 410; business.ErrNotFound: 404), and reports
// whether it did.
func docMissing(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, business.ErrDocDeleted):
		docGone(w, true)
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteDocNotFound(w)
	default:
		return false
	}
	return true
}

func DeleteReactionOnCommentDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var reactionInfo adapter.InputUpdateReactionForCommentInDoc

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentDoc Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentDgraphInfo, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, reactionInfo.CommentId, userInfo.UserDgraphInfo.Uid)

	if err != nil || commentDgraphInfo == nil || commentDgraphInfo.Doc == nil || commentDgraphInfo.CommentBy == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentDoc Failed to get comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't remove the reaction. Try again.",
			"err": err,
		})
		return
	}
	if business.IsDeleted(commentDgraphInfo.Doc) {
		docGone(w, business.CouldRead(commentDgraphInfo.Doc, userInfo.UserDgraphInfo.Uuid))
		return
	}

	dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, commentDgraphInfo.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
	if refusal != nil {
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
		return
	}

	err = business.DeleteReactionOnCommentDoc(ctx, commentDgraphInfo, dgraphReaction.Uid, &userInfo.UserDgraphInfo)

	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't remove the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "removed comment reaction successfully!"})
}

func CreateOrUpdateDocCommentReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var reactionInfo adapter.InputUpdateReactionForCommentInDoc

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateDocCommentReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, reactionInfo.CommentId, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphComment == nil || dgraphComment.Doc == nil || dgraphComment.CommentBy == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateDocCommentReaction Failed to get dgraph comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't add the reaction. Try again.",
			"err": err,
		})
		return
	}
	if business.IsDeleted(dgraphComment.Doc) {
		docGone(w, business.CouldRead(dgraphComment.Doc, userInfo.UserDgraphInfo.Uuid))
		return
	}

	if !business.CanRead(dgraphComment.Doc, userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	// A uid changes that reaction, and only the caller's own on this comment.
	if reactionInfo.ReactionDgraphUid != "" {
		dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, dgraphComment.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
		if refusal != nil {
			helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
			return
		}
		reactionInfo.ReactionDgraphUid = dgraphReaction.Uid
	}

	// The answer is the reaction: the one added, or the one the person already had.
	created, err := business.CreateOrUpdateDocCommentReaction(ctx, &reactionInfo, dgraphComment, &userInfo.UserDgraphInfo)
	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't add the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}
	reactionInfo.ReactionDgraphUid = created

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created/updated chat comment successfully!", "data": reactionInfo})

}

func DeleteDocComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var docCommentInfo adapter.CreateOrUpdateDocCommentInput

	err := json.NewDecoder(r.Body).Decode(&docCommentInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDocComment Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	commentUUID, err := uuid.Parse(docCommentInfo.UUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDocComment Failed to parse docUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse docUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, docCommentInfo.UUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDocComment Failed to get dgraph doc comment info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't delete the comment. Try again.",
			"err": err,
		})
		return
	}
	// A comment uuid that isn't a doc's comment (or isn't anyone's) is no
	// comment here; it used to reach a nil doc further down.
	if dgraphComment == nil || dgraphComment.Doc == nil || dgraphComment.CommentBy == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such comment."})
		return
	}
	if business.IsDeleted(dgraphComment.Doc) {
		docGone(w, business.CouldRead(dgraphComment.Doc, userInfo.UserDgraphInfo.Uuid))
		return
	}

	if dgraphComment.CommentBy.Uuid != userInfo.UserDgraphInfo.Uuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.DeleteDocComment(ctx, commentUUID, docCommentInfo.DocUuid, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDocComment Failed to delete comment from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't delete the comment. Try again.",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted doc commment successfully!"})

}

func UpdateDocComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var docCommentInfo adapter.CreateOrUpdateDocCommentInput

	err := json.NewDecoder(r.Body).Decode(&docCommentInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocComment Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	commentUUID, err := uuid.Parse(docCommentInfo.UUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocComment Failed to parse docUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse docUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, docCommentInfo.UUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocComment Failed to get dgraph doc comment info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't save your edit. Try again.",
			"err": err,
		})
		return
	}
	if dgraphComment == nil || dgraphComment.Doc == nil || dgraphComment.CommentBy == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such comment."})
		return
	}
	if business.IsDeleted(dgraphComment.Doc) {
		docGone(w, business.CouldRead(dgraphComment.Doc, userInfo.UserDgraphInfo.Uuid))
		return
	}

	// Its author, and only while they can still read the doc: someone a
	// private doc is no longer shared with can't put new words into it.
	if dgraphComment.CommentBy.Uuid != userInfo.UserDgraphInfo.Uuid ||
		!business.CanRead(dgraphComment.Doc, userInfo.UserDgraphInfo.Uuid) {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	mentions, err := helpers.GetMentions(docCommentInfo.CommentBody)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocComment Failed to get comment mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	err = business.UpdateDocComment(ctx, commentUUID, &docCommentInfo, mentionsDgraphUsersList, dgraphComment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocComment Failed to update comment  err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated doc comment successfully!"})

}

func CreateDocComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var docCommentInfo adapter.CreateOrUpdateDocCommentInput

	err := json.NewDecoder(r.Body).Decode(&docCommentInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDocComment Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	dgraphBasicDoc, err := business.GetBasicDgraphDocByUUID(ctx, docCommentInfo.DocUuid, userInfo.UserDgraphInfo.Uid)

	if docMissing(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDocComment Failed to get dgraph doc info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't post the comment. Try again.",
			"err": err,
		})
		return
	}
	if business.IsDeleted(dgraphBasicDoc) {
		docGone(w, business.CouldRead(dgraphBasicDoc, userInfo.UserDgraphInfo.Uuid))
		return
	}

	// Commenting takes being able to read the doc: "anyone may comment" means
	// anyone who can see it, and a deleted doc takes no comments.
	if !business.CanComment(dgraphBasicDoc, userInfo.UserDgraphInfo.Uuid) {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	mentions, err := helpers.GetMentions(docCommentInfo.CommentBody)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDocComment Failed to get comment mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	commentInfoRes, err := business.CreateDocComment(ctx, dgraphBasicDoc, &userInfo, &docCommentInfo, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDocComment Failed to create comment in dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't post the comment. Try again.",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created doc comment successfully!", "data": commentInfoRes})

}

func GetPrivateDocList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	queryParams := r.URL.Query()
	pageIndexStr := queryParams["pageIndex"]

	if !queryParams.Has("pageSize") || len(pageIndexStr) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	var err error
	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 200)

	pageIndex := 0
	if len(pageIndexStr) != 0 {
		pageIndex, err = strconv.Atoi(pageIndexStr[0])
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse pageIndex query param: " + pageIndexStr[0],
				"err": err.Error(),
			})
			return
		}
	}

	pageIndex, pageSize = docListPage(pageIndex, pageSize)
	dgraphDocList, err := business.GetPrivateDoc(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetPrivateDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load your docs.",
			"err": err,
		})
		return
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphDocList.Count + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got doc info successfully!",
		"data":      dgraphDocList,
		"pageCount": pageCount,
	})
}

func GetPublicDocList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	queryParams := r.URL.Query()
	pageIndexStr := queryParams["pageIndex"]

	if !queryParams.Has("pageSize") || len(pageIndexStr) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	var err error
	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 200)

	pageIndex := 0
	if len(pageIndexStr) != 0 {
		pageIndex, err = strconv.Atoi(pageIndexStr[0])
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse pageIndex query param: " + pageIndexStr[0],
				"err": err.Error(),
			})
			return
		}
	}

	pageIndex, pageSize = docListPage(pageIndex, pageSize)
	dgraphDocList, err := business.GetPublicDoc(ctx, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetPublicDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the docs.",
			"err": err,
		})
		return
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphDocList.Count + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got doc info successfully!",
		"data":      dgraphDocList,
		"pageCount": pageCount,
	})
}

func GetAllCommentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	docUUIDString := chi.URLParam(r, "doc_uuid")
	parsedDoc, err := uuid.Parse(docUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllCommentList Failed to parse docUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse docUUID string to uuid",
			"err": err,
		})
		return
	}

	docDgraph, err := business.GetDocCommentList(ctx, docUUIDString, userInfo.UserDgraphInfo.Uid)

	if docMissing(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllCommentList Failed to get comment list from dgarph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment list from dgarph",
			"err": err,
		})
		return
	}
	if business.IsDeleted(docDgraph) {
		docGone(w, business.CouldRead(docDgraph, userInfo.UserDgraphInfo.Uuid))
		return
	}

	if !business.CanRead(docDgraph, userInfo.UserDgraphInfo.Uuid) {
		refuseDoc(ctx, w, userInfo, parsedDoc.String())
		return
	}
	// The comment box is offered by these counts: they say what the Access
	// Resolver says, as GetDocInfo's do. They were the doc's direct shares
	// only, so a member who could comment through its general access, a team,
	// its space, full access or a hand-over found no box (docs incident, 14
	// Oct 2026).
	resolved, err := business.ShowAccessNow(ctx, docDgraph, business.Viewer{UID: userInfo.UserDgraphInfo.Uid, UUID: userInfo.UserDgraphInfo.Uuid})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAllCommentList %s: what the person may do couldn't be decided: %+v", docDgraph.Uuid, err)
		helpers.WriteRetryLater(w, "Couldn't load the comments just now.")
		return
	}
	if resolved.Level < accessBusiness.LevelView {
		refuseDoc(ctx, w, userInfo, parsedDoc.String())
		return
	}

	// Merge external guest comments (capability=comment) into the thread so
	// members see and can respond to client/contractor feedback. Guest comments
	// live in an isolated store (never the core comments table); we map them to
	// the same wire shape with a guest-namespaced author and NO profile key, so
	// the UI shows "Guest: <name>" with no member-profile surface. Bodies are
	// plain text and HTML-escaped here (the UI renders comment_text as HTML).
	mergeGuestDocComments(ctx, docDgraph, docUUIDString)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got comments", "data": docDgraph})

}

// mergeGuestDocComments appends guest comments to the doc's comment slice and
// re-orders the merged list chronologically (stable). Best-effort: a failure to
// load guest comments never blocks the member comment list.
func mergeGuestDocComments(ctx context.Context, docDgraph *dgraphStruct.DgraphDoc, docUUID string) {
	guestComments, err := guestBusiness.ListGuestDocComments(ctx, docUUID)
	if err != nil || len(guestComments) == 0 {
		return
	}
	for _, gc := range guestComments {
		if gc.FromTeam {
			// A member's reply to a guest shows in the doc's guest conversation.
			continue
		}
		created := gc.CreatedAt
		docDgraph.Comments = append(docDgraph.Comments, &dgraphStruct.DgraphComment{
			DType:     []string{"Comment"},
			Uuid:      gc.ID.String(),
			Text:      "<p>" + html.EscapeString(gc.Body) + "</p>",
			CreatedAt: &created,
			CommentBy: &dgraphStruct.DgraphUser{
				// Guest-namespaced uuid can never collide with a member; no
				// profile key means the UI renders no member-profile surface.
				Uuid:     gc.AuthorID,
				UserName: "Guest: " + gc.GuestName,
			},
		})
	}
	// Stable chronological order so guest comments interleave correctly without
	// disturbing the relative order of existing member comments.
	sort.SliceStable(docDgraph.Comments, func(i, j int) bool {
		a, b := docDgraph.Comments[i].CreatedAt, docDgraph.Comments[j].CreatedAt
		if a == nil || b == nil {
			return false
		}
		return a.Before(*b)
	})
}

// UpdateDocPermissions is the share dialog's older route, POST
// /doc/updateDocPermissions, which router.go still declares (deleting that
// line is the owner's call). A5's mount registers the same method and pattern
// after it, so UpdateDocSharing answers, and this handler is reached only if
// that registration ever goes. It is UpdateDocSharing, so either way a change
// goes by the Access Resolver's rules (the A5×A6 join).
//
// On its own it let a doc's owner change its sharing with none of the rules a
// change follows now: the demo's own docs weren't kept from its shared
// visitor; someone taken off kept their live sockets; a grant that was to end
// kept its end when it was given again for good, and so still ended; a change
// wasn't capped at 200 people; and when the sharing before couldn't be read,
// the change was made with no event, so no audit row. It never let anyone but
// the owner share, nor give more than edit.
func UpdateDocPermissions(w http.ResponseWriter, r *http.Request) {
	UpdateDocSharing(w, r)
}

func GetDocPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	docUUID := r.URL.Query().Get("doc_uuid")
	if docUUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "doc_uuid is required"})
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	userUID := userInfo.UserDgraphInfo.Uid

	docDgraph, err := business.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)

	if docMissing(w, err) {
		return
	}
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocPermissions Failed to get dgraph doc info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load who can open the doc.",
			"err": err,
		})
		return
	}
	if business.IsDeleted(docDgraph) {
		docGone(w, business.CouldRead(docDgraph, userInfo.UserDgraphInfo.Uuid))
		return
	}

	if !business.CanEdit(docDgraph, userUID) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	doc, err := business.GetDocPermissions(ctx, docUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Doc/GetDocPermissions %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Success",
		"data": doc,
	})
}

// GetDocForCollab is the collaboration service's read of a doc it opens (GET
// /docColab/getDoc/{doc_uuid}, internal: no person asks it).
func GetDocForCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	docUUIDString := chi.URLParam(r, "doc_uuid")

	// Use GetSystemDocByUUID for internal fetch (no user context needed)
	dgraphDoc, err := business.GetSystemDocByUUID(ctx, docUUIDString)
	if err == nil && dgraphDoc == nil {
		err = business.ErrNotFound
	}
	// The collaboration service refuses to open a doc it could not read, rather
	// than serve it empty (an empty doc's first edit was saved over the body),
	// and retries a read that may work later: so a doc that isn't there (404)
	// and a graph that didn't answer (503) are told apart, by the one rule for
	// a read of one doc.
	if err != nil {
		helpers.WriteDocReadFailure(w, r, err, "controllers/GetDocForCollab")
		return
	}
	// A deleted doc isn't opened either. /authorize lets nobody into one, so
	// this is a session let in a moment before the delete; the service, which
	// refuses to open a doc it can't read rather than serve it empty, opens
	// nothing that could later be saved over it.
	if business.IsDeleted(dgraphDoc) {
		docGone(w, true)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Success",
		"data": dgraphDoc,
	})
}

// DocCollabAuthorize is the live socket's authorisation (POST
// /docColab/authorize). It delegates to the Access Resolver
// (AuthorizeDocSocket, docAccessController.go), which the integrator swaps in
// for this route and then deletes this.
func DocCollabAuthorize(w http.ResponseWriter, r *http.Request) {
	AuthorizeDocSocket(w, r)
}

func SearchPrivateDocList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var inputDocName adapter.InputSearchDocName

	err := json.NewDecoder(r.Body).Decode(&inputDocName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SearchPrivateDocList Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	inputDocName.PageIndex, inputDocName.PageSize = docListPage(inputDocName.PageIndex, inputDocName.PageSize)

	// Sanitise before Dgraph regex interpolation.
	if safe, sErr := dgraphquery.SanitizeSearchTerm(inputDocName.SearchText); sErr != nil {
		if errors.Is(sErr, dgraphquery.ErrEmpty) {
			inputDocName.SearchText = ""
		} else {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search text is too long or invalid"})
			return
		}
	} else {
		inputDocName.SearchText = safe
	}

	dgraphDocList, err := business.GetPrivateDocListWithSearchText(ctx, &userInfo.UserDgraphInfo, &inputDocName)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SearchPrivateDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't search your docs.",
			"err": err,
		})
		return
	}

	pageCount := uint64(1)

	if inputDocName.PageSize > 0 {
		pageCount = (dgraphDocList.Count + uint64(inputDocName.PageSize) - 1) / uint64(inputDocName.PageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got doc info successfully!",
		"data":      dgraphDocList,
		"pageCount": pageCount,
	})
}

func SearchPublicDocList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var inputDocName adapter.InputSearchDocName

	err := json.NewDecoder(r.Body).Decode(&inputDocName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SearchPublicDocList Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	inputDocName.PageIndex, inputDocName.PageSize = docListPage(inputDocName.PageIndex, inputDocName.PageSize)

	if safe, sErr := dgraphquery.SanitizeSearchTerm(inputDocName.SearchText); sErr != nil {
		if errors.Is(sErr, dgraphquery.ErrEmpty) {
			inputDocName.SearchText = ""
		} else {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search text is too long or invalid"})
			return
		}
	} else {
		inputDocName.SearchText = safe
	}

	dgraphDocList, err := business.GetPublicDocListWithSearchText(ctx, &inputDocName)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SearchPublicDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't search the docs.",
			"err": err,
		})
		return
	}

	pageCount := uint64(1)

	if inputDocName.PageSize > 0 {
		pageCount = (dgraphDocList.Count + uint64(inputDocName.PageSize) - 1) / uint64(inputDocName.PageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got doc info successfully!",
		"data":      dgraphDocList,
		"pageCount": pageCount,
	})
}

func SearchUsersForDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInput adapter.SearchInputDocUser
	err := json.NewDecoder(r.Body).Decode(&searchInput)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SearchUsersForDoc Failed to parse request body err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse request body",
			"err": err,
		})
		return
	}

	users, err := business.SearchUsersForDoc(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserPostgresInfo.EmailID, cleanOrEmpty(searchInput.SearchText))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SearchUsersForDoc Failed to search users err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to search users",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Found users",
		"data": users,
	})
}

// sanitizeOrEmpty returns the regex-escaped form of the search term,
// or an empty string when the input is empty / control-only. Bounded-
// length errors return the truncated form (caller decides handling).
// The shape mirrors how callers like SearchUsersForDoc want to behave:
// fall back to "match anything" rather than 400 on a noisy keystroke.
func cleanOrEmpty(raw string) string {
	clean, err := dgraphquery.CleanSearchTerm(raw)
	if err != nil {
		return ""
	}
	return clean
}

// GetDocSnapshots GET /doc/getDocSnapshots?doc_uuid= - document version history.
// Requires edit access or ownership.
func GetDocSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	docUUID := r.URL.Query().Get("doc_uuid")
	if _, err := uuid.Parse(docUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse docUUID", "err": err})
		return
	}

	list, err := business.ListDocSnapshots(ctx, docUUID, userInfo.UserDgraphInfo.Uid)
	if docMissing(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocSnapshots failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get doc version history", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": list})
}

// RestoreDocSnapshot POST /doc/restoreDocSnapshot - restore a doc to a snapshot.
// Requires edit access or ownership. Takes effect on next fresh load.
func RestoreDocSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRestoreDocSnapshot
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.DocId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse docUUID", "err": err})
		return
	}
	snapshotUUID, err := uuid.Parse(input.SnapshotId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse snapshotId", "err": err})
		return
	}

	if err := business.RestoreDocSnapshot(ctx, input.DocId, snapshotUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid); err != nil {
		if docMissing(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/RestoreDocSnapshot failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to restore document", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Document restored. Reopen it (ensure everyone has closed it) to see the restored version."})
}

// RecordDocView POST /doc/recordView - records (deduped, throttled) that the
// caller opened the doc. Best-effort; always returns success.
func RecordDocView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRecordDocView
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.DocId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse docUUID", "err": err})
		return
	}

	if err := business.RecordDocView(ctx, input.DocId, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid); err != nil {
		// Best effort, except about a doc that isn't there.
		if docMissing(w, err) {
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/RecordDocView failed err: %+v", err)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
}

// GetDocViewers GET /doc/getViewers?doc_uuid= - distinct viewers, most-recent
// first. Restricted to owner/editors.
func GetDocViewers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	docUUID := r.URL.Query().Get("doc_uuid")
	if _, err := uuid.Parse(docUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse docUUID", "err": err})
		return
	}

	limit, offset := docViewersPage(r.URL.Query())
	page, err := business.ListDocViewers(ctx, docUUID, userInfo.UserDgraphInfo.Uid, limit, offset)
	if docMissing(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocViewers failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get doc viewers", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": page.Viewers, "count": page.Total, "has_more": page.HasMore})
}

// GetDocInsights GET /doc/getInsights?doc_uuid=: the doc's last 28 days of
// views and viewers, members apart from its page's visitors (C6-3).
func GetDocInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	docUUID := r.URL.Query().Get("doc_uuid")
	if _, err := uuid.Parse(docUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse docUUID"})
		return
	}
	insights, err := business.DocInsights(ctx, docUUID, userInfo.UserDgraphInfo.Uid)
	if docMissing(w, err) {
		return
	}
	if err != nil {
		// The cause stays in the log: a response never carries an error's text.
		helpers.LogErrorWithContext(ctx, "controllers/GetDocInsights failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't read this doc's views just now"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": insights})
}
