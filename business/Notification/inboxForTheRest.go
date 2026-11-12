package notification

// InboxForTheRest is told means listed: of the people a thing was told to by
// push or email (told, each with why), those whose feed doesn't list it
// already (listed: the thing's owner, the people it mentions), each to be
// given an inbox item of it alone (InboxOnly: no second push or email). Told
// with the thing's DedupKey, each hears once whichever way it reached them.
// A doc's comment (business/Comment tellDocCommentInbox) and a task's
// (tellTaskCommentInbox) are told this way. Pure.
func InboxForTheRest(told []NoticeRecipient, listed func(string) bool) []NoticeRecipient {
	var to []NoticeRecipient
	for _, r := range told {
		if listed(r.UserUUID) {
			continue
		}
		r.InboxOnly = true
		to = append(to, r)
	}
	return to
}
