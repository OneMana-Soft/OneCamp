package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestClassifyMentionIntent(t *testing.T) {
	agent := &model.AiAgent{Name: "Standup Bot", TriggerConfig: `{"handle":"standup"}`}

	cases := []struct {
		name  string
		text  string
		agent *model.AiAgent
		want  MentionIntent
	}{
		{"bare mention", "@Standup Bot", agent, IntentGreeting},
		{"hi", "hi @Standup Bot", agent, IntentGreeting},
		{"hello there", "hello there @bot", agent, IntentGreeting},
		{"good morning", "good morning @Standup Bot", agent, IntentGreeting},
		{"hey with punctuation", "hey!", agent, IntentGreeting},
		{"stop", "stop", agent, IntentDismiss},
		{"close", "@bot close", agent, IntentDismiss},
		{"never mind", "never mind", agent, IntentDismiss},
		{"nvm", "nvm @bot", agent, IntentDismiss},
		{"no thanks", "no thanks", agent, IntentDismiss},
		{"thats all", "that's all, thanks", agent, IntentDismiss},
		{"real task", "@Standup Bot summarize yesterday", agent, IntentTask},
		{"greeting then task", "hi, can you summarize?", agent, IntentTask},
		{"stop then task", "stop and tell me the status", agent, IntentTask},
		{"html mention span", `<p>hi <span data-mention-id="0x123">@Bot</span></p>`, agent, IntentGreeting},
		{"html dismissal", `<p><span data-mention-id="0x123">@Bot</span> never mind</p>`, agent, IntentDismiss},
		{"no agent info", "hi", nil, IntentGreeting},
		{"no agent info task", "summarize", nil, IntentTask},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyMentionIntent(c.text, c.agent)
			if got != c.want {
				t.Errorf("classifyMentionIntent(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}
