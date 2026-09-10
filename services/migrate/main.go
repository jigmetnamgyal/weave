// Command migrate applies and rolls back database migrations.
//
// It uses goose as a library rather than the goose CLI on purpose. The CLI
// binary links every database driver goose supports — ClickHouse, YDB, MySQL,
// libsql and more — which is a large build and a large dependency surface for
// a project that speaks only PostgreSQL. Driving it from here reuses the pgx
// driver the API already depends on and nothing else.
//
// Migrations run as their own step before the application rolls out, never
// implicitly at API startup: an application that migrates on boot turns a
// routine restart into a schema change, and races when several instances
// start at once.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	weavedb "github.com/jigmetnamgyal/weave/db"
)

const connectTimeout = 15 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "weave-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: migrate <up|down|status|version> (reads DATABASE_URL)")
	}
	command := args[0]

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set. Copy .env.example to .env, or export it in your shell")
	}

	db, err := openDB(databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "weave-migrate: close database: %v\n", err)
		}
	}()

	goose.SetBaseFS(weavedb.MigrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch command {
	case "up":
		err = goose.UpContext(ctx, db, weavedb.MigrationsDir)
	case "down":
		err = goose.DownContext(ctx, db, weavedb.MigrationsDir)
	case "status":
		err = goose.StatusContext(ctx, db, weavedb.MigrationsDir)
	case "version":
		err = goose.VersionContext(ctx, db, weavedb.MigrationsDir)
	default:
		return fmt.Errorf("unknown command %q: expected up, down, status or version", command)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}

	return nil
}

// openDB opens a database/sql handle over pgx and verifies it is reachable,
// so a bad URL fails here with a clear message rather than inside goose.
func openDB(databaseURL string) (*sql.DB, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	db := stdlib.OpenDB(*config)

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to database: %w (is `make up` running?)", err)
	}

	return db, nil
}
