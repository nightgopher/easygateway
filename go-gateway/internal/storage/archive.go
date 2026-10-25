package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/nightgopher/easygateway/internal/archive"
)

// initArchiveSchema creates the conversation_archives table. It is physically
// separate from request_logs so archives never contend with metrics writes and
// the two can be backed up / purged independently.
func initArchiveSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS conversation_archives (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		request_id TEXT DEFAULT '',
		protocol TEXT DEFAULT '',
		model TEXT DEFAULT '',
		provider TEXT DEFAULT '',
		is_stream BOOLEAN DEFAULT 0,
		status TEXT DEFAULT '',
		status_code INTEGER DEFAULT 0,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		request_body TEXT DEFAULT '',
		response_body TEXT DEFAULT '',
		error_reason TEXT DEFAULT '',
		source TEXT DEFAULT '',
		conversation_id TEXT DEFAULT '',
		session_id TEXT DEFAULT '',
		request_bytes INTEGER DEFAULT 0,
		response_bytes INTEGER DEFAULT 0,
		truncated BOOLEAN DEFAULT 0,
		schema_version INTEGER DEFAULT 1
	);

	CREATE INDEX IF NOT EXISTS idx_archives_timestamp ON conversation_archives(timestamp);
	CREATE INDEX IF NOT EXISTS idx_archives_request_id ON conversation_archives(request_id);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Existing installations created by schema version 1 need additive
	// migrations. PRAGMA keeps this idempotent without relying on SQLite error
	// string matching.
	for _, column := range []struct {
		name string
		def  string
	}{
		{name: "source", def: "TEXT DEFAULT ''"},
		{name: "conversation_id", def: "TEXT DEFAULT ''"},
		{name: "session_id", def: "TEXT DEFAULT ''"},
		{name: "request_bytes", def: "INTEGER DEFAULT 0"},
		{name: "response_bytes", def: "INTEGER DEFAULT 0"},
		{name: "truncated", def: "BOOLEAN DEFAULT 0"},
	} {
		if err := ensureArchiveColumn(db, column.name, column.def); err != nil {
			return err
		}
	}
	return nil
}

// SaveArchive persists a single conversation archive.
func (s *SQLiteStore) SaveArchive(a archive.Archive) error {
	_, err := s.db.Exec(`
		INSERT INTO conversation_archives
			(timestamp, request_id, protocol, model, provider, is_stream,
			 status, status_code, input_tokens, output_tokens,
			 request_body, response_body, error_reason, source, conversation_id,
			 session_id, request_bytes, response_bytes, truncated, schema_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.Timestamp.UTC(), a.RequestID, string(a.Protocol), a.Model, a.Provider, a.IsStream,
		string(a.Status), a.StatusCode, a.InputTokens, a.OutputTokens,
		a.RequestBody, a.ResponseBody, a.ErrorReason, a.Source, a.ConversationID,
		a.SessionID, a.RequestBytes, a.ResponseBytes, a.Truncated, a.SchemaVersion,
	)
	if err != nil {
		s.logger.Printf("SaveArchive error: %v", err)
	}
	return err
}

// QueryArchives returns a page of archives ordered newest-first, plus the total
// row count for pagination. Used by the admin list endpoint.
func (s *SQLiteStore) QueryArchives(limit, offset int) ([]archive.Archive, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversation_archives`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(`
		SELECT id, timestamp, request_id, protocol, model, provider, is_stream,
		       status, status_code, input_tokens, output_tokens,
		       request_body, response_body, error_reason, source, conversation_id,
		       session_id, request_bytes, response_bytes, truncated, schema_version
		FROM conversation_archives
		ORDER BY timestamp DESC, id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	records, err := scanArchives(rows)
	if err != nil {
		return nil, 0, err
	}
	return records, total, nil
}

// ExportArchives returns archives strictly newer than the (sinceID, sinceTime)
// cursor, ordered oldest-first for deterministic incremental export. The
// composite cursor guarantees no duplicates and no skipped rows even if two
// archives share the same timestamp: rows are ordered by (timestamp, id) and
// the cursor filters with "timestamp > sinceTime OR (timestamp = sinceTime AND
// id > sinceID)".
func (s *SQLiteStore) ExportArchives(sinceID int64, sinceTime time.Time, limit int) ([]archive.Archive, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	rows, err := s.db.Query(`
		SELECT id, timestamp, request_id, protocol, model, provider, is_stream,
		       status, status_code, input_tokens, output_tokens,
		       request_body, response_body, error_reason, source, conversation_id,
		       session_id, request_bytes, response_bytes, truncated, schema_version
		FROM conversation_archives
		WHERE timestamp > ? OR (timestamp = ? AND id > ?)
		ORDER BY timestamp ASC, id ASC
		LIMIT ?
	`, sinceTime, sinceTime, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanArchives(rows)
}

// DeleteArchivesBefore purges all archives with timestamps older than cutoff.
// Returns the number of rows deleted.
func (s *SQLiteStore) DeleteArchivesBefore(cutoff time.Time) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM conversation_archives WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// DeleteArchive removes a single archive by ID.
func (s *SQLiteStore) DeleteArchive(id int64) error {
	_, err := s.db.Exec(`DELETE FROM conversation_archives WHERE id = ?`, id)
	return err
}

// CleanupArchives is a convenience wrapper used by the periodic cleanup task.
func (s *SQLiteStore) CleanupArchives(days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	n, err := s.DeleteArchivesBefore(cutoff)
	if err == nil && n > 0 {
		s.logger.Printf("Cleaned up %d old archives", n)
	}
	return err
}

func scanArchives(rows *sql.Rows) ([]archive.Archive, error) {
	var records []archive.Archive
	for rows.Next() {
		var a archive.Archive
		var ts string
		var protocol, status string
		if err := rows.Scan(
			&a.ID, &ts, &a.RequestID, &protocol, &a.Model, &a.Provider, &a.IsStream,
			&status, &a.StatusCode, &a.InputTokens, &a.OutputTokens,
			&a.RequestBody, &a.ResponseBody, &a.ErrorReason, &a.Source,
			&a.ConversationID, &a.SessionID, &a.RequestBytes, &a.ResponseBytes,
			&a.Truncated, &a.SchemaVersion,
		); err != nil {
			continue
		}
		a.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		if a.Timestamp.IsZero() {
			a.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		}
		a.Protocol = archive.Protocol(protocol)
		a.Status = archive.Status(status)
		records = append(records, a)
	}
	return records, rows.Err()
}

func ensureArchiveColumn(db *sql.DB, name, definition string) error {
	rows, err := db.Query("PRAGMA table_info(conversation_archives)")
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		cid       int
		column    string
		columnTyp string
		notNull   int
		defaultV  sql.NullString
		primary   int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &column, &columnTyp, &notNull, &defaultV, &primary); err != nil {
			return err
		}
		if column == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE conversation_archives ADD COLUMN " + name + " " + definition)
	return err
}

// ensureArchiveSchema is called from NewSQLiteStore to create the table
// alongside the metrics schema, so a single migration point covers both.
func ensureArchiveSchema(db *sql.DB) error {
	if err := initArchiveSchema(db); err != nil {
		return fmt.Errorf("init archive schema: %w", err)
	}
	return nil
}
