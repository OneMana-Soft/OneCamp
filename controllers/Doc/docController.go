package docController

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"sort"
	"strconv"

	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	business "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
)

func GetDocInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	docUUIDString := chi.URLParam(r, "doc_uuid")
	_, err := uuid.Parse(docUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocInfo Failed to parse docUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse docUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphDoc, err := business.GetDocByDocUUID(ctx, docUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocInfo Failed to get dgraph doc info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to dgraph doc info from dgraph",
			"err": err,
		})
		return
	}

	if *dgraphDoc.IsPrivate == true && dgraphDoc.HasEditAccess == 0 && dgraphDoc.HasReadAccess == 0 && dgraphDoc.HasCommentAccess == 0 && dgraphDoc.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	dgraphDoc.MqttTopic = helpers.GetMqttTopicForDoc(dgraphDoc.Uuid)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got docInfo successfully!", "data": dgraphDoc})

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

	dgraphReaction, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, reactionInfo.ReactionDgraphUid)
	if err != nil || dgraphReaction == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentDoc Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	if dgraphReaction.AddedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "You can only remove your own reaction.",
		})
		return
	}

	commentDgraphInfo, err := commentBusiness.GetDgraphDocCommentInfoByUUID(ctx, reactionInfo.CommentId, userInfo.UserDgraphInfo.Uid)

	if err != nil || commentDgraphInfo == nil || !commentDgraphInfo.Doc.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentDoc Failed to get comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment dgraph info",
			"err": err,
		})
		return
	}

	err = business.DeleteReactionOnCommentDoc(ctx, commentDgraphInfo, dgraphReaction.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentDoc Failed to delete comment reaction info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete comment reaction",
			"err": err,
		})
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
	if err != nil || dgraphComment == nil || !dgraphComment.Doc.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateDocCommentReaction Failed to get dgraph comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph comment",
			"err": err,
		})
		return
	}

	if *dgraphComment.Doc.IsPrivate == false && dgraphComment.Doc.HasEditAccess == 0 && dgraphComment.Doc.HasReadAccess == 0 && dgraphComment.Doc.HasCommentAccess == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	reactionInfo.ReactionDgraphUid, err = business.CreateOrUpdateDocCommentReaction(ctx, &reactionInfo, dgraphComment, &userInfo.UserDgraphInfo)
	if err != nil || reactionInfo.ReactionDgraphUid == "" {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateDocCommentReaction Failed to create/update reaction on comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create/update reaction on comment",
			"err": err,
		})
		return
	}

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
			"msg": "Failed to dgraph doc comment info from dgraph",
			"err": err,
		})
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
			"msg": "Failed to delete comment from dgraph",
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
			"msg": "Failed to dgraph doc comment info from dgraph",
			"err": err,
		})
		return
	}

	if dgraphComment.CommentBy.Uuid != userInfo.UserDgraphInfo.Uuid {

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

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDocComment Failed to get dgraph doc info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to dgraph doc info from dgraph",
			"err": err,
		})
		return
	}

	if *dgraphBasicDoc.PublicComment == false && dgraphBasicDoc.HasEditAccess == 0 && dgraphBasicDoc.HasCommentAccess == 0 && dgraphBasicDoc.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {

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
			"msg": "Failed to create comment in dgraph",
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
	pageSizeStr := queryParams["pageSize"]
	pageIndexStr := queryParams["pageIndex"]

	if len(pageSizeStr) == 0 || len(pageIndexStr) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	var err error
	pageSize := 0

	if len(pageSizeStr) != 0 {
		pageSize, err = strconv.Atoi(pageSizeStr[0])
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/GetPrivateDocList Failed to parse pageSize query param to int err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}
	}

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

	dgraphDocList, err := business.GetPrivateDoc(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetPrivateDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get doc list from dgraph",
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
	pageSizeStr := queryParams["pageSize"]
	pageIndexStr := queryParams["pageIndex"]

	if len(pageSizeStr) == 0 || len(pageIndexStr) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	var err error
	pageSize := 0

	if len(pageSizeStr) != 0 {
		pageSize, err = strconv.Atoi(pageSizeStr[0])
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/GetPublicDocList Failed to parse pageSize query param to int err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}
	}

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

	dgraphDocList, err := business.GetPublicDoc(ctx, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetPublicDocList Failed to get doc list from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get doc list from dgraph",
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
	_, err := uuid.Parse(docUUIDString)

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

	if *docDgraph.IsPrivate == false && docDgraph.HasEditAccess == 0 && docDgraph.HasReadAccess == 0 && docDgraph.HasCommentAccess == 0 && userInfo.UserDgraphInfo.Uuid != docDgraph.CreatedBy.Uuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	// Merge external guest comments (capability=comment) into the thread so
	// members see and can respond to client/contractor feedback. Guest comments
	// live in an isolated store (never the core comments table); we map them to
	// the same wire shape with a guest-namespaced author and NO profile key, so
	// the UI shows "Guest: <name>" with no member-profile surface. Bodies are
	// plain text and HTML-escaped here (the UI renders comment_text as HTML).
	mergeGuestDocComments(ctx, docDgraph, docUUIDString)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created doc successfully!", "data": docDgraph})

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
		created := gc.CreatedAt
		docDgraph.Comments = append(docDgraph.Comments, &dgraphStruct.DgraphComment{
			DType:     []string{"Comment"},
			Uuid:      gc.ID.String(),
			Text:      "<p>" + html.EscapeString(gc.Body) + "</p>",
			CreatedAt: &created,
			CommentBy: &dgraphStruct.DgraphUser{
				// Guest-namespaced uuid can never collide with a member; no
				// profile key means the UI renders no member-profile surface.
				Uuid:     "guest-" + gc.ID.String(),
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

func CreateDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var docInfo adapter.InputCreateDoc

	err := json.NewDecoder(r.Body).Decode(&docInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDoc Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphDoc, err := business.CreateDoc(ctx, &userInfo.UserDgraphInfo, &docInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateDoc Failed to create doc in dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create doc in dgraph",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created doc successfully!", "data": dgraphDoc})

}

func DeleteDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var docInfo adapter.InputUpdateDoc

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	err := json.NewDecoder(r.Body).Decode(&docInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDoc Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	docDgraph, err := business.GetBasicDgraphDocByUUID(ctx, docInfo.DocId, userInfo.UserDgraphInfo.Uid)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDoc Failed to get dgraph doc err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph doc",
			"err": err,
		})
		return
	}

	if docDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	err = business.DeleteDoc(ctx, docInfo.DocId)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteDoc Failed to delete dgraph doc err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete dgraph doc",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted doc successfully!"})

}

func UpdateDocBody(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var docInfo adapter.InputUpdateDoc

	err := json.NewDecoder(r.Body).Decode(&docInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocBody Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	err = business.UpdateDoc(ctx, &docInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocBody Failed to update doc body err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update doc body",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated docInfo successfully!"})

}

func UpdateDocFromCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var collabInput adapter.InputDocCollabUpdate

	err := json.NewDecoder(r.Body).Decode(&collabInput)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocFromCollab Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	// Map to InputUpdateDoc
	// We'll store the HTML content as the doc body for now
	err = business.UpdateDocFromCollab(ctx, collabInput.DocUuid, collabInput.HtmlContent, collabInput.Contributors)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocFromCollab Failed to update doc body from collab err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update doc body",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated docInfo successfully!"})
}

func UpdateDocPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateDocPermissions
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocPermissions Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	docDgraph, err := business.GetBasicDgraphDocByUUID(ctx, input.DocId, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateDocPermissions Failed to get dgraph doc info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph doc info",
			"err": err,
		})
		return
	}

	if docDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.UpdateDocPermissions(ctx, input, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateDocPermissions Failed to update doc permmssion %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update doc permmssion",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated doc permissions successfully!"})
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

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocPermissions Failed to get dgraph doc info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph doc info",
			"err": err,
		})
		return
	}

	if docDgraph.HasEditAccess == 0 && docDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	doc, err := business.GetDocPermissions(ctx, docUUID, userUID)
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

func GetDocForCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	docUUIDString := chi.URLParam(r, "doc_uuid")

	// Use GetSystemDocByUUID for internal fetch (no user context needed)
	dgraphDoc, err := business.GetSystemDocByUUID(ctx, docUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocForCollab Failed to get dgraph doc info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to dgraph doc info from dgraph",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Success",
		"data": dgraphDoc,
	})
}

func DocCollabAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var docInfo adapter.InputUpdateDoc

	err := json.NewDecoder(r.Body).Decode(&docInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DocCollabAuthorize Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	dgraphDoc, err := business.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docInfo.DocId, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphDoc == nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DocCollabAuthorize Failed to get doc info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel list",
			"err": err,
		})
		return

	}

	if dgraphDoc.HasEditAccess == 0 && dgraphDoc.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Unauthorized doc access",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Authorised",
	})

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

	if inputDocName.PageSize == 0 {
		inputDocName.PageSize = 20
	}

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
			"msg": "Failed to get doc list from dgraph",
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

	if inputDocName.PageSize == 0 {
		inputDocName.PageSize = 20
	}

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
			"msg": "Failed to get doc list from dgraph",
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

	users, err := business.SearchUsersForDoc(ctx, userInfo.UserDgraphInfo.Uuid, sanitizeOrEmpty(searchInput.SearchText))
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
func sanitizeOrEmpty(raw string) string {
	safe, err := dgraphquery.SanitizeSearchTerm(raw)
	if err != nil {
		return ""
	}
	return safe
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

	q := r.URL.Query()
	pageSize := 50
	if v := q.Get("pageSize"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 && n <= 200 {
			pageSize = n
		}
	}
	pageIndex := 0
	if v := q.Get("pageIndex"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			pageIndex = n
		}
	}

	page, err := business.ListDocViewers(ctx, docUUID, userInfo.UserDgraphInfo.Uid, pageSize, pageIndex*pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDocViewers failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get doc viewers", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": page.Viewers, "count": page.Total, "has_more": page.HasMore})
}
