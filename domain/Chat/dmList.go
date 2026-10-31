package domain

// The DM list a page at a time (DG4): newest first by each DM's last time
// (dm_last_chat_at, which every chat write moves forward: models/dgraph
// dmLastChat.go), then by grouping id, so DMs whose last times are equal still
// page in one fixed order with nothing repeated or skipped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// dmListFields is what the DM list shows of each DM: its people and its
// newest live message.
const dmListFields = `						dm_grouping_id
						dm_name
						dm_participants @filter(` + dmParticipantVisible + `) {
							user_uuid
							user_name
							user_full_name
							user_profile_object_key
							is_bot
							user_device_connected
							user_status
						}
						dm_chats @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc:chat_created_at, first: 1) @cascade( chat_from){
							chat_uuid
							chat_body_text
							chat_created_at
							chat_kind
							chat_system
							chat_copy_of
							chat_from {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
								user_device_connected
								user_status
							}
							
							chat_attachments {
								attachment_uuid
								attachment_file_name
								attachment_obj_key
								attachment_width
								attachment_height
								attachment_size
								attachment_raw_type
								attachment_type
								attachment_duration
								attachment_created_at
							}
						}
`

// DMEdge is where a page of the DM list starts: just past the DM with this
// last time and grouping id.
type DMEdge struct {
	At         time.Time
	GroupingID string
}

// dmEdgeEarliest is the earliest last time a DM has: the backfill gives a DM
// whose messages carry no time this one, so it sorts last.
var dmEdgeEarliest = time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)

// ParseDMEdge is the edge a client sent (?at=, RFC 3339; ?grp_id=), checked:
// the time is one a DM can have, and the id is a grouping id as the app
// writes one. The edge only bounds a read of the caller's own DMs, so an id
// that names someone else's conversation reads nothing of it.
func ParseDMEdge(at, groupingID string) (DMEdge, error) {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return DMEdge{}, errors.New("at must be a DM's last time, RFC 3339")
	}
	if t.Before(dmEdgeEarliest) || t.After(time.Now().Add(24*time.Hour)) {
		return DMEdge{}, errors.New("at is out of range")
	}
	if !isGroupingID(groupingID) {
		return DMEdge{}, errors.New("grp_id must be a conversation's id")
	}
	return DMEdge{At: t.UTC(), GroupingID: groupingID}, nil
}

// DMPage is up to first of the person's DMs, newest first, past edge (from
// the top when nil), and, of unread (grouping ids), those the person is in:
// one round trip. A DM whose messages are all deleted comes back with none,
// so the caller can page past it.
func DMPage(ctx context.Context, userUUID string, edge *DMEdge, first int, unread []string) (page, unreadDMs []*dgraphStruct.DgraphDm, err error) {
	vars := map[string]string{"$user_id": userUUID}
	filter := "has(dm_last_chat_at)"
	if edge != nil {
		filter += " AND (lt(dm_last_chat_at, $at) OR (eq(dm_last_chat_at, $at) AND lt(dm_grouping_id, $grp)))"
		vars["$at"] = edge.At.Format(time.RFC3339Nano)
		vars["$grp"] = edge.GroupingID
	}
	unreadBlock := ""
	if list, n := quotedList(unread, isGroupingID); n > 0 {
		unreadBlock = fmt.Sprintf(`unread_dms: user_dms @filter(eq(dm_grouping_id, %s)) {
				dm_last_chat_at
%s			}`, list, dmListFields)
	}
	got, err := dgexec.Read[struct {
		UserInfo []struct {
			DMs    []*dgraphStruct.DgraphDm `json:"user_dms"`
			Unread []*dgraphStruct.DgraphDm `json:"unread_dms"`
		} `json:"userInfo"`
	}](ctx, fmt.Sprintf(`query DMPage($user_id: string, $at: string, $grp: string) {
		userInfo(func: eq(user_uuid, $user_id)) {
			user_dms @filter(%s) (orderdesc: dm_last_chat_at, orderdesc: dm_grouping_id, first: %d) {
				dm_last_chat_at
%s			}
			%s
		}
	}`, filter, first, dmListFields, unreadBlock), vars)
	if err != nil || len(got.UserInfo) == 0 {
		return nil, nil, err
	}
	return got.UserInfo[0].DMs, got.UserInfo[0].Unread, nil
}

// StampUnstampedDMs gives up to batch DMs that have messages and no last time
// their newest message's time (a deleted message counts: the time only orders
// the list), in one upsert, and says how many it stamped; zero means none is
// left. A DM whose messages carry no time gets the earliest time, so the
// backfill never stalls on it. Run again, it finds nothing to stamp.
func StampUnstampedDMs(ctx context.Context, batch int) (int, error) {
	q := fmt.Sprintf(`query {
		d as var(func: type(Dm), first: %d) @filter(not has(dm_last_chat_at) AND has(dm_chats)) {
			dm_chats { t as chat_created_at }
			last as max(val(t))
			timed as count(dm_chats @filter(has(chat_created_at)))
		}
		untimed as var(func: uid(d)) @filter(eq(val(timed), 0))
		stamped(func: uid(d)) { count(uid) }
	}`, batch)
	res, err := dgexec.Mutate(ctx, q, nil,
		dgexec.Mutation{Cond: "@if(gt(len(d), 0))", SetNQuads: "uid(d) <dm_last_chat_at> val(last) ."},
		dgexec.Mutation{Cond: "@if(gt(len(untimed), 0))", SetNQuads: fmt.Sprintf("uid(untimed) <dm_last_chat_at> %q .", dmEdgeEarliest.Format(time.RFC3339))},
	)
	if err != nil {
		return 0, err
	}
	var out struct {
		Stamped []struct {
			Count int `json:"count"`
		} `json:"stamped"`
	}
	if err := json.Unmarshal(res.GetJson(), &out); err != nil || len(out.Stamped) == 0 {
		return 0, err
	}
	return out.Stamped[0].Count, nil
}
