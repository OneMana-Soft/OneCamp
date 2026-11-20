package level

import "slices"

// OneRoleEach is a role change's removals so that each person it adds as an
// editor, a commenter or a viewer holds that role alone: whoever is added to
// one is taken off the other two. Each list comes back in order, once each,
// with no empty ids. Docs' and boards' permission changes both ask it
// (business/Doc UpdateDocPermissions, business/Board oneRoleEach). Pure.
func OneRoleEach(addEdit, addComment, addView, removeEdit, removeComment, removeView []string) (edit, comment, view []string) {
	return Unique(slices.Concat(removeEdit, addView, addComment)),
		Unique(slices.Concat(removeComment, addEdit, addView)),
		Unique(slices.Concat(removeView, addEdit, addComment))
}

// Unique is ids once each, in their order, without empty ones; never nil, so
// a list stored from it is an empty array, not NULL.
func Unique(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
