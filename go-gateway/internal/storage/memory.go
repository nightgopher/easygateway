package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/nightgopher/easygateway/internal/memory"
)

// initMemorySchema creates the agent_memories table. Physically separate
// from request_logs/conversation_archives so retention policies stay
// independent: memories are long-lived by design, logs/archives are not.
func initMemorySchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS agent_memories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope_type TEXT NOT NULL DEFAULT 'global',
		scope_key TEXT NOT NULL DEFAULT '',
		statement TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'candidate',
		source TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0.5,
		hit_count INTEGER NOT NULL DEFAULT 0,
		last_hit_at DATETIME,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE (scope_type, scope_key, statement)
	);

	CREATE INDEX IF NOT EXISTS idx_memories_scope
		ON agent_memories(scope_type, scope_key, status);
	CREATE INDEX IF NOT EXISTS idx_memories_status
		ON agent_memories(status, confidence DESC, updated_at DESC);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	return nil
}

// LookupFor merges three scope rings (global ⊕ client ⊕ project) of active
// memories. 全局条目排后面：越窄的作用域越相关（与 AGENTS.md 的目录作用域
// 同构）。命中计数在同一连接上同步累加——SQLite 本地写，量级可忽略。
func (s *SQLiteStore) LookupFor(client, project string, limit int) ([]memory.Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT id, scope_type, scope_key, statement, status, source,
		       confidence, hit_count, last_hit_at, created_at, updated_at
		  FROM agent_memories
		 WHERE status = 'active'
		   AND (
		     (scope_type = 'global')
		     OR (scope_type = 'client' AND scope_key = ?)
		     OR (scope_type = 'project' AND scope_key = ?)
		   )
		 ORDER BY CASE scope_type WHEN 'project' THEN 0 WHEN 'client' THEN 1 ELSE 2 END,
		          confidence DESC, updated_at DESC
		 LIMIT ?`, client, project, limit)
	if err != nil {
		return nil, fmt.Errorf("lookup memories: %w", err)
	}
	defer rows.Close()

	var (
		result []memory.Memory
		ids    []int64
	)
	for rows.Next() {
		m, scanErr := scanMemory(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := s.MarkHits(ids); err != nil {
			// 命中计数是观测信号而非正确性路径，失败只记日志。
			s.logger.Printf("Warning: mark memory hits failed: %v", err)
		}
	}
	return result, nil
}

// Insert stores a new memory; the UNIQUE(scope_type, scope_key, statement)
// constraint absorbs duplicates by refreshing the existing row instead.
// Returns (row, inserted) — inserted=false means the duplicate was refreshed.
func (s *SQLiteStore) Insert(m memory.Memory) (memory.Memory, bool, error) {
	now := time.Now().UTC()
	if m.Status == "" {
		m.Status = memory.StatusCandidate
	}
	if m.Confidence <= 0 {
		m.Confidence = 0.5
	}
	// 先查重：同作用域同命题刷新时间与来源，不重复入库。
	if existing, err := s.FindDuplicate(string(m.ScopeType), m.ScopeKey, m.Statement); err != nil {
		return memory.Memory{}, false, err
	} else if existing != nil {
		if _, err := s.db.Exec(
			"UPDATE agent_memories SET updated_at = ?, source = ? WHERE id = ?",
			now, m.Source, existing.ID); err != nil {
			return memory.Memory{}, false, err
		}
		refreshed, err := s.Get(existing.ID)
		if err != nil || refreshed == nil {
			return memory.Memory{}, false, err
		}
		return *refreshed, false, nil
	}

	res, err := s.db.Exec(`
		INSERT INTO agent_memories
			(scope_type, scope_key, statement, status, source, confidence,
			 hit_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		string(m.ScopeType), m.ScopeKey, m.Statement, string(m.Status),
		m.Source, m.Confidence, now, now)
	if err != nil {
		// 并发窗口下唯一约束兜底：转为刷新既有行。
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			if existing, dupErr := s.FindDuplicate(string(m.ScopeType), m.ScopeKey, m.Statement); dupErr == nil && existing != nil {
				return *existing, false, nil
			}
		}
		return memory.Memory{}, false, fmt.Errorf("insert memory: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return memory.Memory{}, false, err
	}
	m.ID = id
	m.CreatedAt = now
	m.UpdatedAt = now
	return m, true, nil
}

// FindDuplicate locates an existing row with the same scope + statement.
func (s *SQLiteStore) FindDuplicate(scopeType, scopeKey, statement string) (*memory.Memory, error) {
	row := s.db.QueryRow(`
		SELECT id, scope_type, scope_key, statement, status, source,
		       confidence, hit_count, last_hit_at, created_at, updated_at
		  FROM agent_memories
		 WHERE scope_type = ? AND scope_key = ? AND statement = ?`,
		scopeType, scopeKey, statement)
	m, err := scanMemory(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Get fetches one memory by id; returns nil when absent.
func (s *SQLiteStore) Get(id int64) (*memory.Memory, error) {
	row := s.db.QueryRow(`
		SELECT id, scope_type, scope_key, statement, status, source,
		       confidence, hit_count, last_hit_at, created_at, updated_at
		  FROM agent_memories WHERE id = ?`, id)
	m, err := scanMemory(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// List returns a filtered admin listing, newest-updated first.
func (s *SQLiteStore) List(filter memory.ListFilter) ([]memory.Memory, int, error) {
	var (
		conds []string
		args  []any
	)
	if filter.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.ScopeType != "" {
		conds = append(conds, "scope_type = ?")
		args = append(args, string(filter.ScopeType))
	}
	if filter.ScopeKey != "" {
		conds = append(conds, "scope_key = ?")
		args = append(args, filter.ScopeKey)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM agent_memories"+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`
		SELECT id, scope_type, scope_key, statement, status, source,
		       confidence, hit_count, last_hit_at, created_at, updated_at
		  FROM agent_memories%s
		 ORDER BY updated_at DESC
		 LIMIT ? OFFSET ?`, where)
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []memory.Memory
	for rows.Next() {
		m, scanErr := scanMemory(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		result = append(result, m)
	}
	return result, total, rows.Err()
}

// SetStatus transitions governance status (candidate→active 确认, →retired 退役).
func (s *SQLiteStore) SetStatus(id int64, status memory.Status) (bool, error) {
	res, err := s.db.Exec(
		"UPDATE agent_memories SET status = ?, updated_at = ? WHERE id = ?",
		string(status), time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// Delete removes one entry entirely.
func (s *SQLiteStore) Delete(id int64) (bool, error) {
	res, err := s.db.Exec("DELETE FROM agent_memories WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// MarkHits increments retrieval counters for the given entries.
func (s *SQLiteStore) MarkHits(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	query := fmt.Sprintf(
		"UPDATE agent_memories SET hit_count = hit_count + 1, last_hit_at = ? WHERE id IN (%s)",
		placeholders)
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().UTC())
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(query, args...)
	return err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanMemory(row rowScanner) (memory.Memory, error) {
	var (
		m         memory.Memory
		scopeKey  string
		source    string
		lastHitAt sql.NullTime
	)
	err := row.Scan(&m.ID, &m.ScopeType, &scopeKey, &m.Statement, &m.Status,
		&source, &m.Confidence, &m.HitCount, &lastHitAt, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return memory.Memory{}, err
	}
	m.ScopeKey = scopeKey
	m.Source = source
	if lastHitAt.Valid {
		t := lastHitAt.Time
		m.LastHitAt = &t
	}
	return m, nil
}
