package business

// A Slack message is known by its conversation and its ts. Slack's ts is
// unique within a conversation, not across them: two conversations can each
// hold a message at one ts. Known by its ts alone, the second of two such
// messages was taken as already imported and dropped, and a reply could be
// filed in another conversation's thread, for its people to read (a private
// channel's reply under a public channel's post). The import's maps, its own
// and the workspace's, key a message and a reply this way, and so does
// everything that finds one: its edits and deletion, its reactions, its pins
// and its files.

// messageKey is the map key of the message at ts in conversation conv: a
// Slack channel's, group's, DM's or group DM's id. Pure.
func messageKey(conv, ts string) string { return conv + "/" + ts }
