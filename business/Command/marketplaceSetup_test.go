package business

import (
	"testing"

	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// Twenty of the directory's apps send their commands to an address of their
// own, and declared only a key or a sign-in: once those were pasted they
// showed "Installed", their commands entered every member's "/" menu, and
// each answered "has no handler configured" (SA-BUG-8). An app answered at
// an address counts as set up only once it has one.
func TestAnAppAnsweredAtAnAddressNeedsTheAddress(t *testing.T) {
	external := 0
	for _, tpl := range marketplaceTemplates {
		fields := setupFor(tpl)
		asks := false
		for _, f := range fields {
			if f.Type == setupHandler && f.Required {
				asks = true
			}
		}
		if dispatchesExternally(tpl) {
			external++
			if !asks {
				t.Errorf("%s sends its commands to an address but setup never asks for it", tpl.Slug)
			}
		} else if asks {
			t.Errorf("%s answers its commands itself but setup asks for an address", tpl.Slug)
		}
	}
	if external < 15 {
		t.Fatalf("only %d directory apps send their commands to an address; the directory changed under this test", external)
	}
	// The templates themselves are kept as they were.
	if zoom, _ := getTemplate("zoom"); len(zoom.Setup) != 1 {
		t.Errorf("the Zoom template's own setup changed: %+v", zoom.Setup)
	}
}

func TestSetUpMeansAWorkingAddress(t *testing.T) {
	zoom, _ := getTemplate("zoom")
	connected := &appSecretBag{OAuthClientSecret: "s3cret"}
	addr := func(s string) *string { return &s }
	cases := []struct {
		name    string
		handler *string
		want    bool
	}{
		{"signed in, no address", nil, true},
		{"signed in, an empty address", addr(""), true},
		{"signed in, an address that isn't https", addr("http://hooks.example.test/zoom"), true},
		{"signed in, not an address at all", addr("zoom"), true},
		{"signed in, with an https address", addr("https://hooks.example.test/zoom"), false},
	}
	for _, c := range cases {
		app := &slashModel.App{Kind: slashModel.AppKindOAuth, HandlerUrl: c.handler}
		if got := templateNeedsSetup(zoom, app, connected); got != c.want {
			t.Errorf("%s: needs setup %v, want %v", c.name, got, c.want)
		}
	}
	// Not signed in still needs setup, whatever the address.
	if !templateNeedsSetup(zoom, &slashModel.App{HandlerUrl: addr("https://hooks.example.test/zoom")}, &appSecretBag{}) {
		t.Error("an app with an address but no sign-in counts as set up")
	}
	// A built-in answers itself: its key is all it needs.
	giphy, _ := getTemplate("giphy")
	if templateNeedsSetup(giphy, &slashModel.App{Kind: slashModel.AppKindBuiltin}, &appSecretBag{Secrets: map[string]string{"api_key": "k"}}) {
		t.Error("Giphy with its key still needs setup")
	}
}
