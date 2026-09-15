package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// ImmediateTx is a BEGIN IMMEDIATE transaction on a held connection.
//
// modernc.org/sqlite ignores sql.TxOptions.Isolation, so BeginTx always issues a
// deferred BEGIN. A deferred transaction that reads and then writes fails with
// SQLITE_BUSY, without consulting the busy timeout, when another connection
// commits between the read and the write. An immediate transaction takes the
// write lock first, so a competing writer waits out the busy timeout instead.
type ImmediateTx struct {
	ctx  context.Context
	conn *sql.Conn
	done bool
}

// BeginImmediate holds a connection from sqlDB and begins an immediate
// transaction on it. Commit and Rollback both release the connection, so the
// pool's single connection is free again once either returns.
func BeginImmediate(ctx context.Context, sqlDB *sql.DB) (*ImmediateTx, error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin immediate: acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		return nil, fmt.Errorf("begin immediate: %w", err)
	}
	return &ImmediateTx{ctx: ctx, conn: conn}, nil
}

func (tx *ImmediateTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *ImmediateTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx *ImmediateTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

// Commit commits the transaction and releases its connection. A failed commit
// is rolled back.
func (tx *ImmediateTx) Commit() error {
	if tx.done {
		return sql.ErrTxDone
	}
	if _, err := tx.conn.ExecContext(tx.ctx, "COMMIT"); err != nil {
		tx.rollback()
		return fmt.Errorf("commit: %w", err)
	}
	tx.done = true
	return tx.conn.Close()
}

// Rollback rolls the transaction back and releases its connection. After Commit
// or an earlier Rollback it does nothing, so it is safe to defer.
func (tx *ImmediateTx) Rollback() error {
	if !tx.done {
		tx.rollback()
	}
	return nil
}

func (tx *ImmediateTx) rollback() {
	tx.done = true
	// The request may already be cancelled; never return a connection with an
	// open transaction to the pool if cleanup itself fails.
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tx.conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
		_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	tx.conn.Close()
}
