package helpers

import "strings"

// Picking the first usable string out of a list of fallbacks.
//
// This existed TEN TIMES across the codebase under the same name, in three
// different behaviours, which is worse than either having it once or not having
// it at all: the name promised one thing and did another depending on which
// package you were in.
//
//	exact     first v != ""                          (5 copies)
//	blank     first non-whitespace, returned AS IS    (4 copies)
//	trimmed   first non-whitespace, returned TRIMMED  (1 copy)
//
// Two functions rather than one, because the difference between them is real and
// a single "obvious" merge would have changed behaviour at nine call sites
// silently. The names say which is which, so a call site declares its intent
// instead of inheriting whichever variant happened to be in that file.
//
// The middle behaviour is gone deliberately: "this value is blank enough to skip
// but not blank enough to clean" is not a rule anybody chose, it is what you get
// from writing the guard and forgetting the return. It was reading a GitHub
// client secret out of the environment, so an env file with a trailing newline
// produced a secret with a newline in it and an OAuth failure that named nothing.

// FirstNonEmpty returns the first value that is not the empty string, or "".
//
// Exact: a value of " " is returned as " ". Use it where whitespace is content,
// or where the values are already known to be clean.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// FirstNonBlank returns the first value that is not empty or whitespace-only,
// trimmed, or "".
//
// Use it for anything that came from a human, a config file, or the environment,
// which is nearly everything: a trailing newline is never the value somebody
// meant to set.
func FirstNonBlank(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
