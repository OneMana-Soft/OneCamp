package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type SystemConfig struct {
	ID        uuid.UUID `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

func GetConfigByKey(key string) (config *SystemConfig, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `SELECT id, key, value, updated_at FROM system_configs WHERE key = $1`

	var cfg SystemConfig
	var updatedAt sql.NullTime

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, key).Scan(
		&cfg.ID,
		&cfg.Key,
		&cfg.Value,
		&updatedAt,
	)
	if err != nil {
		// An unset key is how a setting says "use the default".
		if !errors.Is(err, sql.ErrNoRows) {
			helpers.LogErrorWithContext(ctx,
				"models/GetConfigByKey Failed to get config by key err: %+v",
				err)
		}
		return
	}

	if updatedAt.Valid {
		cfg.UpdatedAt = updatedAt.Time
	}

	return &cfg, nil
}

func UpsertConfig(key string, value string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		INSERT INTO system_configs (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, key, value)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpsertConfig Failed to upsert config err: %+v",
			err)
		return
	}

	return
}

func GetMultipleConfigsByKeys(keys []string) (configs []*SystemConfig, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Build placeholders
	placeholders := helpers.Placeholders(len(keys))
	query := `SELECT id, key, value, updated_at FROM system_configs WHERE key IN (` + placeholders + `)`

	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetMultipleConfigsByKeys Failed to get configs err: %+v",
			err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cfg SystemConfig
		var updatedAt sql.NullTime
		if err = rows.Scan(&cfg.ID, &cfg.Key, &cfg.Value, &updatedAt); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetMultipleConfigsByKeys Failed to scan config err: %+v",
				err)
			return
		}
		if updatedAt.Valid {
			cfg.UpdatedAt = updatedAt.Time
		}
		configs = append(configs, &cfg)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Config rows iteration failed err: %+v", err)
		return
	}

	return
}
