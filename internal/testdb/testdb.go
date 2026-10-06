// Package testdb gives each test package its own database. go test runs
// packages in parallel, and every package truncates the tables it uses, so a
// shared database makes one package's reset another package's flake.
package testdb

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/store"
)

// Open skips the test when no Postgres is reachable, unless BENCHGRID_REQUIRE_DB
// is set, which CI sets so that a missing database fails instead of passing by
// skipping everything that matters.
func Open(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	base := os.Getenv("BENCHGRID_TEST_PG")
	if base == "" {
		base = "postgres:///postgres?sslmode=disable"
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		if os.Getenv("BENCHGRID_REQUIRE_DB") != "" {
			t.Fatalf("database required: %v", err)
		}
		t.Skipf("no postgres: %v", err)
	}
	dbname := "benchgrid_test_" + name
	var exists bool
	admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", dbname).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbname}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	admin.Close(ctx)

	u.Path = "/" + dbname
	pool, err := store.Open(ctx, u.String(), 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
