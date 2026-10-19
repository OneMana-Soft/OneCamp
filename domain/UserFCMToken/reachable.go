package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserFCMToken"
	"github.com/google/uuid"
)

// Who is told, by person rather than by device.
//
// Every push used to be decided by a token lookup: the push gate sat in the
// same query as the join on users_fcm_token, so a person was only ever
// considered through a device that had registered for push. The desktop app's
// webview has no web push and registers nothing, so someone who used only the
// desktop app was never anyone's recipient (TN-BUG-12). These read the gate
// for each person whatever devices they have, and hand back the tokens of
// those it lets through beside them: one read for both deliveries
// (business/Notification Deliver).

// reachableQuery is who of $1 the gate (pushGate) lets through now, each with
// their devices' tokens (none for someone with no device, who is still told).
func reachableQuery() string {
	return `
	SELECT u.id::text, t.fcm_token
	FROM unnest($1::uuid[]) AS u(id)
	LEFT JOIN users_fcm_token t ON t.user_id = u.id
	WHERE` + pushGate("u.id")
}

// reachableThroughPauseQuery is reachableQuery without the push gate, for
// the one notice that goes through a pause (Notify anyway, one a day per
// sender). A deactivated person is still never told.
var reachableThroughPauseQuery = `
	SELECT u.id::text, t.fcm_token
	FROM unnest($1::uuid[]) AS u(id)
	LEFT JOIN users_fcm_token t ON t.user_id = u.id
	WHERE` + stillActive("u.id")

// stillActive keeps only people whose account is active. Deactivation
// (business/User DeactivateUser) stamps users.deleted_at and cuts MQTT, but
// leaves their channel settings and their devices' tokens in place: without
// this, a deactivated person's phone went on being sent message text.
func stillActive(userCol string) string {
	return ` EXISTS (SELECT 1 FROM users x WHERE x.id = ` + userCol + ` AND x.deleted_at IS NULL)`
}

// ReachableNow is who of userIds may be told of something now: not
// deactivated (stillActive), not paused, outside their quiet hours, not in
// focus time (notPaused). users keeps the order they were asked in, each
// once; tokens are their devices'. An id that
// isn't a uuid is skipped, never failing the read for everyone else.
func ReachableNow(ctx context.Context, userIds []string) (users []string, tokens []string, err error) {
	return reachable(ctx, reachableQuery(), userIds)
}

// ReachableThroughPause is ReachableNow without the gate.
func ReachableThroughPause(ctx context.Context, userIds []string) (users []string, tokens []string, err error) {
	return reachable(ctx, reachableThroughPauseQuery, userIds)
}

func reachable(ctx context.Context, query string, userIds []string) (users []string, tokens []string, err error) {
	ids := WellFormedUserIDs(userIds)
	if len(ids) == 0 {
		return nil, nil, nil
	}
	rows, err := models.GetReachableUsersAndTokens(ctx, query, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ReachableNow Failed to read who may be told err: %+v", err)
		return nil, nil, err
	}
	pass := make(map[string]bool, len(rows))
	seenToken := map[string]bool{}
	for _, r := range rows {
		pass[r.UserID] = true
		if r.Token != "" && !seenToken[r.Token] {
			seenToken[r.Token] = true
			tokens = append(tokens, r.Token)
		}
	}
	for _, id := range ids {
		if pass[id] {
			users = append(users, id)
		}
	}
	return users, tokens, nil
}

// WellFormedUserIDs is ids that are uuids, lower-cased (as Postgres writes a
// uuid back), each once, in the order given. Pure.
func WellFormedUserIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		s := u.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
