package models

// Per-channel AI settings. Today this holds one opt-in, the weekly channel
// report, and it exists because the org-wide toggle alone was all-or-nothing:
// enabling it posted into every active channel with no way to decline.
//
// The org switch in ai_settings is the ceiling and these rows are the opt-in
// beneath it. A missing row means disabled, so a channel is silent until
// somebody deliberately turns it on, and the org can still switch everything
// off in one place.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ChannelTeamReportEnabled reports whether this one channel has opted in.
// A missing row is not an error: it is the default, and it means off.
func ChannelTeamReportEnabled(ctx context.Context, channelUUID uuid.UUID) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var enabled bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT team_report_enabled FROM channel_ai_settings WHERE channel_uuid = $1`,
		channelUUID).Scan(&enabled)
	if err != nil {
		// sql.ErrNoRows is the common case (channel never configured), and it
		// carries the same meaning as an explicit false.
		return false, nil
	}
	return enabled, nil
}

// SetChannelTeamReportEnabled records a channel's choice, creating the row on
// first use. updatedBy is the acting channel admin, kept for the audit trail.
func SetChannelTeamReportEnabled(ctx context.Context, channelUUID uuid.UUID, enabled bool, updatedBy uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`INSERT INTO channel_ai_settings (channel_uuid, team_report_enabled, updated_by, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (channel_uuid) DO UPDATE
		    SET team_report_enabled = EXCLUDED.team_report_enabled,
		        updated_by          = EXCLUDED.updated_by,
		        updated_at          = NOW()`,
		channelUUID, enabled, updatedBy)
	if err != nil {
		return fmt.Errorf("set channel team report enabled: %w", err)
	}
	return nil
}

// TeamReportEnabledChannels returns the set of channels that have opted in.
//
// The agent asks this ONCE per weekly run and filters its discovered channels
// against it, rather than querying per channel: discovery can return 100
// channels a run, and the opted-in set is normally far smaller than that.
func TeamReportEnabledChannels(ctx context.Context) (map[string]bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT channel_uuid FROM channel_ai_settings WHERE team_report_enabled`)
	if err != nil {
		return nil, fmt.Errorf("list team report channels: %w", err)
	}
	defer rows.Close()

	enabled := make(map[string]bool)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan team report channel: %w", err)
		}
		enabled[id.String()] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate team report channels: %w", err)
	}
	return enabled, nil
}
