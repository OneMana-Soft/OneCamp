package domain

// Messages read by their uuids from the graph, as they are now: a channel's
// posts or a DM's or group's messages, with the conversation each is in. A
// conversation's pins read them (business/Pin), so a pin is shown only for a
// message still in the conversation it was pinned to and still there; so do a
// person's threads (business/ChatThread), for each thread's root.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// Person is who wrote a message.
type Person struct {
	UUID       string `json:"user_uuid"`
	Name       string `json:"user_name,omitempty"`
	FullName   string `json:"user_full_name,omitempty"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
	IsBot      bool   `json:"is_bot,omitempty"`
}

// Message is one message as the graph holds it.
type Message struct {
	ID        string
	HTML      string
	CreatedAt *time.Time
	DeletedAt *time.Time
	Kind      string // "system" for a system line
	By        *Person
	// In is the conversation it is in: a channel's uuid, a DM's or group's
	// grouping id.
	In    string
	Files int
	// Place is that conversation as a person reads it: a channel's or a
	// named group's name, and a DM's or group's first few people.
	Place Place
}

// Place is a conversation as a list of threads or pins names it.
type Place struct {
	Name string `json:"name,omitempty"`
	// People are a DM's or group's first PlacePeople people, and Count how
	// many it has.
	People []Person `json:"people,omitempty"`
	Count  int      `json:"count,omitempty"`
}

// PlacePeople is how many of a DM's or group's people a place names.
const PlacePeople = 4

// MaxRead is the most messages one read takes: a conversation holds at most
// 100 pins (business/Pin), and a page of threads is smaller.
const MaxRead = 100

// Messages reads the messages ids name: posts when channel, else a DM's or
// group's messages. Ids that aren't uuids are left out; a message that isn't
// there is missing from the answer.
func Messages(ctx context.Context, channel bool, ids []string) ([]Message, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	if channel {
		return posts(ctx, list)
	}
	return chats(ctx, list)
}

type messageRow struct {
	ID        string     `json:"id"`
	HTML      string     `json:"html"`
	CreatedAt *time.Time `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
	Kind      string     `json:"kind"`
	By        *Person    `json:"by"`
	In        *struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		People []Person `json:"people"`
		Count  int      `json:"count"`
	} `json:"in"`
	Files int `json:"files"`
}

func (r messageRow) message() Message {
	m := Message{ID: r.ID, HTML: r.HTML, CreatedAt: r.CreatedAt, DeletedAt: r.DeletedAt, Kind: r.Kind, By: r.By, Files: r.Files}
	if r.In != nil {
		m.In = r.In.ID
		m.Place = Place{Name: r.In.Name, People: r.In.People, Count: r.In.Count}
	}
	return m
}

func read(ctx context.Context, query string) ([]Message, error) {
	var out struct {
		M []messageRow `json:"m"`
	}
	if err := dgraphInit.ReadInto(ctx, query, nil, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Message read err: %+v", err)
		return nil, err
	}
	msgs := make([]Message, 0, len(out.M))
	for _, r := range out.M {
		msgs = append(msgs, r.message())
	}
	return msgs, nil
}

// Live answers which of posts (a channel's messages) and chats (a DM's or
// group's) are there and not deleted, by id: ONE read of the graph however
// many ids of either kind there are, two blocks of one query, each only the
// ids that are live. The Threads count asks it of up to business/ChatThread's
// MaxThreads roots, past what a full read takes (MaxRead).
func Live(ctx context.Context, posts, chats []string) (map[string]bool, error) {
	out := map[string]bool{}
	q := liveQuery(dgraphquery.UUIDList(posts), len(posts), dgraphquery.UUIDList(chats), len(chats))
	if q == "" {
		return out, nil
	}
	var got struct {
		P []struct {
			ID string `json:"id"`
		} `json:"p"`
		C []struct {
			ID string `json:"id"`
		} `json:"c"`
	}
	if err := dgraphInit.ReadInto(ctx, q, nil, &got); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Message Live err: %+v", err)
		return nil, err
	}
	for _, m := range got.P {
		out[m.ID] = true
	}
	for _, m := range got.C {
		out[m.ID] = true
	}
	return out, nil
}

// liveQuery is Live's one query: a block for the posts and one for the chats
// it names (an empty list has no block), "" when it names none. Pure.
func liveQuery(posts string, nPosts int, chats string, nChats int) string {
	var blocks []string
	if posts != "" {
		blocks = append(blocks, fmt.Sprintf(`p(func: eq(post_uuid, [%s]), first: %d) @filter(type(Post) AND %s) { id: post_uuid }`,
			posts, nPosts, dgraphquery.Live("post_deleted_at")))
	}
	if chats != "" {
		blocks = append(blocks, fmt.Sprintf(`c(func: eq(chat_uuid, [%s]), first: %d) @filter(type(Chat) AND %s) { id: chat_uuid }`,
			chats, nChats, dgraphquery.Live("chat_deleted_at")))
	}
	if len(blocks) == 0 {
		return ""
	}
	return "{\n\t\t" + strings.Join(blocks, "\n\t\t") + "\n\t}"
}

func posts(ctx context.Context, list string) ([]Message, error) {
	return read(ctx, fmt.Sprintf(`{
		m(func: eq(post_uuid, [%s]), first: %d) @filter(type(Post)) {
			id: post_uuid
			html: post_text
			created_at: post_created_at
			deleted_at: post_deleted_at
			kind: post_kind
			by: post_by {
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
			}
			in: post_channel { id: ch_uuid name: ch_name }
			files: count(post_attachments)
		}
	}`, list, MaxRead))
}

func chats(ctx context.Context, list string) ([]Message, error) {
	return read(ctx, fmt.Sprintf(`{
		m(func: eq(chat_uuid, [%s]), first: %d) @filter(type(Chat)) {
			id: chat_uuid
			html: chat_body_text
			created_at: chat_created_at
			deleted_at: chat_deleted_at
			kind: chat_kind
			by: chat_from {
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				is_bot
			}
			in: chat_dm {
				id: dm_grouping_id
				name: dm_name
				count: count(dm_participants)
				people: dm_participants(first: %d) {
					user_uuid
					user_name
					user_full_name
					user_profile_object_key
					is_bot
				}
			}
			files: count(chat_attachments)
		}
	}`, list, MaxRead, PlacePeople))
}
