package postgresInit

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type DB struct {
	GormDB    *gorm.DB
	SqlDB     *sql.DB
	DBTimeout time.Duration
}

var DBConn = &DB{}

// The pool every request and every background worker shares: outbox (8),
// event handoff (16), email (5-20), webhook deliveries (up to 100), sweeps and
// rollbacks. At 10, with waiting for a connection counted against the 3 s
// statement timeout, a busy minute timed requests out with the database
// idle. 30 leaves Postgres 12's default 100 connections room for a second API
// instance, the migration tools and an operator's psql.
const maxOpenDBConn = 30
const maxIdleDbConn = 15
const maxDbLifetime = 5 * time.Minute
const dbTimeout = time.Second * 3

func ConnectPostgres(ctx context.Context, dsn string) error {

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	sqlDB.SetMaxOpenConns(maxOpenDBConn)
	sqlDB.SetMaxIdleConns(maxIdleDbConn)
	sqlDB.SetConnMaxLifetime(maxDbLifetime)

	err = testDB(ctx, sqlDB)
	if err != nil {
		return err
	}

	DBConn.GormDB = db
	DBConn.SqlDB = sqlDB
	DBConn.DBTimeout = dbTimeout

	return nil
}

func testDB(ctx context.Context, d *sql.DB) error {
	err := d.Ping()

	if err != nil {
		helpers.LogErrorWithContext(ctx, "Failed to connect to postgres DB err:= %+v", err)
		return err
	}

	helpers.MessageLogs.InfoLog.Println("Successfully connected with postgres server !")

	return nil
}
