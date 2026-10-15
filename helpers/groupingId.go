package helpers

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// DMPeople are the two people a direct conversation's grouping id names, as
// GetGroupingId writes it: both their ids, sorted, with a space between. A
// conversation with oneself names its person twice. ok is false for anything
// else, so an id can name a conversation only one way. Pure.
func DMPeople(groupingID string) (first, second string, ok bool) {
	people := strings.Split(groupingID, " ")
	if len(people) != 2 || !isLowerUUID(people[0]) || !isLowerUUID(people[1]) ||
		GetGroupingId(people[0], people[1]) != groupingID {
		return "", "", false
	}
	return people[0], people[1], true
}

// groupIDShape is how GenerateGroupID writes a group conversation's id.
var groupIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsGroupID reports whether id is written as GenerateGroupID writes a group
// conversation's id. Pure.
func IsGroupID(id string) bool {
	return groupIDShape.MatchString(id)
}

// isLowerUUID is a uuid as people's ids are written: canonical, lower case.
func isLowerUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}
