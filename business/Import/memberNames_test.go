package business

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Members are found by either of their names, however the file spells them;
// someone whose two names are one is one member for it, and two members who
// share a name are both under it, so neither is guessed.
func TestMembersAreIndexedByEachOfTheirNames(t *testing.T) {
	ana, ana2, ben, bot := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	byName, nameOf := membersByName([]*dgraphStruct.DgraphUser{
		{Uuid: ana.String(), UserName: "Ana Lima", UserFullName: "ana  lima"},
		{Uuid: ana2.String(), UserFullName: "Ana Lima"},
		{Uuid: ben.String(), UserName: "Ben", UserFullName: "Benjamin Ray"},
		{Uuid: "not-a-uuid", UserName: "Ghost"},
		{Uuid: bot.String(), UserName: "Ben"},
	}, func(id uuid.UUID) bool { return id != bot })
	if got := byName["ana lima"]; len(got) != 2 || got[0] != ana || got[1] != ana2 {
		t.Errorf("ana lima: %v", got)
	}
	if got := byName["benjamin ray"]; len(got) != 1 || got[0] != ben {
		t.Errorf("a full name finds them: %v", got)
	}
	if got := byName["ben"]; len(got) != 1 || got[0] != ben {
		t.Errorf("a display name finds them, and someone who isn't a member is no one's match: %v", got)
	}
	if _, ok := byName["ghost"]; ok {
		t.Error("a member with no id is no one's match")
	}
	if nameOf[ben] != "Ben" || nameOf[ana2] != "Ana Lima" || nameOf[bot] != "" {
		t.Errorf("members' names as people see them: %v", nameOf)
	}
}

// A person is matched in one order: the import's own map, then an earlier
// import's, then their address, then their name. Someone with an address is
// never matched by name, a bot never by name, and a shared name is no one's.
func TestAPersonIsMatchedInOneOrder(t *testing.T) {
	own, earlier, byMail, ana, bo1, bo2 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	members := &memberNames{byName: map[string][]uuid.UUID{"ana lima": {ana}, "bo ray": {bo1, bo2}}}
	f := &personFacts{
		own:       map[string]uuid.UUID{"u1": own},
		earlier:   map[string]uuid.UUID{"u1": earlier, "u2": earlier},
		byAddress: map[string]uuid.UUID{"cy@acme.test": byMail},
		members:   members,
	}
	cases := []struct {
		name string
		su   importProvider.SourceUser
		want personMatch
	}{
		{"this import's map first", importProvider.SourceUser{SourceID: "u1", Email: "cy@acme.test"}, personMatch{by: matchedByImport, id: own}},
		{"then an earlier import's", importProvider.SourceUser{SourceID: "u2", Email: "cy@acme.test"}, personMatch{by: matchedByWorkspace, id: earlier}},
		{"then the address, in any case", importProvider.SourceUser{SourceID: "u3", Email: " Cy@Acme.test "}, personMatch{by: matchedByEmail, id: byMail}},
		{"an address no one has is no one, whatever the name", importProvider.SourceUser{SourceID: "u4", Email: "ana@else.test", DisplayName: "Ana Lima"}, personMatch{}},
		{"then the one member with the name", importProvider.SourceUser{SourceID: "u5", DisplayName: "ANA  Lima"}, personMatch{by: matchedByName, id: ana}},
		{"a bot is never matched by name", importProvider.SourceUser{SourceID: "u6", DisplayName: "Ana Lima", IsBot: true}, personMatch{}},
		{"a shared name is no one's, and says how many share it", importProvider.SourceUser{SourceID: "u7", DisplayName: "Bo Ray"}, personMatch{sameName: 2}},
		{"a name no member has is no one", importProvider.SourceUser{SourceID: "u8", DisplayName: "Dee"}, personMatch{}},
	}
	for _, c := range cases {
		if got := f.match(&c.su); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
