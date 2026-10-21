package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// U2a (#19): the work strip on a thread asked whether the reader sees the
// job's surface once a job, a graph read each, for jobs that are nearly
// always on the same surface.
func TestASurfaceIsAskedAboutOnce(t *testing.T) {
	asked := 0
	sees := onceASurface(func(*model.AgentActiveTask) bool { asked++; return true })
	same := &model.AgentActiveTask{Surface: `{"kind":"channel_post","channel_id":"c1"}`}
	for i := 0; i < 5; i++ {
		if !sees(same) {
			t.Fatal("a surface the reader sees")
		}
	}
	sees(&model.AgentActiveTask{Surface: `{"kind":"channel_post","channel_id":"c2"}`})
	if asked != 2 {
		t.Errorf("asked %d times for two surfaces", asked)
	}
}
