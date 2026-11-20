package domain

// Who a call's notes doc belongs with (V-1209).
//
// A notes doc holds a call's recap and its full transcript, and is shared with
// the people of the conversation the call was in. The doc itself names no
// conversation: the call's transcript node does, by recording_notes_doc beside
// its home (recording_channel, recording_dm or recording_event). A handover of
// a departed owner's docs reads those ties here, once for all their docs, so a
// private conversation's notes go to someone still in it.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// NotesDocTie is one notes doc's tie to the conversation its call was in.
type NotesDocTie struct {
	Doc   string
	Title string
	// Public is a public channel's notes: anyone in the workspace may open the
	// channel, so a handover is not narrowed.
	Public bool
	// ToIn says the person the handover names is in the conversation.
	ToIn bool
	// Order is who else may take it, best first, as user uuids: the channel's
	// maker, then its moderators, each still in it; or a DM's participants.
	Order []string
	// Event is the event's uuid for an event call's notes; its people are
	// read from the calendar.
	Event string
}

// NotesDocTies reads the ties of the docs among docIDs that are a call's
// notes, keyed by doc uuid, in one read; to is the person a handover names.
// A doc that is no call's notes is absent.
func NotesDocTies(ctx context.Context, docIDs []string, to string) (map[string]NotesDocTie, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}
	params, names, vars := egressIDVars(docIDs)
	vars["$to"] = to
	out, err := dgexec.Read[notesDocTiesRead](ctx, notesDocTiesQuery(params, names), vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/NotesDocTies err: %+v", err)
		return nil, err
	}
	return out.ties(), nil
}

// notesDocTiesQuery reads, for the docs named ($e0...), each call node that
// names one as its notes, with its home: a channel (private or not, whether
// $to is in it, its maker and moderators with which of the tied channels
// each is in), a DM (whether $to is in it, its participants) or an event; and
// the private docs' titles. The ids are query variables, never query text.
// Pure.
func notesDocTiesQuery(params, names string) string {
	return `query NotesDocTies(` + params + `, $to: string) {
		calls as var(func: eq(recording_notes_doc, [` + names + `]))
		var(func: uid(calls)) { recording_channel { chans as uid } }
		ties(func: uid(calls)) {
			doc: recording_notes_doc
			event: recording_event
			recording_channel {
				uid
				private: ch_private
				to_in: count(ch_members @filter(eq(user_uuid, $to)))
				maker: ch_created_by { user_uuid in: ~ch_members @filter(uid(chans)) { uid } }
				mods: ch_moderators { user_uuid in: ~ch_members @filter(uid(chans)) { uid } }
			}
			recording_dm {
				to_in: count(dm_participants @filter(eq(user_uuid, $to)))
				people: dm_participants { user_uuid }
			}
		}
		titles(func: eq(doc_uuid, [` + names + `])) @filter(eq(doc_private, true)) {
			doc_uuid
			doc_title
		}
	}`
}

type tiePerson struct {
	UUID string `json:"user_uuid"`
	In   []struct {
		Uid string `json:"uid"`
	} `json:"in"`
}

type notesDocTiesRead struct {
	Ties []struct {
		Doc   string `json:"doc"`
		Event string `json:"event"`
		// recording_channel, recording_dm and ch_created_by are single uid
		// edges: Dgraph answers each as one object.
		Channel *struct {
			Uid     string      `json:"uid"`
			Private bool        `json:"private"`
			ToIn    int         `json:"to_in"`
			Maker   *tiePerson  `json:"maker"`
			Mods    []tiePerson `json:"mods"`
		} `json:"recording_channel"`
		Dm *struct {
			ToIn   int         `json:"to_in"`
			People []tiePerson `json:"people"`
		} `json:"recording_dm"`
	} `json:"ties"`
	Titles []struct {
		UUID  string `json:"doc_uuid"`
		Title string `json:"doc_title"`
	} `json:"titles"`
}

// ties shapes the read: a channel's people count only while they are in it.
// A node with no home is no tie. Pure.
func (r notesDocTiesRead) ties() map[string]NotesDocTie {
	titles := make(map[string]string, len(r.Titles))
	for _, t := range r.Titles {
		titles[t.UUID] = t.Title
	}
	out := map[string]NotesDocTie{}
	for _, n := range r.Ties {
		tie := NotesDocTie{Doc: n.Doc, Title: titles[n.Doc]}
		switch {
		case n.Channel != nil && n.Channel.Uid != "":
			ch := n.Channel
			tie.Public, tie.ToIn = !ch.Private, ch.ToIn > 0
			people := ch.Mods
			if ch.Maker != nil {
				people = append([]tiePerson{*ch.Maker}, ch.Mods...)
			}
			for _, p := range people {
				for _, in := range p.In {
					if in.Uid == ch.Uid {
						tie.Order = append(tie.Order, p.UUID)
						break
					}
				}
			}
		case n.Dm != nil:
			tie.ToIn = n.Dm.ToIn > 0
			for _, p := range n.Dm.People {
				tie.Order = append(tie.Order, p.UUID)
			}
		case n.Event != "":
			tie.Event = n.Event
		default:
			continue
		}
		// Two calls naming one doc: the narrower tie holds.
		if prev, ok := out[n.Doc]; ok && !prev.Public {
			continue
		}
		out[n.Doc] = tie
	}
	return out
}
