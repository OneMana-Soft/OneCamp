package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Option is one choice in a poll.
type Option struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Poll is a poll as stored.
type Poll struct {
	ID        uuid.UUID
	ChannelID uuid.UUID
	PostID    *uuid.UUID
	Question  string
	Options   []Option
	Multiple  bool
	ClosesAt  *time.Time
	ClosedAt  *time.Time
	CreatedBy uuid.UUID
	CreatedAt time.Time
}

// ErrNotFound is a poll id that does not exist.
var ErrNotFound = errors.New("poll not found")

func db() *sql.DB { return postgresInit.DBConn.SqlDB }

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// Create stores a new poll.
func Create(ctx context.Context, p *Poll) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	opts, err := json.Marshal(p.Options)
	if err != nil {
		return err
	}
	_, err = db().ExecContext(ctx, `
		INSERT INTO polls (id, channel_id, question, options, multiple, closes_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.ChannelID, p.Question, opts, p.Multiple, p.ClosesAt, p.CreatedBy)
	return err
}

// SetPost records the message a poll was posted as.
func SetPost(ctx context.Context, pollID, postID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `UPDATE polls SET post_id = $2 WHERE id = $1`, pollID, postID)
	return err
}

// Delete removes a poll whose message could not be posted.
func Delete(ctx context.Context, pollID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `DELETE FROM polls WHERE id = $1`, pollID)
	return err
}

// Get loads a poll.
func Get(ctx context.Context, pollID uuid.UUID) (*Poll, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var p Poll
	var opts []byte
	var post sql.NullString
	var closes, closed sql.NullTime
	err := db().QueryRowContext(ctx, `
		SELECT id, channel_id, post_id, question, options, multiple, closes_at, closed_at, created_by, created_at
		FROM polls WHERE id = $1`, pollID).
		Scan(&p.ID, &p.ChannelID, &post, &p.Question, &opts, &p.Multiple, &closes, &closed, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(opts, &p.Options); err != nil {
		return nil, err
	}
	if post.Valid {
		if id, perr := uuid.Parse(post.String); perr == nil {
			p.PostID = &id
		}
	}
	if closes.Valid {
		p.ClosesAt = &closes.Time
	}
	if closed.Valid {
		p.ClosedAt = &closed.Time
	}
	return &p, nil
}

// Tally is how many people chose each option, and what one person chose.
type Tally struct {
	Counts map[string]int
	Voters int
	Mine   []string
}

// Count tallies a poll's votes, with userID's own choices.
func Count(ctx context.Context, pollID, userID uuid.UUID) (*Tally, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(ctx, `SELECT option_id, user_id FROM poll_votes WHERE poll_id = $1`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Tally{Counts: map[string]int{}}
	voters := map[uuid.UUID]bool{}
	for rows.Next() {
		var opt string
		var uid uuid.UUID
		if err := rows.Scan(&opt, &uid); err != nil {
			return nil, err
		}
		t.Counts[opt]++
		voters[uid] = true
		if uid == userID {
			t.Mine = append(t.Mine, opt)
		}
	}
	t.Voters = len(voters)
	return t, rows.Err()
}

// ReplaceVotes sets userID's choices to optionIDs (none retracts the vote).
func ReplaceVotes(ctx context.Context, pollID, userID uuid.UUID, optionIDs []string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	tx, err := db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM poll_votes WHERE poll_id = $1 AND user_id = $2`, pollID, userID); err != nil {
		return err
	}
	for _, o := range optionIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO poll_votes (poll_id, option_id, user_id) VALUES ($1, $2, $3)`, pollID, o, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close ends voting now.
func Close(ctx context.Context, pollID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `UPDATE polls SET closed_at = now() WHERE id = $1 AND closed_at IS NULL`, pollID)
	return err
}
