package models

// UUIDsOf is the users' uuids in their order, leaving out a nil user or one
// without a uuid. Pure.
func UUIDsOf(users []*DgraphUser) []string {
	var ids []string
	for _, u := range users {
		if u != nil && u.Uuid != "" {
			ids = append(ids, u.Uuid)
		}
	}
	return ids
}
