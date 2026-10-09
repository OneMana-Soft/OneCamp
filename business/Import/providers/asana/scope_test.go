package asana

import (
	"strings"
	"testing"
)

func TestTheAsanaWorkspaceIsPickedNeverGuessed(t *testing.T) {
	acme := asanaWorkspace{GID: "1", Name: "Acme"}
	personal := asanaWorkspace{GID: "2", Name: "Personal Projects"}
	twin := asanaWorkspace{GID: "3", Name: "acme"}

	for _, tc := range []struct {
		name       string
		workspaces []asanaWorkspace
		picked     string
		label      string
		want       string
		err        string
	}{
		{"the pick", []asanaWorkspace{acme, personal}, "2", "Acme", "2", ""},
		{"a pick it can't see", []asanaWorkspace{acme, personal}, "9", "Acme", "", "isn't visible to this token"},
		{"no pick: the label's workspace", []asanaWorkspace{acme, personal}, "", " ACME ", "1", ""},
		{"no pick, the only workspace", []asanaWorkspace{personal}, "", "Acme", "2", ""},
		// It used to import the first workspace here.
		{"no pick, no name, several", []asanaWorkspace{acme, personal}, "", "Acme Inc.", "", "pick which Asana workspace to import: this token can see 2 (Acme, Personal Projects)"},
		{"two called the label", []asanaWorkspace{acme, twin}, "", "Acme", "", `more than one Asana workspace is called "Acme"`},
		{"none at all", nil, "", "Acme", "", "can't see any workspace"},
	} {
		got, err := pickWorkspace(tc.workspaces, tc.picked, tc.label)
		if got != tc.want || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
}
