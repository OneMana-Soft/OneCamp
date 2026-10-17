package dgraphquery

import "testing"

func TestAUUIDListTakesUUIDsOnlyEachOnce(t *testing.T) {
	got := UUIDList([]string{
		"0D0C0000-0000-4000-8000-000000000001", "0d0c0000-0000-4000-8000-000000000001",
		`x"] } evil(func: has(user_uuid)) { uid } q(func: eq(a, ["`, "", "9e0c0000-0000-4000-8000-000000000002",
	})
	if want := `"0d0c0000-0000-4000-8000-000000000001", "9e0c0000-0000-4000-8000-000000000002"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got := UUIDList([]string{"nope"}); got != "" {
		t.Fatalf("no uuids: %q", got)
	}
}
