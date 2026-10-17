package business

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Who hears of a new task by their setting is only who may read it now: a
// setting outlives someone's place in the project. With no answer of either,
// nobody hears of it by setting (whom it's given to still does).
func TestANewTaskIsHeardOfByItsReadersOnly(t *testing.T) {
	eligible0, readers0 := newTaskEligible, newTaskReaders
	t.Cleanup(func() { newTaskEligible, newTaskReaders = eligible0, readers0 })

	for _, c := range []struct {
		name                    string
		eligibleErr, readersErr error
		want                    []string
	}{
		{"its readers, in the order their settings were read", nil, nil, []string{"ben", "sam"}},
		{"who hears of it by setting can't be read", errors.New("postgres is down"), nil, nil},
		{"who may read it can't be read", nil, errors.New("dgraph is down"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			newTaskEligible = func(ctx context.Context, projectID, excludeUserID string, mentions []string, limitToMentions bool) ([]string, error) {
				if projectID != "launch" || excludeUserID != "maya" || !slices.Equal(mentions, []string{"sam"}) || limitToMentions {
					t.Errorf("settings asked of %q without %q for %v (mentions only %v), want launch without maya for [sam], everyone", projectID, excludeUserID, mentions, limitToMentions)
				}
				return []string{"ben", "lee", "sam"}, c.eligibleErr
			}
			newTaskReaders = func(ctx context.Context, taskUUID string, ids []string) (map[string]bool, error) {
				if taskUUID != "venue" || !slices.Equal(ids, []string{"ben", "lee", "sam"}) {
					t.Errorf("readers asked of %q for %v", taskUUID, ids)
				}
				return map[string]bool{"ben": true, "sam": true}, c.readersErr
			}
			if got := newTaskHearers(context.Background(), "launch", "venue", "maya", []string{"sam"}); !slices.Equal(got, c.want) {
				t.Errorf("heard of by %v, want %v", got, c.want)
			}
		})
	}
}
