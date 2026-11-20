package Calendar

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// A calendar shared at a level is decided by its share alone: whose busy
// times the reader sees is not asked for it (H6 A1-11), so choices that
// can't be read no longer fail it; a calendar no share decides still asks,
// and still fails closed when the choices can't be read.
func TestAShareDecidesWithoutAskingWhoIsBusy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	mayas, _ := h.store.personalCalendar(ctx, personOf(maya), "UTC")
	prevPolicy, prevOwn := freeBusyPolicy, freeBusyShownBy
	t.Cleanup(func() { freeBusyPolicy, freeBusyShownBy = prevPolicy, prevOwn })
	asked := 0
	freeBusyPolicy = func(context.Context) bool { return true }
	freeBusyShownBy = func(context.Context, []string) (map[string]bool, error) {
		asked++
		return nil, errors.New("the choices could not be read")
	}

	if err := h.store.setShare(ctx, mayas, personOf(jonas), personOf(maya), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.decideCalendars(ctx, subjectOf(jonas), []string{mayas.String()})
	if err != nil {
		t.Fatalf("shared at details: %v", err)
	}
	if d := got[mayas.String()]; d.Level != accessBusiness.LevelView || len(d.Reasons) != 1 || d.Reasons[0] != reasonShared {
		t.Errorf("shared at details, Jonas has %+v", d)
	}
	if asked != 0 {
		t.Errorf("whose busy times Jonas sees was asked %d times for a calendar its share decides", asked)
	}

	if err := h.store.setShare(ctx, mayas, personOf(jonas), personOf(maya), ""); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.decideCalendars(ctx, subjectOf(jonas), []string{mayas.String()}); err == nil {
		t.Errorf("unshared, with the choices unreadable, Jonas has %+v and no error", got[mayas.String()])
	}
	if asked != 1 {
		t.Errorf("unshared: whose busy times Jonas sees was asked %d times; want 1", asked)
	}
}

// Whatever its owner chose about their busy times, a calendar shared at a
// level is decided the same (H6 A1-11: busySeen is no longer read for it,
// and it never decided it): the same level and reason, so the same events
// and the same fields (hiddenFrom reads the decision alone); unshared, the
// owner's choice decides again.
func TestAShareLevelCalendarIsDecidedAsBeforeWhateverItsOwnerShows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	mayas, _ := h.store.personalCalendar(ctx, personOf(maya), "UTC")
	decide := func() accessBusiness.Decision {
		t.Helper()
		got, err := h.s.decideCalendars(ctx, subjectOf(jonas), []string{mayas.String()})
		if err != nil {
			t.Fatal(err)
		}
		return got[mayas.String()]
	}
	for _, shows := range []bool{true, false} {
		freeBusyChoices(t, true, map[string]bool{personOf(maya).String(): shows})
		for _, share := range []struct {
			level string
			want  accessBusiness.Level
		}{{models.ShareDetails, accessBusiness.LevelView}, {models.ShareEdit, accessBusiness.LevelEdit}, {models.ShareManage, accessBusiness.LevelFull}} {
			if err := h.store.setShare(ctx, mayas, personOf(jonas), personOf(maya), share.level); err != nil {
				t.Fatal(err)
			}
			if d := decide(); d.Level != share.want || busyOnly(d) || len(d.Reasons) != 1 || d.Reasons[0] != reasonShared {
				t.Errorf("Maya shows when busy %v, shared at %s: Jonas has %+v", shows, share.level, d)
			}
		}
		if err := h.store.setShare(ctx, mayas, personOf(jonas), personOf(maya), ""); err != nil {
			t.Fatal(err)
		}
		d := decide()
		if shows && (!busyOnly(d) || d.Level != accessBusiness.LevelView) || !shows && d.Level != accessBusiness.LevelNone {
			t.Errorf("Maya shows when busy %v, unshared: Jonas has %+v", shows, d)
		}
	}
}
