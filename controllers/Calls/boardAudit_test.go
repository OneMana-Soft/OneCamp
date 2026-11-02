package controllers

import (
	"testing"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
)

// Every board action on a call's stage says, in the audit log, what it did,
// and a slide move keeps which frame it showed, the whole board included.
func TestBoardActionsSayWhatTheyDid(t *testing.T) {
	for _, a := range []string{callControlBusiness.ActPresentBoard, callControlBusiness.ActNewBoard, callControlBusiness.ActStopBoard, callControlBusiness.ActBoardDraw,
		callControlBusiness.ActBoardGuests, callControlBusiness.ActUndoGuests, callControlBusiness.ActBoardSlide} {
		if actSummaries[a] == "" {
			t.Errorf("%s has no audit summary", a)
		}
	}
	if m := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActBoardSlide, Slide: "frame-2"}, callControlBusiness.Result{Affected: 1}); m["slide"] != "frame-2" {
		t.Errorf("a move to frame-2 is kept as %v", m)
	}
	if m := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActBoardSlide}, callControlBusiness.Result{Affected: 1}); m["slide"] != "" {
		t.Errorf("a move back to the whole board is kept as %v", m)
	}
	if m := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActStopBoard}, callControlBusiness.Result{Affected: 1}); m["slide"] != nil {
		t.Errorf("a stop keeps a slide: %v", m)
	}
}

// Running a call's breakout rooms says what it did, and keeps how many rooms,
// which room and for how long, never what was said to every room.
func TestBreakoutActionsSayWhatTheyDidNeverWhatWasSaid(t *testing.T) {
	for _, a := range []string{callControlBusiness.ActBreakoutsSet, callControlBusiness.ActBreakoutsAssign, callControlBusiness.ActBreakoutsSpread,
		callControlBusiness.ActBreakoutsOpen, callControlBusiness.ActBreakoutsClose, callControlBusiness.ActBreakoutsBroadcast} {
		if actSummaries[a] == "" {
			t.Errorf("%s has no audit summary", a)
		}
	}
	said := "Our numbers for the board meeting"
	meta := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsBroadcast, Text: said}, callControlBusiness.Result{Affected: 3})
	for k, v := range meta {
		if v == said {
			t.Errorf("the audit keeps what was said to every room, as %q", k)
		}
	}
	if m := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"A", "B"}}, callControlBusiness.Result{}); m["rooms"] != 2 {
		t.Errorf("setting up two rooms is kept as %v", m)
	}
	if m := actMeta("s1", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen, On: true, Minutes: 15}, callControlBusiness.Result{}); m["minutes"] != 15 || m["choose"] != true {
		t.Errorf("opening the rooms for 15 minutes, people choosing, is kept as %v", m)
	}
}
