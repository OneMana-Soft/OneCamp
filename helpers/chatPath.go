package helpers

import "strings"

// IsDMGroupingID is whether a DM's or group's grouping id is a DM's: two
// people's ids with a space between them (GetGroupingId). A group's has none.
func IsDMGroupingID(groupingID string) bool {
	return strings.Contains(strings.TrimSpace(groupingID), " ")
}

// ChatPath is where a conversation opens in the web app for viewerUUID, or
// "" without a grouping id. A group opens at /app/chat/group/<grouping id>.
// A DM's grouping id is both people's ids with a space between them
// (GetGroupingId), and the web app reads the segment after /app/chat/ as the
// other person's id, so a DM opens at /app/chat/<the other person>; one with
// oneself opens at oneself. With messageUUID, the path opens that message
// with its thread.
//
// One rule for every link the server writes: email, the AI's citations, the
// unread summary. A link built from the grouping id itself opened a
// conversation "with" nobody. The web app's copy of this rule is chatPath in
// lib/routes/conversationRoute.ts.
func ChatPath(groupingID, viewerUUID, messageUUID string) string {
	grp := strings.TrimSpace(groupingID)
	if grp == "" {
		return ""
	}
	var path string
	if !strings.Contains(grp, " ") {
		path = "/app/chat/group/" + grp
	} else {
		other := viewerUUID
		for _, id := range strings.Fields(grp) {
			if id != viewerUUID {
				other = id
				break
			}
		}
		path = "/app/chat/" + other
	}
	if messageUUID != "" {
		path += "/" + messageUUID
	}
	return path
}
