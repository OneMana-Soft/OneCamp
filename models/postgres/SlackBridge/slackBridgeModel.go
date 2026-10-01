package models

// Storage for the live Slack bridge (migration 171). Secrets are stored sealed;
// sealing and unsealing is the business layer's job, so nothing here ever holds
// a usable token.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Origin says which side wrote a bridged message.
const (
	OriginSlack   = "slack"
	OriginOneCamp = "onecamp"
)

// Bridge is the one connected Slack workspace.
type Bridge struct {
	TeamID           string
	TeamName         string
	BotUserID        string
	BotID            string
	BotTokenEnc      string
	SigningSecretEnc string
	CreatedBy        *uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastError        string
	LastErrorAt      *time.Time
}

// Link joins one Slack channel to one OneCamp channel.
type Link struct {
	ID               uuid.UUID
	SlackChannelID   string
	SlackChannelName string
	ChannelUUID      uuid.UUID
	CreatedBy        *uuid.UUID
	CreatedAt        time.Time
}

// Message is one bridged message: a Slack ts and the post or comment it is.
type Message struct {
	SlackChannelID string
	SlackTs        string
	ChannelUUID    uuid.UUID
	PostUUID       *uuid.UUID
	CommentUUID    *uuid.UUID
	Origin         string
}

func dbCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// GetBridge returns the connected workspace, or nil when none is connected.
func GetBridge(ctx context.Context) (*Bridge, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	var b Bridge
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT team_id, team_name, bot_user_id, bot_id, bot_token_enc, signing_secret_enc,
		       created_by, created_at, updated_at, last_error, last_error_at
		FROM slack_bridge WHERE id = 1`).Scan(
		&b.TeamID, &b.TeamName, &b.BotUserID, &b.BotID, &b.BotTokenEnc, &b.SigningSecretEnc,
		&b.CreatedBy, &b.CreatedAt, &b.UpdatedAt, &b.LastError, &b.LastErrorAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// SaveBridge connects a workspace, or replaces the credentials of the one
// already connected. Connecting a DIFFERENT workspace drops the old links and
// message map, which name channels the new token cannot reach.
func SaveBridge(ctx context.Context, b Bridge) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var prevTeam sql.NullString
	if err := tx.QueryRowContext(c, `SELECT team_id FROM slack_bridge WHERE id = 1`).Scan(&prevTeam); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prevTeam.Valid && prevTeam.String != b.TeamID {
		if _, err := tx.ExecContext(c, `DELETE FROM slack_bridge_messages`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(c, `DELETE FROM slack_bridge_links`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(c, `
		INSERT INTO slack_bridge (id, team_id, team_name, bot_user_id, bot_id, bot_token_enc, signing_secret_enc, created_by)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			team_id = $1, team_name = $2, bot_user_id = $3, bot_id = $4,
			bot_token_enc = $5, signing_secret_enc = $6,
			updated_at = NOW(), last_error = '', last_error_at = NULL`,
		b.TeamID, b.TeamName, b.BotUserID, b.BotID, b.BotTokenEnc, b.SigningSecretEnc, b.CreatedBy); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteBridge disconnects Slack and forgets every link and bridged message.
// The bridged posts themselves stay: they are the channel's history.
func DeleteBridge(ctx context.Context) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM slack_bridge_messages`,
		`DELETE FROM slack_bridge_links`,
		`DELETE FROM slack_bridge`,
	} {
		if _, err := tx.ExecContext(c, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordError stores the latest delivery failure; an empty msg clears it.
func RecordError(ctx context.Context, msg string) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	var err error
	if msg == "" {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(c,
			`UPDATE slack_bridge SET last_error = '', last_error_at = NULL WHERE id = 1 AND last_error <> ''`)
	} else {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(c,
			`UPDATE slack_bridge SET last_error = $1, last_error_at = NOW() WHERE id = 1`, msg)
	}
	return err
}

// ListLinks returns every link, oldest first.
func ListLinks(ctx context.Context) ([]Link, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id, slack_channel_id, slack_channel_name, channel_uuid, created_by, created_at
		FROM slack_bridge_links ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.SlackChannelID, &l.SlackChannelName, &l.ChannelUUID, &l.CreatedBy, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ErrLinkTaken is returned when either channel already belongs to a link.
var ErrLinkTaken = errors.New("channel already linked")

// CreateLink joins two channels.
func CreateLink(ctx context.Context, l Link) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO slack_bridge_links (id, slack_channel_id, slack_channel_name, channel_uuid, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		l.ID, l.SlackChannelID, l.SlackChannelName, l.ChannelUUID, l.CreatedBy)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLinkTaken
	}
	return nil
}

// DeleteLink removes a link and reports whether it existed. The bridged
// messages of that pair are forgotten with it.
func DeleteLink(ctx context.Context, id uuid.UUID) (bool, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	var slackChannel string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`DELETE FROM slack_bridge_links WHERE id = $1 RETURNING slack_channel_id`, id).Scan(&slackChannel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM slack_bridge_messages WHERE slack_channel_id = $1`, slackChannel)
	return true, err
}

// ClaimSlackMessage reserves a Slack message for bridging. False means another
// delivery of the same message already claimed it, so this one does nothing.
func ClaimSlackMessage(ctx context.Context, slackChannel, ts string, channelUUID uuid.UUID) (bool, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO slack_bridge_messages (slack_channel_id, slack_ts, channel_uuid, origin)
		VALUES ($1, $2, $3, 'slack')
		ON CONFLICT DO NOTHING`, slackChannel, ts, channelUUID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseClaim drops a claim whose post was never written, so Slack's retry
// of the same message can try again.
func ReleaseClaim(ctx context.Context, slackChannel, ts string) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		DELETE FROM slack_bridge_messages
		WHERE slack_channel_id = $1 AND slack_ts = $2 AND post_uuid IS NULL`, slackChannel, ts)
	return err
}

// CompleteClaim records the post (and, for a thread reply, the comment) a
// claimed Slack message became.
func CompleteClaim(ctx context.Context, slackChannel, ts string, postUUID uuid.UUID, commentUUID *uuid.UUID) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE slack_bridge_messages SET post_uuid = $3, comment_uuid = $4
		WHERE slack_channel_id = $1 AND slack_ts = $2`, slackChannel, ts, postUUID, commentUUID)
	return err
}

// RecordOneCampMessage records a OneCamp post or comment delivered to Slack.
func RecordOneCampMessage(ctx context.Context, m Message) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO slack_bridge_messages (slack_channel_id, slack_ts, channel_uuid, post_uuid, comment_uuid, origin)
		VALUES ($1, $2, $3, $4, $5, 'onecamp')
		ON CONFLICT DO NOTHING`,
		m.SlackChannelID, m.SlackTs, m.ChannelUUID, m.PostUUID, m.CommentUUID)
	return err
}

const messageColumns = `slack_channel_id, slack_ts, channel_uuid, post_uuid, comment_uuid, origin`

func scanMessage(row *sql.Row) (*Message, error) {
	var m Message
	err := row.Scan(&m.SlackChannelID, &m.SlackTs, &m.ChannelUUID, &m.PostUUID, &m.CommentUUID, &m.Origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MessageBySlackTs finds the bridged message for a Slack ts, or nil.
func MessageBySlackTs(ctx context.Context, slackChannel, ts string) (*Message, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	return scanMessage(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+messageColumns+` FROM slack_bridge_messages WHERE slack_channel_id = $1 AND slack_ts = $2`,
		slackChannel, ts))
}

// MessageByPost finds the Slack message a top-level post was bridged as, or nil.
func MessageByPost(ctx context.Context, postUUID uuid.UUID) (*Message, error) {
	c, cancel := dbCtx(ctx)
	defer cancel()
	return scanMessage(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+messageColumns+` FROM slack_bridge_messages
		 WHERE post_uuid = $1 AND comment_uuid IS NULL LIMIT 1`, postUUID))
}

// DeleteMessage forgets one bridged message.
func DeleteMessage(ctx context.Context, slackChannel, ts string) error {
	c, cancel := dbCtx(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM slack_bridge_messages WHERE slack_channel_id = $1 AND slack_ts = $2`, slackChannel, ts)
	return err
}
