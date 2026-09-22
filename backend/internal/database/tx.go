package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of pgx that repository SQL actually uses. Both
// *pgxpool.Pool and pgx.Tx satisfy it, which is what lets one repository method
// run inside or outside a transaction without a second copy of every query.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RunInTx owns the begin/commit/rollback mechanics so no repository has to
// repeat them. Each feature package wraps this in its own InTx, which rebinds
// its repository to the transaction — that is what keeps pgx types from ever
// appearing in a service signature.
//
// The deferred Rollback is always armed. After a successful Commit it is a
// no-op, and before one it is the only thing standing between an early return
// and a transaction that stays open holding a pool connection hostage.
func RunInTx(ctx context.Context, pool *pgxpool.Pool, fn func(Querier) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
