package business

import (
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// An update says what it changed, and nothing it didn't (B28).
func TestAChannelUpdateSaysWhatItChanged(t *testing.T) {
	live, gone := time.Time{}, time.Now()
	before := &dgraphStruct.DgraphChannel{Uuid: "c", Name: "q4-launch", About: "Old", DeletedAt: &live}
	kinds := func(es []resourceEventBusiness.Event) []resourceEventBusiness.Kind {
		var out []resourceEventBusiness.Kind
		for _, e := range es {
			out = append(out, e.Kind)
		}
		return out
	}
	got := ChannelChanges(before, &adapter.UpdateChannelInfo{ChannelName: "launch", ChannelAbout: "Ship on Friday"}, PrivacyClosed)
	if k := kinds(got); len(k) != 3 || k[0] != resourceEventBusiness.ChannelRenamed || k[1] != resourceEventBusiness.ChannelTopicChanged || k[2] != resourceEventBusiness.ChannelPrivacyChanged {
		t.Fatalf("kinds %v", k)
	}
	if got[0].Data["was"] != "q4-launch" || got[0].Data["name"] != "launch" || got[2].Data["private"] != "true" || got[0].ResourceUUID != "c" {
		t.Fatalf("data %+v", got)
	}
	if got := ChannelChanges(before, &adapter.UpdateChannelInfo{ChannelName: "q4-launch"}, PrivacyChange(0)); len(got) != 0 {
		t.Fatalf("an update that changed nothing said %v", kinds(got))
	}
	if k := kinds(ChannelChanges(before, &adapter.UpdateChannelInfo{ChannelName: "x", ChannelArchived: true}, PrivacyChange(0))); len(k) != 1 || k[0] != resourceEventBusiness.ChannelArchived {
		t.Fatalf("an archive: %v", k)
	}
	archived := &dgraphStruct.DgraphChannel{Uuid: "c", Name: "a", DeletedAt: &gone}
	if k := kinds(ChannelChanges(archived, &adapter.UpdateChannelInfo{ChannelName: "a"}, PrivacyChange(0))); len(k) != 1 || k[0] != resourceEventBusiness.ChannelRestored {
		t.Fatalf("a restore: %v", k)
	}
}
