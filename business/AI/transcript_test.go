package business

import (
	"context"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// GetRecentConversationTranscript must refuse (return "" without touching the
// search backend) for any surface the asker cannot see, for an unknown grouping
// field, and for empty input. These deny branches return before SearchRecent,
// so the test is DB-free and guards the permission boundary the DM-able agent
// context relies on (it reads the DM AS THE ASKER).
func TestGetRecentConversationTranscript_Denies(t *testing.T) {
	ctx := context.Background()
	userInfo := &userModels.UserInfo{
		UserDgraphInfo: dgraphStruct.DgraphUser{
			Uuid:     "user-1",
			Channels: []*dgraphStruct.DgraphChannel{{Uuid: "chan-A"}},
			DMs:      []*dgraphStruct.DgraphDm{{GroupingId: "grp-A"}},
		},
	}

	cases := []struct {
		name       string
		groupField string
		groupValue string
		userInfo   *userModels.UserInfo
	}{
		{"channel the asker is not in", "channel_uuid", "chan-OTHER", userInfo},
		{"dm the asker is not in", "chat_grp_id", "grp-OTHER", userInfo},
		{"unknown grouping field", "bogus_field", "chan-A", userInfo},
		{"empty value", "chat_grp_id", "", userInfo},
		{"nil userInfo", "chat_grp_id", "grp-A", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GetRecentConversationTranscript(ctx, tc.userInfo, tc.groupField, tc.groupValue, 10); got != "" {
				t.Fatalf("expected empty transcript (deny), got %q", got)
			}
		})
	}
}
