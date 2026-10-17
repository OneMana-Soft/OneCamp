// Package models (DocExport) keeps which private docs their owners let into a
// workspace export (migration 343, doc_export_consents).
package models

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

var errNoDatabase = errors.New("doc export consents: no database")

// Allow records that ownerUUID lets docUUID into workspace exports, or moves
// the consent to them when someone else gave it before.
func Allow(ctx context.Context, docUUID, ownerUUID uuid.UUID) error {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return errNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO doc_export_consents (doc_uuid, owner_uuid) VALUES ($1, $2)
		ON CONFLICT (doc_uuid) DO UPDATE SET owner_uuid = EXCLUDED.owner_uuid, created_at = NOW()`, docUUID, ownerUUID)
	return err
}

// Withdraw takes the doc's consent away.
func Withdraw(ctx context.Context, docUUID uuid.UUID) error {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return errNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM doc_export_consents WHERE doc_uuid = $1`, docUUID)
	return err
}

// Given is, for each of the docs that has a consent, who gave it.
func Given(ctx context.Context, docUUIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(docUUIDs) == 0 {
		return out, nil
	}
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, errNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT doc_uuid, owner_uuid FROM doc_export_consents WHERE doc_uuid = ANY($1)`, pq.Array(docUUIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var doc, owner uuid.UUID
		if err := rows.Scan(&doc, &owner); err != nil {
			return nil, err
		}
		out[doc] = owner
	}
	return out, rows.Err()
}
