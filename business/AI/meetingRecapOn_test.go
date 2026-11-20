package business

import (
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// V-951. A recap is written, and every call screen is told one will be, only
// while AI is on, the recap is on (it is opt-in), and a model is ready to
// write it. Any one of them off says no.
func TestARecapIsMadeOnlyWhileAIAndTheRecapAreOnAndAModelIsReady(t *testing.T) {
	ready := &ai.AIService{Config: &ai.AIConfig{Enabled: true}, LLM: struct{ ai.LLMProvider }{}}
	for _, c := range []struct {
		name     string
		settings aiModels.AISettings
		svc      *ai.AIService
		want     bool
	}{
		{"all on", aiModels.AISettings{Enabled: true, MeetingRecapEnabled: true}, ready, true},
		{"AI off", aiModels.AISettings{Enabled: false, MeetingRecapEnabled: true}, ready, false},
		{"the recap off (a new workspace)", aiModels.AISettings{Enabled: true}, ready, false},
		{"no model ready", aiModels.AISettings{Enabled: true, MeetingRecapEnabled: true}, &ai.AIService{Config: &ai.AIConfig{Enabled: true}}, false},
		{"no AI service", aiModels.AISettings{Enabled: true, MeetingRecapEnabled: true}, nil, false},
	} {
		if got := recapsAreOn(&c.settings, c.svc); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
