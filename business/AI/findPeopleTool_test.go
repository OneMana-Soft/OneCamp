package business

import (
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestFindPeopleIsRegisteredReadOnly(t *testing.T) {
	if _, ok := ai.Executors["find_people"]; !ok {
		t.Fatal("find_people has no executor, so the model can propose it and nothing runs")
	}
	if !ai.ToolIsReadOnly("find_people") {
		t.Error("a directory lookup is being treated as a write")
	}
}

// Every agent has a real users row so it can author as itself. Without the bot
// filter, "who works on billing" answers with the other agents.
func TestBotsAreExcludedFromPeopleSearch(t *testing.T) {
	src := readSource(t, "../../domain/User/userDomian.go")
	i := strings.Index(src, "func SearchPeople")
	if i < 0 {
		t.Fatal("SearchPeople not found")
	}
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "NOT eq(is_bot, true)") {
		t.Error("agents would be returned as people")
	}
	if !strings.Contains(body, "NOT eq(is_external, true)") {
		t.Error("external users would be returned as members")
	}
}

// The needle reaches a regex. It is a model-produced string, so it must go
// through the same escaping the other search uses.
func TestPeopleSearchEscapesItsNeedle(t *testing.T) {
	src := readSource(t, "../../domain/User/userDomian.go")
	i := strings.Index(src, "func SearchPeople")
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "escapeForDgraphRegex(needle)") {
		t.Fatal("the people search interpolates an unescaped needle into a regex")
	}
}

// GetDgraphUsersList unmarshals a field tagged json:"userInfo". Any other block
// name parses cleanly into an empty slice: no error, no rows, a tool that always
// says nobody was found.
func TestQueryBlockIsNamedUserInfo(t *testing.T) {
	src := readSource(t, "../../domain/User/userDomian.go")
	i := strings.Index(src, "func SearchPeople")
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "userInfo(func: has(user_uuid)") {
		t.Error("the result block is not named userInfo, so nothing will unmarshal")
	}
}

// A directory is not something to page through one tool call at a time.
func TestPeopleResultsAreBounded(t *testing.T) {
	src := readSource(t, "../../domain/User/userDomian.go")
	if !strings.Contains(src, "peopleSearchMaxResults = ") {
		t.Error("no bound on how many people one lookup returns")
	}
}
