package business

import (
	"reflect"
	"testing"
)

// An app's token carries only what its app may still carry (Validate): taking
// a scope from the app takes it from its tokens on their next call.
func TestAnAppsTokenCarriesOnlyWhatItsAppStillMay(t *testing.T) {
	cases := []struct {
		token, app, want []string
	}{
		{[]string{"tables:read", "tasks:read"}, []string{"tasks:read", "tables:read"}, []string{"tables:read", "tasks:read"}},
		{[]string{"tables:read", "tasks:read"}, []string{"tasks:read"}, []string{"tasks:read"}},
		{[]string{"tables:read"}, nil, []string{}},
	}
	for _, c := range cases {
		if got := within(c.token, c.app); !reflect.DeepEqual(got, c.want) {
			t.Errorf("a token with %v for an app with %v carries %v, not %v", c.token, c.app, got, c.want)
		}
	}
}
