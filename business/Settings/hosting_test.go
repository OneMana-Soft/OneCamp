package business

import "testing"

// Where the workspace runs, demo first: the public demo is run by OneMana, but
// what a visitor must be told is that it's a shared demo.
func TestHostingAnswersDemoCloudOrSelf(t *testing.T) {
	t.Cleanup(forget)
	for _, c := range []struct {
		name, demo, managed, want string
	}{
		{"the public demo", "true", "", HostingDemo},
		{"the demo even when Cloud runs it", "true", "true", HostingDemo},
		{"OneCamp Cloud", "", "true", HostingCloud},
		{"a server of the customer's own", "", "false", HostingSelf},
		{"nothing said, nothing marked", "", "", HostingSelf},
	} {
		forget()
		t.Setenv("DEMO_MODE", c.demo)
		t.Setenv("ONECAMP_MANAGED", c.managed)
		if got := Hosting(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
