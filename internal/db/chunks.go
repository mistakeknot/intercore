package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Chunk kinds in a chunked migration file.
const (
	chunkPlain = "plain" // whole file, no markers
	chunkAlter = "alter" // one ALTER TABLE ... ADD COLUMN, duplicate-tolerant
	chunkDDL   = "ddl"   // idempotent CREATE ... IF NOT EXISTS (also in schema.sql)
	chunkData  = "data"  // idempotent data statements
)

const chunkMarker = "-- ic:chunk "

type sqlChunk struct {
	Kind string
	SQL  string
}

// splitChunks splits a migration file on "-- ic:chunk <kind>" marker lines.
// A file with no markers is a single plain chunk.
func splitChunks(src string) []sqlChunk {
	if !strings.Contains(src, chunkMarker) {
		return []sqlChunk{{Kind: chunkPlain, SQL: src}}
	}
	var (
		chunks []sqlChunk
		kind   string
		buf    []string
	)
	flush := func() {
		if kind == "" {
			return
		}
		body := strings.TrimSpace(strings.Join(buf, "\n"))
		if body != "" {
			chunks = append(chunks, sqlChunk{Kind: kind, SQL: body})
		}
		buf = nil
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, chunkMarker) {
			flush()
			kind = strings.TrimSpace(strings.TrimPrefix(line, chunkMarker))
			continue
		}
		if kind != "" {
			buf = append(buf, line)
		}
	}
	flush()
	return chunks
}

const migration041File = "migrations/041_scheduler_runner.sql"

func migration041Chunks() ([]sqlChunk, error) {
	data, err := migrationsFS.ReadFile(migration041File)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migration041File, err)
	}
	return splitChunks(string(data)), nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// applyMigration041Alters adds the 041 columns to an existing scheduler_jobs.
// On a database old enough to have no scheduler_jobs the table is created with
// the columns by schema.sql, so there is nothing to alter.
func applyMigration041Alters(ctx context.Context, tx execer) error {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='scheduler_jobs'").Scan(&n); err != nil {
		return fmt.Errorf("migrate v40→v41: %w", err)
	}
	if n == 0 {
		return nil
	}
	chunks, err := migration041Chunks()
	if err != nil {
		return err
	}
	for _, c := range chunks {
		if c.Kind != chunkAlter {
			continue
		}
		if _, err := tx.ExecContext(ctx, c.SQL); err != nil && !isDuplicateColumnError(err) {
			return fmt.Errorf("migrate v40→v41: %w", err)
		}
	}
	return nil
}

// applyMigration041Data runs the data chunks (the ddl chunk is covered by
// schema.sql in DB.Migrate).
func applyMigration041Data(ctx context.Context, tx execer) error {
	chunks, err := migration041Chunks()
	if err != nil {
		return err
	}
	for _, c := range chunks {
		if c.Kind != chunkData {
			continue
		}
		if _, err := tx.ExecContext(ctx, c.SQL); err != nil {
			return fmt.Errorf("migrate v40→v41 data: %w", err)
		}
	}
	return nil
}
