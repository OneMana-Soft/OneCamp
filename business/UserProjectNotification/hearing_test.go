package business

import (
	"context"
	"slices"
	"testing"
)

// One project's settings: Ada muted it, Ben hears what names him or a thread
// he is in, Cy hears everything; Eve has no setting (not in the project).
func withProjectLevels(t *testing.T, asked *[][]string) {
	t.Helper()
	was := projectLevels
	t.Cleanup(func() { projectLevels = was })
	levels := map[string]string{"ada": "block", "ben": "mention", "cy": "all", "maya": "all"}
	projectLevels = func(_ context.Context, _ string, ids []string) (map[string]string, error) {
		*asked = append(*asked, ids)
		if ids == nil {
			return levels, nil
		}
		out := map[string]string{}
		for _, id := range ids {
			if l, ok := levels[id]; ok {
				out[id] = l
			}
		}
		return out, nil
	}
}

func TestProjectActivityHearsByTheOneRule(t *testing.T) {
	var asked [][]string
	withProjectLevels(t, &asked)
	for _, c := range []struct {
		what      string
		mentioned []string
		only      bool
		want      []string
	}{
		{"something new in the project, naming Ada and Ben", []string{"Ada", "ben"}, false, []string{"ben", "cy"}},
		// Eve has no setting for the project: she hears, as only a mute removes.
		{"only whom it names", []string{"ada", "ben", "cy", "eve"}, true, []string{"ben", "cy", "eve"}},
		{"naming nobody, only whom it names", nil, true, nil},
		// Something new given to Eve, who has no setting for the project (the
		// task's assignee, in its project or not, or a member whose row was
		// never written): she is named, so she hears, as the email tells her.
		{"something new in the project, given to Eve, who has no setting", []string{"eve"}, false, []string{"cy", "eve"}},
	} {
		got, err := GetEligibleUsersForProjectActivity(context.Background(), "spring", "maya", c.mentioned, c.only)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v", c.what, got, err, c.want)
		}
	}
	if len(asked) != 3 || asked[0] != nil || asked[2] != nil {
		t.Errorf("settings read %v, want one read per event (the whole project, the mentioned, the whole project)", asked)
	}
}

func TestTaskCommentAudienceHearsByTheOneRule(t *testing.T) {
	var asked [][]string
	withProjectLevels(t, &asked)
	// Ada (muted) and Ben (Mentions) are in the thread, Cy and Eve (no setting)
	// are named, Maya wrote it.
	got, err := TaskCommentAudience(context.Background(), "spring", "maya", []string{"ada", "ben", "maya"}, []string{"cy", "eve"})
	if want := []string{"ben", "cy", "eve"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("TaskCommentAudience = %v, %v; want %v (Mentions hears a thread they are in, Nothing hears nothing, no setting hears)", got, err, want)
	}
	if len(asked) != 1 {
		t.Fatalf("settings read %d times, want once", len(asked))
	}
	if got, _ := TaskCommentAudience(context.Background(), "spring", "maya", nil, nil); got != nil || len(asked) != 1 {
		t.Fatalf("an empty audience read the settings or told %v", got)
	}
}
