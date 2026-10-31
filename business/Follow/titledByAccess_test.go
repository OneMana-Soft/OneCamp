package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// A kind the Access Resolver titles (an event, a project) is named, and
// opens, in the inbox only for a reader who may open the thing now, asked as
// the resolver names a reader, by graph uid and user id: a project's title
// reader decides by the graph uid. One they can't open, or a reader the
// resolver can't answer for, is left untitled.
func TestAnAccessTitledKindIsNamedOnlyForWhoMayOpenIt(t *testing.T) {
	const kind, open, gone, reader = "project", "5e1f0000-0000-4000-8000-0000000000e1", "5e1f0000-0000-4000-8000-0000000000e2", "00000000-0000-4000-8000-0000000000c3"
	oldTitles, oldUID := readableTitles, readerUID
	t.Cleanup(func() { readableTitles, readerUID = oldTitles, oldUID })
	readerUID = func(_ context.Context, who accessBusiness.Subject) (string, error) {
		if who.UserUUID != reader {
			t.Errorf("named %+v", who)
		}
		return "0xc3", nil
	}
	var asked []accessBusiness.ItemRef
	readableTitles = func(_ context.Context, readers []accessBusiness.Subject, refs []accessBusiness.ItemRef) (map[accessBusiness.ItemRef]string, error) {
		asked = refs
		if len(readers) != 1 || readers[0].UserUUID != reader || readers[0].UserUID != "0xc3" {
			t.Errorf("asked for %+v", readers)
		}
		return map[accessBusiness.ItemRef]string{{Kind: kind, ID: open}: "Launch"}, nil
	}
	names := TitledByAccess(kind)
	readable, titles, err := names(context.Background(), reader, []string{open, gone})
	if err != nil || len(asked) != 2 || asked[0].Kind != kind || asked[1].Kind != kind {
		t.Fatalf("asked %+v: %v", asked, err)
	}
	if !readable[open] || titles[open] != "Launch" {
		t.Errorf("the one they may open: %v %q", readable[open], titles[open])
	}
	if readable[gone] || titles[gone] != "" {
		t.Errorf("one they can't open is named: %v %q", readable[gone], titles[gone])
	}

	readableTitles = func(context.Context, []accessBusiness.Subject, []accessBusiness.ItemRef) (map[accessBusiness.ItemRef]string, error) {
		return nil, errors.New("down")
	}
	if readable, titles, err := names(context.Background(), reader, []string{open}); err == nil || len(readable)+len(titles) != 0 {
		t.Errorf("a resolver that failed named %v %v (%v)", readable, titles, err)
	}
	readerUID = func(context.Context, accessBusiness.Subject) (string, error) { return "", errors.New("down") }
	if readable, titles, err := names(context.Background(), reader, []string{open}); err == nil || len(readable)+len(titles) != 0 {
		t.Errorf("a reader that couldn't be named named %v %v (%v)", readable, titles, err)
	}
}
