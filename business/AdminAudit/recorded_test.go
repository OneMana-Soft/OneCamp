package business

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// recordedLog fakes Recorded's two writers: what each was handed, in order,
// with the steps the change took between them.
type recordedLog struct {
	steps     []string
	refuse    error // the written row's error
	failedRow map[string]interface{}
}

func (l *recordedLog) install(t *testing.T) {
	t.Helper()
	old := recordedSeams
	t.Cleanup(func() { recordedSeams = old })
	recordedSeams.written = func(_ *http.Request, action, category, _ string, _ map[string]interface{}) error {
		l.steps = append(l.steps, "written "+action+" "+category)
		return l.refuse
	}
	recordedSeams.record = func(_ *http.Request, action, category, _ string, meta map[string]interface{}) {
		l.steps = append(l.steps, "record "+action+" "+category)
		l.failedRow = meta
	}
}

func TestRecordedWritesTheRowBeforeTheChange(t *testing.T) {
	l := &recordedLog{}
	l.install(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	err := Recorded(r, "team.member.remove", CategorySecurity, "Took someone off a team", map[string]interface{}{"team_uuid": "t"}, func() error {
		l.steps = append(l.steps, "change")
		return nil
	})
	if err != nil {
		t.Fatalf("a recorded change that took: %v", err)
	}
	want := []string{"written team.member.remove security", "change"}
	if !reflect.DeepEqual(l.steps, want) {
		t.Fatalf("steps %q, want %q: the row is written before the change, and nothing else", l.steps, want)
	}
}

func TestRecordedRefusesAChangeItCantRecord(t *testing.T) {
	l := &recordedLog{refuse: errors.New("the database is gone")}
	l.install(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	err := Recorded(r, "team.admin.remove", CategorySecurity, "Took someone's team admin role away", nil, func() error {
		l.steps = append(l.steps, "change")
		return nil
	})
	if !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("err %v, want ErrNotRecorded", err)
	}
	if want := []string{"written team.admin.remove security"}; !reflect.DeepEqual(l.steps, want) {
		t.Fatalf("steps %q, want %q: no record, no action", l.steps, want)
	}
}

func TestRecordedSaysWhenARecordedChangeDidntTake(t *testing.T) {
	l := &recordedLog{}
	l.install(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	failed := errors.New("the graph refused the write")
	meta := map[string]interface{}{"team_uuid": "t", "user_uuid": "u"}
	err := Recorded(r, "team.member.add", CategorySecurity, "Added someone to a team", meta, func() error {
		l.steps = append(l.steps, "change")
		return failed
	})
	if !errors.Is(err, failed) || errors.Is(err, ErrNotRecorded) {
		t.Fatalf("err %v, want the change's own error", err)
	}
	want := []string{"written team.member.add security", "change", "record team.member.add_failed security"}
	if !reflect.DeepEqual(l.steps, want) {
		t.Fatalf("steps %q, want %q", l.steps, want)
	}
	if !reflect.DeepEqual(l.failedRow, meta) {
		t.Fatalf("the _failed row's metadata %v, want the change's own %v", l.failedRow, meta)
	}
}
