package livekitInit

import (
	"testing"

	"github.com/livekit/protocol/livekit"
)

// The default is what every install had before the variable existed; the
// small-machine value `make tune` writes must be understood.
func TestEgressPresetDefaultsTo1080pAndKnows720p(t *testing.T) {
	if EgressPreset("") != livekit.EncodingOptionsPreset_H264_1080P_30 {
		t.Error("empty must be 1080p30, the historical default")
	}
	if EgressPreset("nonsense") != livekit.EncodingOptionsPreset_H264_1080P_30 {
		t.Error("an unknown value must fall back, not fail")
	}
	if EgressPreset("720p30") != livekit.EncodingOptionsPreset_H264_720P_30 || EgressPreset(" 720P ") != livekit.EncodingOptionsPreset_H264_720P_30 {
		t.Error("720p30 is what tune writes for four cores or fewer")
	}
}
