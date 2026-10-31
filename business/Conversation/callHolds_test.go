package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// withChatHeld answers which posts' live calls hold chat for whoever asks,
// and records what each read was asked.
func withChatHeld(t *testing.T, held map[uuid.UUID]bool, fail error) *[][]uuid.UUID {
	t.Helper()
	old := chatHeldOver
	t.Cleanup(func() { chatHeldOver = old })
	var asked [][]uuid.UUID
	chatHeldOver = func(_ context.Context, posts []uuid.UUID, member *uuid.UUID, identity string) (map[uuid.UUID]bool, error) {
		if member == nil || identity != "" {
			t.Errorf("a member's thread is asked by their uuid: %v %q", member, identity)
		}
		asked = append(asked, posts)
		out := map[uuid.UUID]bool{}
		for _, post := range posts {
			if held[post] {
				out[post] = true
			}
		}
		return out, fail
	}
	return &asked
}

// While a channel's call holds chat, its post's thread drops to Read for
// whoever would reply there, with the reason the web words (calls meet-12,
// agreed with CHAT-LEAD-7). Its moderators keep it, as do other threads, and
// what's read already isn't asked about: one read for every thread at once.
func TestAHeldCallsThreadIsReadOnly(t *testing.T) {
	sam := accessBusiness.Subject{UserUUID: uuid.NewString()}
	callPost, other, moderated, readOnly := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	asked := withChatHeld(t, map[uuid.UUID]bool{callPost: true, moderated: true}, nil)
	out := map[string]accessBusiness.Decision{
		callPost.String():  {Level: LevelReply, Reasons: []string{reasonMember}},
		other.String():     {Level: LevelReply, Reasons: []string{reasonMember}},
		moderated.String(): {Level: LevelModerate, Reasons: []string{reasonModerator}},
		readOnly.String():  {Level: LevelRead, Reasons: []string{reasonMember}, Archived: true},
	}
	if err := chatHolds(context.Background(), sam, out); err != nil {
		t.Fatal(err)
	}
	if d := out[callPost.String()]; d.Level != LevelRead || d.Was != LevelReply || !slices.Contains(d.Reasons, ReasonCallChatHeld) {
		t.Errorf("the held call's thread: %+v", d)
	}
	if d := out[other.String()]; d.Level != LevelReply || slices.Contains(d.Reasons, ReasonCallChatHeld) {
		t.Errorf("another thread: %+v", d)
	}
	if d := out[moderated.String()]; d.Level != LevelModerate {
		t.Errorf("a moderator, who runs the channel's calls: %+v", d)
	}
	if len(*asked) != 1 || len((*asked)[0]) != 2 || slices.Contains((*asked)[0], moderated) || slices.Contains((*asked)[0], readOnly) {
		t.Errorf("one read, for the threads at reply only: %v", *asked)
	}
}

// A read that fails is the decision's error, never a silent open or close;
// with nothing at reply there's nothing to read.
func TestAHoldThatCantBeReadIsAnError(t *testing.T) {
	sam := accessBusiness.Subject{UserUUID: uuid.NewString()}
	down := errors.New("postgres is down")
	withChatHeld(t, nil, down)
	out := map[string]accessBusiness.Decision{uuid.NewString(): {Level: LevelReply}}
	if err := chatHolds(context.Background(), sam, out); !errors.Is(err, down) {
		t.Errorf("a failed read: %v", err)
	}
	asked := withChatHeld(t, nil, nil)
	if err := chatHolds(context.Background(), sam, map[string]accessBusiness.Decision{uuid.NewString(): {Level: LevelRead}}); err != nil || len(*asked) != 0 {
		t.Errorf("nothing at reply: %v, %d reads", err, len(*asked))
	}
}

// An agent acting for a member is held as that member.
func TestAnAgentIsHeldAsItsMember(t *testing.T) {
	member := uuid.New()
	post := uuid.New()
	old := chatHeldOver
	t.Cleanup(func() { chatHeldOver = old })
	chatHeldOver = func(_ context.Context, _ []uuid.UUID, who *uuid.UUID, _ string) (map[uuid.UUID]bool, error) {
		if who == nil || *who != member {
			return map[uuid.UUID]bool{}, nil
		}
		return map[uuid.UUID]bool{post: true}, nil
	}
	out := map[string]accessBusiness.Decision{post.String(): {Level: LevelReply}}
	if err := chatHolds(context.Background(), accessBusiness.Subject{UserUUID: member.String(), AgentUUID: uuid.NewString()}, out); err != nil {
		t.Fatal(err)
	}
	if out[post.String()].Level != LevelRead {
		t.Errorf("the agent writes where its member can't: %+v", out[post.String()])
	}
}
