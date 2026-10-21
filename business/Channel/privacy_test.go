package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestWhoMayChangeAChannelsPrivacy(t *testing.T) {
	private, public := true, false
	priv := &dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &private}
	pub := &dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &public}
	unknown := &dgraphStruct.DgraphChannel{Uuid: "c"}
	for name, tc := range map[string]struct {
		ch      *dgraphStruct.DgraphChannel
		private bool
		want    PrivacyChange
	}{
		"private, kept private":  {priv, true, PrivacyUnchanged},
		"private, made public":   {priv, false, PrivacyOpened},
		"public, made private":   {pub, true, PrivacyClosed},
		"public, kept public":    {pub, false, PrivacyUnchanged},
		"unknown, made public":   {unknown, false, PrivacyOpened},
		"unknown, kept private":  {unknown, true, PrivacyUnchanged},
		"not loaded, set public": {nil, false, PrivacyOpened},
	} {
		if got := PrivacyChangeOf(tc.ch, tc.private); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		change PrivacyChange
		admin  bool
		may    bool
	}{
		{PrivacyOpened, false, false}, {PrivacyOpened, true, true},
		{PrivacyClosed, false, true}, {PrivacyClosed, true, true},
		{PrivacyUnchanged, false, true},
	} {
		if got := MayChangePrivacy(tc.change, tc.admin); got != tc.may {
			t.Errorf("change %v by an admin=%v: may = %v, want %v", tc.change, tc.admin, got, tc.may)
		}
	}
}
