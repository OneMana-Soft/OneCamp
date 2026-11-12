package controllers

import (
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/Activity"
	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

func GetLatestMentions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	page := helpers.PageParam(r.URL.Query(), 20, 50)
	pageIndex, pageSize := page.Index, page.Size

	mentionsPage, err := business.GetMentionsByUserId(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestMentions Failed to user's mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to user's mentions",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got mentions successfully!", "data": mentionsPage})
}

func GetLatestComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	page := helpers.PageParam(r.URL.Query(), 20, 50)
	pageIndex, pageSize := page.Index, page.Size

	commentsPage, err := business.GetCommentsByUserId(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestComments Failed to user's comments err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's comments",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got comments successfully!", "data": commentsPage})
}

func GetLatestReactions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	page := helpers.PageParam(r.URL.Query(), 20, 50)
	pageIndex, pageSize := page.Index, page.Size

	reactionsPage, err := business.GetReactionsByUserId(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestReactions Failed to user's reactions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to user's reactions",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got reactions successfully!", "data": reactionsPage})
}

// A page of the feed: 20 items when the app doesn't say, and never more than
// maxActivityPage.
const activityPageSize, maxActivityPage = 20, 50

// unifiedActivity is the feed's read, a seam for tests.
var unifiedActivity = business.GetUnifiedActivity

func GetUnifiedActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	timeValue := r.URL.Query()["beforeTime"]

	// The client's page size, within a page: the limit drives the notices
	// too (limit+1 of them, each a Postgres read, an access check and a
	// title read), and an uncapped one read as many as it asked for.
	limit := helpers.LimitParam(r.URL.Query(), activityPageSize, maxActivityPage)
	// Default to current time if not provided
	beforeTime := time.Now()

	if len(timeValue) > 0 && timeValue[0] != "" {
		// To the nanosecond, as the feed writes times; whole seconds from an
		// older client still parse.
		parsedTime, err := time.Parse(time.RFC3339Nano, timeValue[0])
		if err == nil {
			beforeTime = parsedTime
		}
	}

	// Notices ("NOTICE" items: a mention in a doc's body, "shared with you", a
	// proposal) only for an app that says it draws them: an older app meets an
	// activity type it has no badge for and stops drawing the feed.
	unifiedPage, err := unifiedActivity(ctx, business.FeedQuery{
		User: userInfo.UserPostgresInfo.Id, DgraphID: userInfo.UserDgraphInfo.Uid, Before: beforeTime, Limit: limit,
		Notices: r.URL.Query().Get("notices") == "1",
	})

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUnifiedActivity Failed to get unified activity err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get unified activity",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got unified activity successfully!", "data": unifiedPage})
}

// Seams for tests: opening the Inbox, and counting its badge.
var (
	inboxSeen   = readStateBusiness.InboxSeen
	unreadCount = business.UnreadActivityCount
)

// MarkActivitySeen is POST /activity/seen, the Inbox opened: everything in the
// signed-in person's feed until now is seen, and the badge counts from here.
// It answers the badge after the write ({"count": n}; none when it couldn't be
// counted, and the app asks GET /activity/unread), and tells their own tabs and
// devices (read.moved, kind inbox). The Inbox calls it; reading the feed marks
// nothing, or Home, which reads it, cleared the badge on every load. Only the
// caller's own last look moves.
func MarkActivitySeen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	count, err := inboxSeen(ctx, &userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/MarkActivitySeen Failed to mark the feed seen err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't mark your Inbox read. Try again."})
		return
	}
	data := helpers.Envolope{}
	if count != nil {
		data["count"] = *count
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Marked your Inbox read.", "data": data})
}

// GetActivityUnread is GET /activity/unread: the signed-in person's Inbox badge,
// {"count": n} up to 100 (the app shows 99+), counted from their own feed with
// its notices by the rule the Inbox marks its items by (business/Activity
// UnreadActivityCount), so it never counts what they can't open. A count that
// can't be read is 503, never a 0 the server didn't count.
func GetActivityUnread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}

	n, err := unreadCount(ctx, userInfo.UserPostgresInfo.Id, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetActivityUnread Failed to count the Inbox err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't count your Inbox. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Counted your Inbox.", "data": helpers.Envolope{"count": n}})
}
