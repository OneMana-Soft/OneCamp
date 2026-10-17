package models

// A project as read for one person (IsProjectMember and IsProjectAdmin count
// that person): the rule for what they may do with it and its tasks, in one
// place.

// ReaderIsIn reports whether the person the project was read for is in it,
// as a member or one of its admins: they see its tasks and hear its topic.
func (p *DgraphProject) ReaderIsIn() bool {
	return p != nil && (p.IsProjectMember > 0 || p.IsProjectAdmin > 0)
}

// ReaderRunsIt reports whether the person the project was read for is one of
// its admins, who make and change its tasks.
func (p *DgraphProject) ReaderRunsIt() bool {
	return p != nil && p.IsProjectAdmin > 0
}
