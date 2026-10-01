package helpers

import "testing"

// The blast radius is asymmetric: divert when we should not and the server never
// starts; fail to divert and a full server boots beside the instance being
// checked. The container's CMD passes no arguments, so the no-argument case is
// the one production actually depends on.
func TestOnlyAnExactSubcommandInFirstPositionDiverts(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"container CMD, no arguments", []string{"go-one-camp"}, false},
		{"the subcommand", []string{"go-one-camp", "journey"}, true},
		{"the subcommand with its own arguments", []string{"go-one-camp", "journey", "-v"}, true},
		{"an unrelated flag", []string{"go-one-camp", "-port=3000"}, false},
		{"a word that merely contains it", []string{"go-one-camp", "journeys"}, false},
		{"a prefix of it", []string{"go-one-camp", "jour"}, false},
		{"the right word in the wrong position", []string{"go-one-camp", "serve", "journey"}, false},
		{"nothing at all", nil, false},
	}

	for _, tc := range cases {
		if got := IsClientSubcommand(tc.args, "journey"); got != tc.want {
			t.Errorf("%s: got %v, want %v (args %v)", tc.name, got, tc.want, tc.args)
		}
	}
}

// An empty name would otherwise match any process invoked with one argument.
func TestAnEmptySubcommandNameNeverMatches(t *testing.T) {
	if IsClientSubcommand([]string{"go-one-camp", ""}, "") {
		t.Error("an empty subcommand name must never divert the server")
	}
}

func TestOneShotSubcommandsRunNoWorkers(t *testing.T) {
	if r := RoleForProcess(RoleAll, []string{"/app/go-one-camp", "demoseed", "visitor@example.test", "--refresh"}); r.RunsWorkers() {
		t.Fatal("demoseed must not start the background loops beside the live server")
	}
	if r := RoleForProcess(RoleWorker, []string{"/app/go-one-camp", "demoseed"}); r.RunsWorkers() {
		t.Fatal("not even on a worker replica")
	}
	for _, args := range [][]string{{"/app/go-one-camp"}, {"/app/go-one-camp", "--port", "demoseed"}} {
		if r := RoleForProcess(RoleAll, args); r != RoleAll {
			t.Fatalf("the server itself keeps its role: %v -> %s", args, r)
		}
	}
}
