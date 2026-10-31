package postgresInit

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
	cfg, err := sessionConfig(dsn)
	if err != nil {
		return err
	}
	sqlDB := stdlib.OpenDB(*cfg)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		_ = sqlDB.Close()
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

// sessionConfig is dsn's connection config with the settings every session
// of the pool starts with: they go in its startup message, so no statement
// ever runs before them, on a session the pool makes at any time.
//
// jit off: Postgres compiles a statement whose estimated cost passes
// jit_above_cost before it reads a row, which pays back only on long
// analytical queries the app doesn't send. A table's three sums over 100k
// rows spent 1.5-1.8 s of their 3-4.7 s compiling on a half-CPU Postgres,
// past their 2 s budget (V-114). A DSN that sets jit keeps its own.
func sessionConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if _, set := cfg.RuntimeParams["jit"]; !set {
		cfg.RuntimeParams["jit"] = "off"
	}
	return cfg, nil
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
