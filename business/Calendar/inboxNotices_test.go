package Calendar

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// A calendar notice is named, and opens, in the inbox only for a reader who
// may open its event now (business/Follow's per-kind seam): one they can't
// open, or a reader the resolver can't answer for, is left untitled.
func TestTheInboxNamesAnEventOnlyForWhoMayOpenIt(t *testing.T) {
	const open, gone, reader = "5e1f0000-0000-4000-8000-0000000000e1", "5e1f0000-0000-4000-8000-0000000000e2", "00000000-0000-4000-8000-0000000000c3"
	old := readableTitles
	t.Cleanup(func() { readableTitles = old })
	var asked []accessBusiness.ItemRef
	readableTitles = func(_ context.Context, readers []accessBusiness.Subject, refs []accessBusiness.ItemRef) (map[accessBusiness.ItemRef]string, error) {
		asked = refs
		if len(readers) != 1 || readers[0].UserUUID != reader {
			t.Errorf("asked for %+v", readers)
		}
		return map[accessBusiness.ItemRef]string{{Kind: KindEvent, ID: open}: "Design review"}, nil
	}
	readable, titles, err := eventNotices(context.Background(), reader, []string{open, gone})
	if err != nil || len(asked) != 2 || asked[0].Kind != KindEvent {
		t.Fatalf("asked %+v: %v", asked, err)
	}
	if !readable[open] || titles[open] != "Design review" {
		t.Errorf("the event they may open: %v %q", readable[open], titles[open])
	}
	if readable[gone] || titles[gone] != "" {
		t.Errorf("an event they can't open is named: %v %q", readable[gone], titles[gone])
	}
	readableTitles = func(context.Context, []accessBusiness.Subject, []accessBusiness.ItemRef) (map[accessBusiness.ItemRef]string, error) {
		return nil, errors.New("down")
	}
	if readable, titles, err := eventNotices(context.Background(), reader, []string{open}); err == nil || len(readable)+len(titles) != 0 {
		t.Errorf("a resolver that failed named %v %v (%v)", readable, titles, err)
	}
}
