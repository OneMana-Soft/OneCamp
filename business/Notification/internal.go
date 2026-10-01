package notification

import (
	"github.com/google/uuid"
)

// uuidParse is a thin alias kept here so the dispatcher file's imports
// don't need to repeat google/uuid for one parse call.
func uuidParse(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// EmailWorkerSignal wakes the worker the moment a new row lands in the queue,
// instead of waiting for the next periodic poll. Buffered (1) so the sender
// never blocks: a missed signal just means the worker handles the row on the
// next poll, which is acceptable because polls are frequent (15s).
var EmailWorkerSignal = make(chan struct{}, 1)

// signalEmailWorker performs a non-blocking send on EmailWorkerSignal.
func signalEmailWorker() {
	select {
	case EmailWorkerSignal <- struct{}{}:
	default:
	}
}

// recipientsFromStrings converts a string-UUID list to Recipient structs,
// silently dropping any that fail to parse. Used by the wiring helpers
// so callers (chat/channel/comment/task) don't have to repeat parse
// boilerplate at every emit site.
func recipientsFromStrings(uuids []string) []Recipient {
	out := make([]Recipient, 0, len(uuids))
	seen := make(map[string]struct{}, len(uuids))
	for _, s := range uuids {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		// Use the userUUID we already have — these are Dgraph UUIDs which
		// are also the Postgres user IDs. Skip ones that don't parse.
		// (Same shape used everywhere in business/User/business/Chat.)
		if id, err := uuidParse(s); err == nil {
			out = append(out, Recipient{UserUUID: id})
		}
	}
	return out
}
