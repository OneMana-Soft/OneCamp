package businness

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/business/Access/level"
)

// A hosted doc's decision is its host's, never above edit: what only full
// access does to a doc (share, move, lock, trash) is its host's to do. It is
// owned by nobody, and it is in the trash when its host is, with the level
// the person had before.
func TestAHostedDocIsDecidedByItsHostCappedAtEdit(t *testing.T) {
	hosted := DocDecision{Hosted: true}
	for _, c := range []struct {
		name string
		host HostDecision
		want DocDecision
	}{
		{"the host's owner", HostDecision{Level: level.Full, Was: level.Full},
			DocDecision{Hosted: true, Level: level.Edit, Unlocked: level.Edit, Was: level.Edit, Reasons: []level.Reason{level.Host}}},
		{"an editor", HostDecision{Level: level.Edit, Was: level.Edit},
			DocDecision{Hosted: true, Level: level.Edit, Unlocked: level.Edit, Was: level.Edit, Reasons: []level.Reason{level.Host}}},
		{"a reader", HostDecision{Level: level.View, Was: level.View},
			DocDecision{Hosted: true, Level: level.View, Unlocked: level.View, Was: level.View, Reasons: []level.Reason{level.Host}}},
		{"someone the host isn't open to", HostDecision{},
			DocDecision{Hosted: true}},
		{"its host in the trash", HostDecision{Deleted: true, Was: level.Full},
			DocDecision{Hosted: true, Deleted: true, Was: level.Edit}},
	} {
		got := hostedDecision(hosted, c.host)
		if got.Level != c.want.Level || got.Unlocked != c.want.Unlocked || got.Was != c.want.Was || got.Deleted != c.want.Deleted ||
			got.Owner || !got.Hosted || len(got.Reasons) != len(c.want.Reasons) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	// The owner a doc would have is nobody's on a hosted one.
	if got := hostedDecision(DocDecision{Hosted: true, Owner: true}, HostDecision{Level: level.Full}); got.Owner {
		t.Error("a hosted doc has an owner")
	}
	// A deletion of its own holds whatever its host says.
	if got := hostedDecision(DocDecision{Hosted: true, Deleted: true}, HostDecision{Level: level.Edit, Was: level.Edit}); got.Level != level.None || !got.Deleted {
		t.Errorf("a hosted doc deleted itself: %+v", got)
	}
}

// The host is asked only on a context a handler marked as opening hosted
// docs: on any other, a hosted doc keeps the rule's none and its host is never
// asked. A host that isn't there leaves it at none; a host that can't be
// decided is an error, never a no.
func TestHostsAreAskedOnlyWhereAHandlerOpensThem(t *testing.T) {
	defer func(r HostRule) { hostRule = r }(hostRule)
	row := Host{Kind: "table_row", UUID: "8e2b4d6f-1a3c-4e5f-8b7a-9c0d1e2f3a4b"}
	asked := 0
	hostRule = func(_ context.Context, _ Viewer, hosts []Host) (map[Host]HostDecision, error) {
		asked++
		if len(hosts) != 1 || hosts[0] != row {
			t.Errorf("asked for %v", hosts)
		}
		return map[Host]HostDecision{row: {Level: level.Edit, Was: level.Edit}}, nil
	}
	decided := func() map[string]DocDecision {
		return map[string]DocDecision{"a": {Hosted: true}, "b": {Hosted: true}, "plain": {Level: level.View}}
	}
	hosts := map[string]Host{"a": row, "b": row}

	out, err := withHosts(context.Background(), Viewer{UUID: "u"}, decided(), hosts)
	if err != nil || out["a"].Level != level.None || asked != 0 {
		t.Fatalf("a context nobody opened: %+v, %v, the host asked %d times", out, err, asked)
	}
	out, err = withHosts(WithHostedOpen(context.Background()), Viewer{UUID: "u"}, decided(), hosts)
	if err != nil || out["a"].Level != level.Edit || out["b"].Level != level.Edit || out["plain"].Level != level.View || asked != 1 {
		t.Fatalf("an opened context: %+v, %v, the host asked %d times (want once for both)", out, err, asked)
	}

	hostRule = func(context.Context, Viewer, []Host) (map[Host]HostDecision, error) {
		return map[Host]HostDecision{}, nil
	}
	if out, _ := withHosts(WithHostedOpen(context.Background()), Viewer{UUID: "u"}, decided(), hosts); out["a"].Level != level.None {
		t.Errorf("a host that isn't there: %+v", out["a"])
	}
	failed := errors.New("the tables didn't answer")
	hostRule = func(context.Context, Viewer, []Host) (map[Host]HostDecision, error) { return nil, failed }
	if _, err := withHosts(WithHostedOpen(context.Background()), Viewer{UUID: "u"}, decided(), hosts); !errors.Is(err, failed) {
		t.Errorf("a host that couldn't be decided: %v", err)
	}
}

// Only a registered kind hosts docs, once a kind: a second registration, or
// one with no name or no place, is two packages' mistake and panics.
func TestHostKindsAreRegisteredOnce(t *testing.T) {
	defer func() {
		hostKindsMu.Lock()
		delete(hostKinds, "probe_host")
		hostKindsMu.Unlock()
	}()
	place := func(context.Context, string) (HostPlace, error) { return HostPlace{}, nil }
	RegisterHostKind("probe_host", HostKind{Place: place})
	for name, register := range map[string]func(){
		"twice":    func() { RegisterHostKind("probe_host", HostKind{Place: place}) },
		"no name":  func() { RegisterHostKind("", HostKind{Place: place}) },
		"no place": func() { RegisterHostKind("other_host", HostKind{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering a host kind %s didn't panic", name)
				}
			}()
			register()
		}()
	}
	if _, _, err := EnsureHostedDoc(context.Background(), Host{Kind: "nobody_registered", UUID: "8e2b4d6f-1a3c-4e5f-8b7a-9c0d1e2f3a4b"}, "x"); !errors.Is(err, ErrNoHost) {
		t.Errorf("a kind nobody registered made a doc: %v", err)
	}
	if _, _, err := EnsureHostedDoc(context.Background(), Host{Kind: "probe_host", UUID: "row-1"}, "x"); !errors.Is(err, ErrNoHost) {
		t.Errorf("a host id that isn't one made a doc: %v", err)
	}
	if _, err := HostPlaceOf(context.Background(), Host{Kind: "nobody_registered"}); !errors.Is(err, ErrNoHost) {
		t.Errorf("a kind nobody registered was placed: %v", err)
	}
}
