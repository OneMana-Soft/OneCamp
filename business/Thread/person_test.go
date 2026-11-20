package business

import (
	"encoding/json"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// V-1023: a bot made before bots took their name as user_name has its login
// handle there ("onecamp-ai"); the name it goes by is its user_full_name. A
// thread's author carries whether it is a bot and, for a bot, that name, so
// the web names it as every other surface does (lib/utils/userDisplayName).
// A person is named by the one rule alone: no full name on the wire.
func TestAThreadAuthorSaysWhetherItIsABotAndABotsName(t *testing.T) {
	bot, _ := json.Marshal(personOf(&dgraphStruct.DgraphUser{Uuid: "u-ai", UserName: "onecamp-ai", UserFullName: "OneCamp AI", IsBot: true}))
	if got, want := string(bot), `{"user_uuid":"u-ai","user_name":"onecamp-ai","user_full_name":"OneCamp AI","is_bot":true}`; got != want {
		t.Fatalf("an older bot's author:\n got %s\nwant %s", got, want)
	}
	person, _ := json.Marshal(personOf(&dgraphStruct.DgraphUser{Uuid: "u-sam", UserName: "sam", UserFullName: "Sam Rivera"}))
	if got, want := string(person), `{"user_uuid":"u-sam","user_name":"sam"}`; got != want {
		t.Fatalf("a person's author:\n got %s\nwant %s", got, want)
	}
}
