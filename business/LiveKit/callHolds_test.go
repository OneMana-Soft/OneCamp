package business

import (
	"slices"
	"testing"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// What the people running a call hold for everyone else is left out of what
// someone may publish (calls meet-12, zoom-14): never for whoever runs the
// call, and not for someone let through; chat and reactions aren't media.
func TestAHoldTakesItsSourcesFromWhoeverDoesntRunTheCall(t *testing.T) {
	all := []string{"camera", "microphone", "screen_share", "screen_share_audio"}
	mics := []string{callStateModel.HoldMicrophone}
	cases := []struct {
		name    string
		held    []string
		allowed []string
		runs    bool
		want    []string
	}{
		{"nothing held", nil, nil, false, all},
		{"microphones held", mics, nil, false, []string{"camera", "screen_share", "screen_share_audio"}},
		{"sharing held takes its sound too", []string{callStateModel.HoldShare}, nil, false, []string{"camera", "microphone"}},
		{"every medium held", []string{callStateModel.HoldShare, callStateModel.HoldMicrophone, callStateModel.HoldCamera}, nil, false, []string{}},
		{"the people running the call", []string{callStateModel.HoldShare, callStateModel.HoldMicrophone, callStateModel.HoldCamera}, nil, true, all},
		{"let through one hold", []string{callStateModel.HoldMicrophone, callStateModel.HoldCamera}, mics, false, []string{"microphone", "screen_share", "screen_share_audio"}},
		{"chat and reactions aren't media", []string{callStateModel.HoldChat, callStateModel.HoldReact}, nil, false, all},
	}
	for _, c := range cases {
		if got := PublishSources(c.held, c.allowed, c.runs); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
