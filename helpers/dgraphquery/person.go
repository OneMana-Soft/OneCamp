package dgraphquery

// PersonByUUID is the root function that names a person's own graph node by
// their uuid, as in `user as var(func: `+PersonByUUID("$id")+`)`. value is a
// query variable or a quoted uuid. A person's own node is the first one
// holding their user_uuid (the lowest uid, made when they joined), the one
// every read of a person by uuid takes: imports made one more node holding a
// person's user_uuid for each file they kept, so a write through every node
// with the uuid reaches those too, and a check that the uuid names one node
// fails for that person.
func PersonByUUID(value string) string {
	return "eq(user_uuid, " + value + "), first: 1"
}
