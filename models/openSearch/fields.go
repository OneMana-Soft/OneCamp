package models

// ChatGroupingIDField is the field that holds a chat entry's grouping id
// whole: every exact match on a chat's grouping id goes through it. The chats
// index was created without a mapping for chat_grp_id, so the field is text,
// cut into words that a whole id never equals, with the whole value under
// .keyword; new indexes map it the same way on purpose
// (initializers/opensearchInit), so this one name is right on every install.
// A term on chat_grp_id itself never matched: a group's re-key, when someone
// was added, left its messages under the old id. A comment's and a file's
// grouping ids (comment_chat_grp_id, attachment_chat_grp_id) are keywords.
const ChatGroupingIDField = "chat_grp_id.keyword"
