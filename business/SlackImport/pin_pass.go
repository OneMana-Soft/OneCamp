package business

// A Slack channel's pins, brought across (chat B25 on the move from Slack):
// channels.json and groups.json list each channel's pinned messages (the
// message's ts, who pinned it and when). Once the messages are imported, the
// reaction pass pins them all through business/Pin Imported, each as the
// person who pinned it and at the time they did, with Pin's own rules (a
// person's live message in that channel, at most 100 a channel): three reads
// of the import's id map and one write, however many pins there are. A pin
// whose message or pinner wasn't brought across is skipped and noted as a
// warning, all the warnings in one write.

import (
	"context"
	"fmt"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	pinBusiness "github.com/akashc777/OneCamp/business/Pin"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// Seams for tests: the import's id map, the pin writer and the warnings.
var (
	lookupMappedMany = importModels.LookupIdMappingsMany
	importPins       = pinBusiness.Imported
	notePins         = importModels.LogImportErrors
)

// listedPin is a pin as an export lists it, with its channel.
type listedPin struct {
	channel, name string
	pin           SlackPin
}

// applyPins pins every listed pin of the export's public and private
// channels, and says how many it pinned and how many it skipped.
func applyPins(ctx context.Context, importID, chunkID uuid.UUID, parsed *ParsedExport) (applied, skipped int) {
	if parsed == nil {
		return 0, 0
	}
	var listed []listedPin
	for _, ch := range append(append([]SlackChannel(nil), parsed.Channels...), parsed.Groups...) {
		for _, p := range ch.Pins {
			listed = append(listed, listedPin{channel: ch.ID, name: ch.Name, pin: p})
		}
	}
	if len(listed) == 0 {
		return 0, 0
	}
	var channelIDs, messageIDs, userIDs []string
	for _, l := range listed {
		channelIDs, messageIDs, userIDs = append(channelIDs, l.channel), append(messageIDs, l.pin.ID), append(userIDs, l.pin.User)
	}
	channels, err := lookupMappedMany(ctx, importID, importModels.EntityChannel, unique(channelIDs))
	if err == nil {
		var messages, people map[string]uuid.UUID
		if messages, err = lookupMappedMany(ctx, importID, importModels.EntityMessage, unique(messageIDs)); err == nil {
			if people, err = lookupMappedMany(ctx, importID, importModels.EntityUser, unique(userIDs)); err == nil {
				return pinListed(ctx, importID, chunkID, listed, channels, messages, people)
			}
		}
	}
	notePins(ctx, importID, &chunkID, []importModels.ImportError{{
		EntityType: importModels.EntityMessage, Severity: importModels.SeverityWarning, Code: "PIN_IMPORT_FAILED",
		Message: fmt.Sprintf("the channels' %d pins weren't brought across: %v", len(listed), err),
	}})
	return 0, len(listed)
}

// pinListed pins what of listed came across (its channel, message and
// pinner), and notes the rest.
func pinListed(ctx context.Context, importID, chunkID uuid.UUID, listed []listedPin, channels, messages, people map[string]uuid.UUID) (applied, skipped int) {
	var notes []importModels.ImportError
	note := func(l listedPin, code, msg string) {
		skipped++
		notes = append(notes, importModels.ImportError{
			EntityType: importModels.EntityMessage, SourceID: l.pin.ID, Severity: importModels.SeverityWarning,
			Code: code, Message: fmt.Sprintf("a pin in #%s %s", l.name, msg),
		})
	}
	var pins []pinBusiness.ImportedPin
	var from []listedPin
	for _, l := range listed {
		channel, message, by := channels[l.channel], messages[l.pin.ID], people[l.pin.User]
		switch {
		case channel == uuid.Nil:
			skipped++ // the channel's own skip is noted where it was skipped
			continue
		case message == uuid.Nil:
			note(l, "PIN_MESSAGE_NOT_IMPORTED", "names a message that wasn't brought across")
			continue
		case by == uuid.Nil:
			note(l, "PIN_PINNER_NOT_IMPORTED", "was made by someone who wasn't brought across")
			continue
		}
		at := time.Now().UTC()
		if l.pin.Created > 0 {
			at = time.Unix(l.pin.Created, 0).UTC()
		}
		pins = append(pins, pinBusiness.ImportedPin{
			Ref:     conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: channel.String()},
			Message: message.String(), By: by, At: at,
		})
		from = append(from, l)
	}
	if len(pins) > 0 {
		refused, err := importPins(ctx, pins)
		if err != nil {
			refused = make(map[int]error, len(pins))
			for i := range pins {
				refused[i] = err
			}
		}
		for i, l := range from {
			if why := refused[i]; why != nil {
				note(l, "PIN_IMPORT_FAILED", "wasn't pinned: "+why.Error())
				continue
			}
			applied++
		}
	}
	notePins(ctx, importID, &chunkID, notes)
	return applied, skipped
}

// unique is ids, each once, in their order. Pure.
func unique(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
