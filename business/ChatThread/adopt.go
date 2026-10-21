package business

// The chat.threads.adopt sweep: threads from before replies were indexed join
// the index, so the Threads view has the threads people took part in before
// it existed. It reads the messages with replies written in the last 90 days,
// oldest first, a few hundred a pass, and makes each thread that isn't a row
// one, with its root's author and repliers as its people, each having read it
// to its latest reply (threadIndex.AdoptAsRead): they join their threads read,
// not as a flood of unread. A thread already a row is left as it is.
//
// It goes on from where the last pass stopped (system_configs, adoptKey) until
// it has read up to now, then records itself done and never reads again: from
// then every reply is indexed as it is written. A thread whose root is older
// than the window, or that the sweep missed, joins at its next reply, or when
// someone follows or opens it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	domain "github.com/akashc777/OneCamp/domain/Message"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

const (
	adoptJob = "chat.threads.adopt"
	// adoptKey keeps where the sweep is ({"posts": …, "chats": …}), or "done: …".
	adoptKey   = "chat.threads.adopt"
	adoptEvery = 15 * time.Minute
	// adoptWindow is how far back the sweep reads.
	adoptWindow = 90 * 24 * time.Hour
	// adoptPerPass is the most roots of each kind one pass reads.
	adoptPerPass = 1000
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{
		Name: adoptJob, Every: adoptEvery, RunAtStart: true,
		Run: func(ctx context.Context) error { _, err := AdoptEarlier(ctx); return err },
	})
}

// adoptState is where the sweep is: the next root's time for each kind, and
// which kinds it has read up to now.
type adoptState struct {
	Posts     time.Time `json:"posts"`
	Chats     time.Time `json:"chats"`
	PostsDone bool      `json:"posts_done,omitempty"`
	ChatsDone bool      `json:"chats_done,omitempty"`
}

// AdoptReport is what one pass did.
type AdoptReport struct {
	Read    int  `json:"read"`
	Adopted int  `json:"adopted"`
	Done    bool `json:"done"`
}

// Seams for tests.
var (
	rootsSince  = domain.RootsSince
	adoptAsRead = threadIndex.AdoptAsRead
	adoptDone   = func() (bool, error) { return configModel.IsDone(adoptKey) }
	loadAdopt   = func() (string, error) {
		cfg, err := configModel.GetConfigByKey(adoptKey)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cfg == nil) {
			return "", nil // never run: start at the window's edge
		}
		if err != nil {
			return "", err
		}
		return cfg.Value, nil
	}
	saveAdopt = func(v string) error { return configModel.UpsertConfig(adoptKey, v) }
)

// AdoptEarlier runs one pass of the chat.threads.adopt sweep.
func AdoptEarlier(ctx context.Context) (AdoptReport, error) {
	if done, err := adoptDone(); err != nil || done {
		return AdoptReport{Done: done}, err
	}
	raw, err := loadAdopt()
	if err != nil {
		return AdoptReport{}, err
	}
	var st adoptState
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			st = adoptState{} // unreadable: start again; adopting is idempotent
		}
	}
	edge := now().Add(-adoptWindow)
	var report AdoptReport
	var passErr error
	for _, kind := range []struct {
		channel bool
		at      *time.Time
		done    *bool
	}{{true, &st.Posts, &st.PostsDone}, {false, &st.Chats, &st.ChatsDone}} {
		if *kind.done {
			continue
		}
		if kind.at.Before(edge) {
			*kind.at = edge
		}
		read, adopted, finished, err := adoptKind(ctx, kind.channel, kind.at)
		report.Read += read
		report.Adopted += adopted
		*kind.done = finished
		if err != nil {
			passErr = errors.Join(passErr, err)
		}
	}
	if st.PostsDone && st.ChatsDone {
		report.Done = true
		return report, errors.Join(passErr, saveAdopt("done: "+now().Format(time.RFC3339)))
	}
	b, _ := json.Marshal(st)
	return report, errors.Join(passErr, saveAdopt(string(b)))
}

// adoptKind adopts up to adoptPerPass roots of one kind from *at on, moving
// *at past what it read, and says whether it read up to now.
func adoptKind(ctx context.Context, channel bool, at *time.Time) (read, adopted int, finished bool, err error) {
	for read < adoptPerPass {
		if ctx.Err() != nil {
			return read, adopted, false, ctx.Err()
		}
		roots, err := rootsSince(ctx, channel, *at, domain.MaxRead)
		if err != nil {
			return read, adopted, false, err
		}
		for _, r := range roots {
			t := threadIndex.Thread{Kind: threadIndex.KindChannel, ID: r.In, Root: r.ID, RootBy: r.By, RootAt: r.CreatedAt}
			if !channel {
				t = threadIndex.Thread{Kind: threadIndex.KindOfChat(r.In), ID: r.In, Root: r.ID, RootBy: r.By, RootAt: r.CreatedAt}
			}
			made, err := adoptAsRead(ctx, t, r.LastReply, r.Repliers)
			if err != nil {
				return read, adopted, false, err
			}
			if made {
				adopted++
			}
		}
		read += len(roots)
		if len(roots) < domain.MaxRead {
			return read, adopted, true, nil
		}
		// The next page starts at the last root's time: roots written in the
		// same instant are read again, and left as they are. A page all of one
		// instant moves on past it.
		last := roots[len(roots)-1].CreatedAt
		if !last.After(*at) {
			last = at.Add(time.Millisecond)
		}
		*at = last
	}
	return read, adopted, false, nil
}
