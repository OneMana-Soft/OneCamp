package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/google/uuid"
)

// A4: every member of a project hears its topic, so nothing said there names a
// task that only a few may see, not even by id. Whatever is said about one
// (a change, its dates, a field, a comment or a reaction to one, or what
// GitHub changed on it, with the new values) goes out as Reread, naming
// nothing; each member then reads what it may see through the filter. Before,
// GitHub sync told every member the task and its new values.
func TestAProjectsTopicNeverNamesARestrictedTask(t *testing.T) {
	const secret = "6c1f9a52-3b7e-4d0a-9e21-5f8b7c6d4e3a"
	const open = "0b4e7c1d-2a9f-4e6b-8d3c-1f0a9b8c7d6e"
	prevSend, prevHidden := sendToProject, restrictedAmong
	t.Cleanup(func() { sendToProject, restrictedAmong = prevSend, prevHidden })
	var told []string
	sendToProject = func(_ context.Context, project string, kind int8, data any, _ string) {
		b, err := json.Marshal(mqttStruct.Message{Type: kind, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		told = append(told, project+" "+string(b))
	}
	restrictedAmong = func(ids []string) map[string]bool {
		out := map[string]bool{}
		for _, id := range ids {
			if strings.EqualFold(id, secret) {
				out[secret] = true
			}
		}
		return out
	}

	said := map[string]func(task string){
		"a change": func(task string) {
			PublishTaskChanged(context.Background(), &mqttStruct.MqttTaskChanged{TaskUuid: task, ProjectUuid: "p"})
		},
		"its dates": func(task string) {
			PublishTaskDates(&mqttStruct.MqttTaskDates{TaskUuid: task, ProjectUuid: "p", DueDate: "2026-11-02"})
		},
		"a field": func(task string) {
			PublishTaskField(&mqttStruct.MqttTaskField{TaskUuid: task, ProjectUuid: "p", FieldID: "f", Value: json.RawMessage(`"Signed"`)})
		},
		"a comment": func(task string) {
			PublishTaskComment(&mqttStruct.MqttTaskComment{TaskUuid: task, HTMLText: "Raise to 92k"}, "p")
		},
		"a reaction": func(task string) {
			PublishTaskCommentReaction(&mqttStruct.MqttTaskCommentReaction{TaskUuid: task, EmojiReactionId: "+1"}, "p")
		},
		"GitHub's sync": func(task string) {
			PublishGitHubSync(&mqttStruct.MqttGitHubSync{TaskUuid: task, ProjectUuid: "p", SyncType: "name", Payload: map[string]any{"task_name": "Salary review"}}, "p")
		},
	}
	const reread = `p {"type":40,"data":{"task_uuid":"","project_uuid":"p","reread":true}}`
	for what, say := range said {
		told = nil
		say(strings.ToUpper(secret))
		if len(told) != 1 || told[0] != reread {
			t.Errorf("%s of a restricted task told the project %v, want Reread alone", what, told)
		}
		told = nil
		say(open)
		if len(told) != 1 || !strings.Contains(told[0], open) || strings.Contains(told[0], "reread") {
			t.Errorf("%s of an open task told the project %v, want the task named", what, told)
		}
	}
}

// A batch of tasks just made is told of without reading their restriction
// again (business/Task CreateTasks marks them open), and a task the mark
// doesn't name is still read: kept off the topic when it is restricted.
// Before, each task.created of a batch read its task's restriction once more,
// so 50 tasks cost 50 more statements than 5.
func TestTasksMarkedOpenAreToldWithoutReadingTheirRestriction(t *testing.T) {
	const secret = "6c1f9a52-3b7e-4d0a-9e21-5f8b7c6d4e3a"
	made := uuid.MustParse("0b4e7c1d-2a9f-4e6b-8d3c-1f0a9b8c7d6e")
	prevSend, prevHidden := sendToProject, restrictedAmong
	t.Cleanup(func() { sendToProject, restrictedAmong = prevSend, prevHidden })
	var told, read []string
	sendToProject = func(_ context.Context, _ string, kind int8, data any, _ string) {
		b, err := json.Marshal(mqttStruct.Message{Type: kind, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		told = append(told, string(b))
	}
	restrictedAmong = func(ids []string) map[string]bool {
		read = append(read, ids...)
		out := map[string]bool{}
		for _, id := range ids {
			if id == secret {
				out[secret] = true
			}
		}
		return out
	}
	batch := helpers.WithOpenTasks(context.Background(), made)
	for _, c := range []struct {
		what  string
		ctx   context.Context
		task  string
		read  bool
		named bool
	}{
		{"one of a batch just made", batch, made.String(), false, true},
		{"a restricted task the batch's mark doesn't name", batch, secret, true, false},
		{"a task with no mark", context.Background(), made.String(), true, true},
	} {
		told, read = nil, nil
		PublishTaskChanged(c.ctx, &mqttStruct.MqttTaskChanged{TaskUuid: c.task, ProjectUuid: "p"})
		if (len(read) > 0) != c.read {
			t.Errorf("%s: read restrictions of %v, want a read %v", c.what, read, c.read)
		}
		if len(told) != 1 || strings.Contains(told[0], c.task) != c.named {
			t.Errorf("%s: told the project %v, want the task named %v", c.what, told, c.named)
		}
	}
}
