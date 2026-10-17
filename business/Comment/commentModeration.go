package business

// Who may remove a comment (docs programme A5): its author, while they can
// still read what it is on; and a moderator, for any comment, abusive ones
// above all. A moderator is the resource's owner, someone with full access to
// it, or a workspace admin, and only while it isn't deleted.
//
// The Access Resolver (business/Access) decides the levels; it is built on
// packages that use this one, so the caller hands its answer in.

// Moderator is what moderating a resource's comments is decided by.
type Moderator struct {
	Owner      bool // owns the resource
	FullAccess bool // has full access to it
	Admin      bool // a workspace admin, acting themselves
	Deleted    bool // the resource is in the trash
}

// CanModerate is whether they may remove anyone's comment on the resource.
func CanModerate(m Moderator) bool {
	return !m.Deleted && (m.Owner || m.FullAccess || m.Admin)
}

// CanRemoveComment is whether they may remove one comment: their own, on a
// resource they can still read, or anyone's as a moderator.
func CanRemoveComment(isAuthor, canRead bool, m Moderator) bool {
	return (isAuthor && canRead && !m.Deleted) || CanModerate(m)
}
