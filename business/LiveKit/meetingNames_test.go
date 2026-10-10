package business

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"

	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Guests join an instant meeting (a meet- room) from its link, from outside
// the workspace. A member is named there, on their tile and as whoever
// started the recording, by the one name rule without its last step, so
// never by part of their address; with no name at all they are "Someone".
// Every other call keeps the whole rule.
func TestAMeetingNeverNamesAMemberByTheirAddress(t *testing.T) {
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	var mu sync.Mutex
	var startedBy []string
	// LiveKit's room and egress services, as far as these calls need: every
	// room asked about is there, and an empty answer is a valid empty reply.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/protobuf")
		switch path.Base(r.URL.Path) {
		case "ListRooms":
			var req livekit.ListRoomsRequest
			_ = proto.Unmarshal(body, &req)
			resp := &livekit.ListRoomsResponse{}
			for _, name := range req.Names {
				resp.Rooms = append(resp.Rooms, &livekit.Room{Name: name})
			}
			out, _ := proto.Marshal(resp)
			_, _ = w.Write(out)
		case "UpdateRoomMetadata":
			var req livekit.UpdateRoomMetadataRequest
			_ = proto.Unmarshal(body, &req)
			var meta struct {
				StartedBy string `json:"recordingStartedBy"`
			}
			_ = json.Unmarshal([]byte(req.Metadata), &meta)
			mu.Lock()
			startedBy = append(startedBy, meta.StartedBy)
			mu.Unlock()
		}
	}))
	defer server.Close()
	saved := livekitInit.LiveKitService
	defer func() { livekitInit.LiveKitService = saved }()
	livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{
		LiveKitClient: lksdk.NewRoomServiceClient(server.URL, key, secret),
		EgressClient:  lksdk.NewEgressClient(server.URL, key, secret),
		Config:        &livekitInit.LiveKitConfigStruct{HostURL: server.URL, ApiKey: key, ApiSecret: secret},
	}

	ctx := context.Background()
	named := &dgraphStruct.DgraphUser{Uid: "0x1", UserFullName: "Mia Kapoor", EmailID: "mia.k@example.test"}
	nameless := &dgraphStruct.DgraphUser{Uid: "0x2", EmailID: "mia.k@example.test"}
	for _, c := range []struct {
		room string
		user *dgraphStruct.DgraphUser
		want string
	}{
		{"meet-6f1c", nameless, "Someone"},
		{"meet-6f1c", named, "Mia Kapoor"},
		{"6f1c0e2a-channel", nameless, "mia.k"}, // a member's own call: the whole rule
	} {
		token, _, err := CreateRoomAndGetToken(ctx, c.room, c.user, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := lka.ParseAPIToken(token)
		if err != nil {
			t.Fatal(err)
		}
		grants, err := verifier.Verify(secret)
		if err != nil {
			t.Fatal(err)
		}
		if grants.Name != c.want {
			t.Errorf("%s: the tile says %q, want %q", c.room, grants.Name, c.want)
		}

		mu.Lock()
		startedBy = nil
		mu.Unlock()
		if _, _, err := StartRecording(ctx, c.room, c.user); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if len(startedBy) == 0 {
			t.Errorf("%s: the recording named nobody as its starter", c.room)
		}
		for _, got := range startedBy {
			if got != c.want {
				t.Errorf("%s: the recording was started by %q, want %q", c.room, got, c.want)
			}
		}
		mu.Unlock()
	}
}
