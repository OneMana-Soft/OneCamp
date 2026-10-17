package business

// Open tabs learn of a doc's changes live: renames, deletes and restores,
// moves, and changes to its access or lock, published on the doc's topic
// (MESSAGE_DOC_META), which only people who may read the doc subscribe to.
//
// A meta event carries what a tab needs to act on, and nothing about who was
// given or lost access: a tab told its doc's access changed asks again what it
// may do there. The body's own changes reach open editors through the
// collaboration service, not here.

import (
	"context"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// metaKeep is, for each kind open tabs hear of, the event data they're sent.
var metaKeep = map[resourceEventBusiness.Kind][]string{
	resourceEventBusiness.DocRenamed:        {"title"},
	resourceEventBusiness.DocMoved:          {"parent"},
	resourceEventBusiness.DocDeleted:        nil,
	resourceEventBusiness.DocRestored:       nil,
	resourceEventBusiness.DocPurged:         nil,
	resourceEventBusiness.DocArchived:       nil,
	resourceEventBusiness.DocAccessChanged:  {"private", "public_comment"},
	resourceEventBusiness.DocShared:         nil,
	resourceEventBusiness.DocLocked:         {"level"},
	resourceEventBusiness.DocVersionRestore: nil,
	resourceEventBusiness.DocTransferred:    nil,
	resourceEventBusiness.DocRecovered:      nil,
	// Threads: the client reads the thread again.
	resourceEventBusiness.ThreadCreated:    threadMeta,
	resourceEventBusiness.ThreadReplied:    threadMeta,
	resourceEventBusiness.ThreadResolved:   threadMeta,
	resourceEventBusiness.ThreadReopened:   threadMeta,
	resourceEventBusiness.ThreadReanchored: threadMeta,
}

// threadMeta is what open tabs are sent of a thread's event: ids, never text.
var threadMeta = []string{"thread_id", "anchor_kind", "status", "comment_id"}

// Seam for tests: where meta events are published.
var publishMeta = func(docUUID string, meta mqttStruct.MqttDocMeta) {
	mqttBusiness.PublishToTopics([]string{helpers.GetMqttTopicForDoc(docUUID)}, mqttStruct.MESSAGE_DOC_META, meta)
}

func isMeta(e resourceEventBusiness.Event) bool {
	_, ok := metaKeep[e.Kind]
	return ok && e.ResourceKind == "doc"
}

func publishDocMeta(_ context.Context, e resourceEventBusiness.Event) {
	publishMeta(e.ResourceUUID, docMeta(e))
}

// docMeta is the meta event for e. Pure.
func docMeta(e resourceEventBusiness.Event) mqttStruct.MqttDocMeta {
	m := mqttStruct.MqttDocMeta{
		Kind:      string(e.Kind),
		DocUuid:   e.ResourceUUID,
		ActorUuid: e.Actor.UserUUID,
		AgentUuid: e.Actor.AgentUUID,
		At:        e.At.UTC(),
	}
	for _, k := range metaKeep[e.Kind] {
		if v, ok := e.Data[k]; ok {
			if m.Data == nil {
				m.Data = map[string]string{}
			}
			m.Data[k] = v
		}
	}
	return m
}
