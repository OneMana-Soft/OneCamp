package business

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A doc run archives in batches. When a later batch fails, the docs the
// earlier batches archived are archived all the same, and the run answers
// them with its error. Search must follow them, or they, their comments and
// their files stay findable while archived: the run skipped search whenever
// it failed. The run is still recorded as failed, with the docs it moved, and
// a run that moved nothing touches search not at all.
func TestARunThatFailsPartWayTakesWhatItArchivedOutOfSearch(t *testing.T) {
	const kind = "test-docs-in-batches"
	type ended struct {
		ids []string
		err error
	}
	for name, c := range map[string]struct {
		ids  []string
		err  error
		want []string
	}{
		"a later batch failed": {ids: []string{"doc-1", "doc-2"}, err: errors.New("the second batch failed"), want: []string{"doc-1", "doc-2"}},
		"every batch went":     {ids: []string{"doc-1"}, want: []string{"doc-1"}},
		"the first one failed": {err: errors.New("the first batch failed")},
	} {
		var searched []string
		entityRegistry[kind] = entityOps{
			ArchiveFn: func(context.Context, time.Time, *ArchivePolicy) (int64, int64, []string, error) {
				return int64(len(c.ids)), 3, c.ids, c.err
			},
			OpenSearchArchiveFn: func(ids []string) { searched = append(searched, ids...) },
		}
		var got []ended
		prev := finishJob
		finishJob = func(_ context.Context, _ uuid.UUID, _ string, _, _ int64, ids []string, err error) {
			got = append(got, ended{ids, err})
		}
		executeArchiveJob(uuid.New(), kind, &ArchivePolicy{RetentionDays: 365})
		finishJob = prev
		delete(entityRegistry, kind)

		if len(searched) != len(c.want) || (len(c.want) > 0 && (searched[0] != c.want[0] || searched[len(searched)-1] != c.want[len(c.want)-1])) {
			t.Errorf("%s: search was told of %v, want %v", name, searched, c.want)
		}
		if len(got) != 1 || len(got[0].ids) != len(c.ids) || (got[0].err == nil) != (c.err == nil) {
			t.Errorf("%s: the run was recorded as %+v, want its ids %v and error %v", name, got, c.ids, c.err)
		}
	}
}
