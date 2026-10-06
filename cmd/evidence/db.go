package main

import (
	"context"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/store"
)

// freshDB drops and recreates a database for one run, so no run inherits a
// fence or a lease from the one before it.
func (h *harness) freshDB(ctx context.Context, name string) (*pgxpool.Pool, string, error) {
	admin, err := pgx.Connect(ctx, h.pgBase)
	if err != nil {
		return nil, "", err
	}
	defer admin.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return nil, "", err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(h.pgBase)
	u.Path = "/" + name
	pool, err := store.Open(ctx, u.String(), 100)
	if err != nil {
		return nil, "", err
	}
	return pool, u.String(), store.Migrate(ctx, pool)
}
