package business

import (
	"context"
	"testing"

	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

// A stranger's task is timed by the door it came through: a public form's as
// "form", the mail door's as "mail"; anyone else's by the feed's source.
func TestATimerNamesTheDoorAStrangersTaskCameThrough(t *testing.T) {
	old := typesOf
	t.Cleanup(func() { typesOf = old })
	typesOf = func(context.Context, []uuid.UUID) (map[uuid.UUID]taskKeyModel.Facts, error) { return nil, nil }
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{
		{context.Background(), "app"},
		{helpers.WithPublicSubmission(context.Background()), "form"},
		{helpers.WithSubmissionDoor(context.Background(), "mail"), "mail"},
	} {
		got, err := timerTask(c.ctx, uuid.New(), uuid.New(), &dgraphStruct.DgraphTask{}, workFeed.Entry{Source: "app"})
		if err != nil || got.Facts.Source != c.want {
			t.Errorf("source %q (err %v), want %q", got.Facts.Source, err, c.want)
		}
	}
}
