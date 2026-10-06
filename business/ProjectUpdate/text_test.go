package business

import "testing"

func TestCheck(t *testing.T) {
	if _, err := Check(Input{Health: "great", Body: "x"}); err == nil {
		t.Error("an unknown health must be refused")
	}
	if _, err := Check(Input{Health: "on_track", Body: "  \n "}); err == nil {
		t.Error("an empty update must be refused")
	}
	if _, err := Check(Input{Health: "on_track", Body: "ok", ChannelUUID: "general"}); err == nil {
		t.Error("a channel that isn't an id must be refused")
	}
	in, err := Check(Input{Health: "at_risk", Body: " Late \r\n\r\n\r\n- a "})
	if err != nil || in.Body != "Late\n\n- a" {
		t.Errorf("Check = %+v, %v", in, err)
	}
}
