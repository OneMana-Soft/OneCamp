package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

func boardPoll() *pollModel.Poll {
	board := uuid.New()
	return &pollModel.Poll{
		ID: uuid.New(), ChannelID: uuid.Nil, BoardID: &board, FrameID: "intro", Question: "Ship on Friday?",
		Options: []pollModel.Option{{ID: "1", Text: "Yes"}, {ID: "2", Text: "No"}}, CreatedBy: uuid.New(),
	}
}

// A board's poll is its board's and its frame's, never a channel's.
func TestABoardPollReadsAsItsBoardsNotAChannels(t *testing.T) {
	p := boardPoll()
	v := view(p, &pollModel.Tally{Counts: map[string]int{"1": 2}, Voters: 2}, uuid.Nil, false)
	if v.Channel != "" || v.Board != p.BoardID.String() || v.Frame != "intro" {
		t.Fatalf("a board's poll reads channel %q, board %q, frame %q; want no channel, its board and its frame", v.Channel, v.Board, v.Frame)
	}
}

// A share-link guest reads the question and the tally: never whose a vote
// is, nobody's own choices, and no right to close it.
func TestAGuestReadsABoardPollsQuestionAndTallyAlone(t *testing.T) {
	p := boardPoll()
	v := GuestBoardView(p, &pollModel.Tally{Counts: map[string]int{"1": 2, "2": 1}, Voters: 3, Mine: []string{"1"}})
	if v.Question != "Ship on Friday?" || v.Voters != 3 || v.Options[0].Votes != 2 || v.Options[1].Votes != 1 {
		t.Fatalf("the guest's view lost the question or the tally: %+v", v)
	}
	if v.Mine == nil || len(v.Mine) != 0 || v.CanClose || v.CreatedBy != "" {
		t.Fatalf("the guest's view says mine %v, can close %v, made by %q; want none, false and nobody", v.Mine, v.CanClose, v.CreatedBy)
	}
}

// A vote or a close on a board's poll tells the board's own topic, which
// only who may open the board hears (business/MqttAccess readsBoard), and
// never a channel's; a channel's poll still tells its channel.
func TestABoardPollIsToldOnTheBoardsTopicAlone(t *testing.T) {
	type told struct {
		where, poll, frame string
		kind               int8
	}
	var got []told
	oldPoll, oldBoard := publishPoll, publishBoardPoll
	t.Cleanup(func() { publishPoll, publishBoardPoll = oldPoll, oldBoard })
	publishPoll = func(topic, pollID string) { got = append(got, told{where: "channel " + topic, poll: pollID}) }
	publishBoardPoll = func(board, pollID, frame string, kind int8) {
		got = append(got, told{where: "board " + board, poll: pollID, frame: frame, kind: kind})
	}

	p := boardPoll()
	where{board: p.BoardID.String()}.tell(p, mqttStruct.TYPE_UPDATE)
	if len(got) != 1 || got[0] != (told{where: "board " + p.BoardID.String(), poll: p.ID.String(), frame: "intro", kind: mqttStruct.TYPE_UPDATE}) {
		t.Fatalf("a board's poll told %+v; want its board's topic once", got)
	}

	got = nil
	inChannel := &pollModel.Poll{ID: uuid.New(), ChannelID: uuid.New()}
	where{channel: &dgraphStruct.DgraphChannel{}}.tell(inChannel, mqttStruct.TYPE_UPDATE)
	if len(got) != 1 || got[0].where != "channel "+inChannel.ChannelID.String() {
		t.Fatalf("a channel's poll told %+v; want its channel", got)
	}
}
