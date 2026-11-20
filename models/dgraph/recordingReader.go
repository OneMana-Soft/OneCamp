package models

// ReaderIsIn reports whether the person the recording was read for is in the
// place it is filed under: a member of its channel (ch_is_member), or in its
// DM or group (dm_is_member); and, when place isn't "", whether that place is
// the one asked through (the channel's uuid, the conversation's grouping id).
// False for a nil recording and one filed nowhere. Every recording route asks
// this, so whoever isn't in a recording's place is answered alike (V-1126);
// a moderator's or a workspace admin's rights are the route's own.
func (r *DgraphRecording) ReaderIsIn(place string) bool {
	if r == nil {
		return false
	}
	in := func(member bool, id string) bool { return member && (place == "" || id == place) }
	return (r.Channel != nil && in(r.Channel.IsMember > 0, r.Channel.Uuid)) ||
		(r.Dm != nil && in(r.Dm.ParticipantIsMember > 0, r.Dm.GroupingId))
}
