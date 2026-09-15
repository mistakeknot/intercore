package db

import (
	"context"
	"database/sql/driver"
	"testing"
)

// Open enables foreign keys on its first connection. When the pool discards a
// bad connection and opens another, that one must enforce foreign keys too.
func TestForeignKeysSurviveReconnect(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	var on int
	if err := d.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys on the first connection = %d, want 1", on)
	}

	conn, err := d.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()

	if err := d.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys on a reconnected connection = %d, want 1", on)
	}
}
