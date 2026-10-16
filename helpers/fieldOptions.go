package helpers

// KeptOptions is the one rule for saving a field's options, for a table's
// select columns and a project's task fields alike: every stored option the
// save doesn't list is kept, after those it lists, and only an option whose id
// the save names in removed is taken out. A save made from a copy read before
// an option was added (another tab, a menu opened on the field as it was) used
// to take that option, and every value holding it, without a word.
//
// listed says whether the save lists a stored option (by id, or however the
// caller matches them). It returns the stored options kept and the ids of
// those taken out, each in stored order; one the save both lists and names in
// removed is listed, so neither. Pure.
func KeptOptions[T any](stored []T, id func(T) string, listed func(T) bool, removed map[string]bool) (kept []T, out []string) {
	for _, o := range stored {
		switch {
		case listed(o):
		case removed[id(o)]:
			out = append(out, id(o))
		default:
			kept = append(kept, o)
		}
	}
	return kept, out
}
