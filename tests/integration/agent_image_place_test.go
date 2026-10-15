//go:build integration
// +build integration

package integration_test

// An agent mentioned under a message reads the pictures in it, and in its
// thread, to "see" them. What a message lists as its files is written as its
// sender sent it, storage keys and all, so a list could name anyone's file: a
// member could post a message listing the key of a picture from a channel
// they aren't in, mention an agent, and be told what it shows. A picture is
// now read through the conversation its message is in: only that
// conversation's own files, each by the key its upload stored.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAgentReadsOnlyTheConversationsOwnPictures -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnAgentReadsOnlyTheConversationsOwnPictures(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	mia, ann := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"mia": mia, "ann": ann} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	ours, theirs := uuid.NewString(), uuid.NewString()
	ourPicture, theirPicture := uuid.NewString(), uuid.NewString()
	for _, f := range []struct{ id, key, channel string }{
		{ourPicture, mia.String() + "/" + ourPicture + "_whiteboard.png", ours},
		{theirPicture, ann.String() + "/" + theirPicture + "_salaries.png", theirs},
	} {
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'channel', $3, $4)`,
			f.id, f.key, f.channel, mia); err != nil {
			t.Fatal(err)
		}
	}

	// The message in our channel lists another channel's picture, a picture
	// that isn't there, and its own, with keys of the sender's choosing.
	listed := []aiBusiness.ImageRef{
		{ID: theirPicture, ObjKey: ann.String() + "/" + theirPicture + "_salaries.png", FileName: "salaries.png"},
		{ID: uuid.NewString(), ObjKey: "anything", FileName: "nothing.png"},
		{ID: ourPicture, ObjKey: ann.String() + "/" + theirPicture + "_salaries.png", FileName: "whiteboard.png"},
	}
	got := agentBusiness.PlaceImageRefs(ctx, fileAccessBusiness.Place{Kind: "channel", ID: ours}, listed)
	if len(got) != 1 || got[0].ID != ourPicture || got[0].ObjKey != mia.String()+"/"+ourPicture+"_whiteboard.png" {
		t.Fatalf("read %+v; want only our own picture, by the key its upload stored", got)
	}
}
