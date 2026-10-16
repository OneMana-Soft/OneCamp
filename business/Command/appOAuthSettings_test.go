package business

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// The admin form edits an OAuth app's saved settings, so it is shown them,
// read back the way they were stored, and never the client secret: that is kept
// encrypted elsewhere, and only whether one is stored is said.
func TestAnAppsSavedOAuthSettingsAreShownWithoutItsSecret(t *testing.T) {
	stored, err := json.Marshal(publicOAuthConfig(&commandAdapter.AppOAuthConfig{
		ClientID: "client-1", ClientSecret: "s3cret", AuthURL: "https://provider.example/authorize",
		TokenURL: "https://provider.example/token", Scopes: []string{"read", "write"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "s3cret") || strings.Contains(string(stored), "client_secret") {
		t.Fatalf("the stored settings must not hold the secret: %s", stored)
	}
	raw := string(stored)
	got := storedOAuthSettings(&slashModel.App{OAuthConfig: &raw})
	want := commandAdapter.AppOAuthSettings{ClientID: "client-1", AuthURL: "https://provider.example/authorize",
		TokenURL: "https://provider.example/token", Scopes: []string{"read", "write"}}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("read back %+v, want %+v", got, want)
	}

	empty, garbage := "", "{not json"
	for _, c := range []*string{nil, &empty, &garbage} {
		if s := storedOAuthSettings(&slashModel.App{OAuthConfig: c}); s != nil {
			t.Errorf("no readable settings must read as none; got %+v", s)
		}
	}
}
