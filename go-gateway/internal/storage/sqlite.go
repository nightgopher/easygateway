package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nightgopher/easygateway/internal/metrics"
)

// SQLiteStore SQLite 持久化存储
type SQLiteStore struct {
	db     *sql.DB
	logger *log.Logger
}

// NewSQLiteStore 创建 SQLite 存储
func NewSQLiteStore(dbPath string, logger *log.Logger) (*SQLiteStore, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	// Metrics and conversation archives share one SQLite file but are written
	// by independent goroutines. A connection pool larger than 1 lets those
	// writers race for the write lock; a long write (cleanup DELETE, large
	// archive save) then exhausts busy_timeout and silently drops metric
	// records. Converge on a single connection so writers serialize in Go
	// instead of failing in SQLite; WAL keeps readers unaffected.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout%3d5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// 启用 WAL 模式提高并发性能
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set wal mode: %w", err)
	}

	// 创建表
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// 归档表（conversation_archives）与 request_logs 物理隔离
	if err := initArchiveSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init archive schema: %w", err)
	}

	// 记忆表（agent_memories）同样物理隔离：记忆长期存活，保留策略独立。
	if err := initMemorySchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init memory schema: %w", err)
	}

	// 助理人设与反馈表（agent_settings / assistant_feedback）。
	if err := initAgentSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init agent schema: %w", err)
	}

	logger.Printf("SQLite store initialized: %s", dbPath)

	return &SQLiteStore{db: db, logger: logger}, nil
}

// initSchema 初始化数据库表结构
func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		request_id TEXT DEFAULT '',
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		model TEXT DEFAULT '',
		provider TEXT DEFAULT '',
		provider_attempts TEXT DEFAULT '[]',
		status_code INTEGER DEFAULT 0,
		latency_ms REAL DEFAULT 0,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0,
		cache_creation_input_tokens INTEGER DEFAULT 0,
		cache_input_tokens INTEGER DEFAULT 0,
		cache_usage_known BOOLEAN DEFAULT 0,
		is_stream BOOLEAN DEFAULT 0,
		error TEXT DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
	CREATE INDEX IF NOT EXISTS idx_logs_provider ON request_logs(provider);

	CREATE TABLE IF NOT EXISTS daily_stats (
		date TEXT NOT NULL,
		model TEXT NOT NULL,
		provider TEXT NOT NULL DEFAULT '',
		requests INTEGER DEFAULT 0,
		successes INTEGER DEFAULT 0,
		errors INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		total_latency_ms REAL DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0,
		cache_creation_input_tokens INTEGER DEFAULT 0,
		cache_input_tokens INTEGER DEFAULT 0,
		cache_usage_requests INTEGER DEFAULT 0,
		PRIMARY KEY (date, model, provider)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Existing installations predate request correlation metadata. CREATE TABLE
	// IF NOT EXISTS does not alter them, so add the new columns idempotently.
	if err := ensureColumn(db, "request_logs", "request_id", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "request_logs", "provider_attempts", "TEXT DEFAULT '[]'"); err != nil {
		return err
	}
	for _, column := range []struct {
		table, name, definition string
	}{
		{"request_logs", "cache_read_input_tokens", "INTEGER DEFAULT 0"},
		{"request_logs", "cache_creation_input_tokens", "INTEGER DEFAULT 0"},
		{"request_logs", "cache_input_tokens", "INTEGER DEFAULT 0"},
		{"request_logs", "cache_usage_known", "BOOLEAN DEFAULT 0"},
		{"daily_stats", "cache_read_input_tokens", "INTEGER DEFAULT 0"},
		{"daily_stats", "cache_creation_input_tokens", "INTEGER DEFAULT 0"},
		{"daily_stats", "cache_input_tokens", "INTEGER DEFAULT 0"},
		{"daily_stats", "cache_usage_requests", "INTEGER DEFAULT 0"},
	} {
		if err := ensureColumn(db, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	_, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_logs_request_id ON request_logs(request_id)")
	if err != nil {
		return err
	}
	return migrateDailyStatsPrimaryKey(db)
}

// migrateDailyStatsPrimaryKey 把历史库的 daily_stats 主键从 (date, model)
// 重建为 (date, model, provider)。旧主键下一个模型一天只记首个写入的渠道，
// fallback 换渠道后维度归属不准，渠道级统计无法做。行数据保持不变，一次迁移。
func migrateDailyStatsPrimaryKey(db *sql.DB) error {
	var createSQL string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'daily_stats'`).Scan(&createSQL)
	if err != nil {
		return err
	}
	if strings.Contains(createSQL, "PRIMARY KEY (date, model, provider)") {
		return nil // 已是新结构
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE daily_stats_v2 (
			date TEXT NOT NULL,
			model TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			requests INTEGER DEFAULT 0,
			successes INTEGER DEFAULT 0,
			errors INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			total_latency_ms REAL DEFAULT 0,
			cache_read_input_tokens INTEGER DEFAULT 0,
			cache_creation_input_tokens INTEGER DEFAULT 0,
			cache_input_tokens INTEGER DEFAULT 0,
			cache_usage_requests INTEGER DEFAULT 0,
			PRIMARY KEY (date, model, provider)
		)`,
		`INSERT OR IGNORE INTO daily_stats_v2 (date, model, provider, requests, successes, errors, total_tokens, total_latency_ms, cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_requests)
		 SELECT date, model, provider, requests, successes, errors, total_tokens, total_latency_ms, cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_requests FROM daily_stats`,
		`DROP TABLE daily_stats`,
		`ALTER TABLE daily_stats_v2 RENAME TO daily_stats`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate daily_stats primary key: %w", err)
		}
	}
	return tx.Commit()
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

// SaveRecord 保存一条请求记录
func (s *SQLiteStore) SaveRecord(r metrics.RequestRecord) error {
	return s.SaveRecordContext(context.Background(), r)
}

func (s *SQLiteStore) SaveRecordContext(ctx context.Context, r metrics.RequestRecord) error {
	attempts, err := json.Marshal(r.ProviderAttempts)
	if err != nil {
		return fmt.Errorf("encode provider attempts: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO request_logs (timestamp, request_id, method, path, model, provider, provider_attempts, status_code, latency_ms, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_known, is_stream, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.Timestamp, r.RequestID, r.Method, r.Path, r.Model, r.Provider, string(attempts), r.StatusCode, r.Latency, r.InputTokens, r.OutputTokens, r.CacheReadInputTokens, r.CacheCreationInputTokens, r.CacheInputTokens, r.CacheUsageKnown, r.IsStream, r.Error)

	if err != nil {
		s.logger.Printf("Save record error: %v", err)
		return err
	}

	// 更新每日统计
	date := r.Timestamp.Format("2006-01-02")
	success := 0
	if metrics.RequestSucceeded(r) {
		success = 1
	}
	totalInput := r.InputTokens
	if r.CacheUsageKnown && r.CacheInputTokens > totalInput {
		totalInput = r.CacheInputTokens
	}
	tokens := totalInput + r.OutputTokens
	cacheUsageRequests := 0
	if r.CacheUsageKnown {
		cacheUsageRequests = 1
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO daily_stats (date, model, provider, requests, successes, errors, total_tokens, total_latency_ms, cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_requests)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(date, model, provider) DO UPDATE SET
			requests = requests + 1,
			successes = successes + ?,
			errors = errors + ?,
			total_tokens = total_tokens + ?,
			total_latency_ms = total_latency_ms + ?,
			cache_read_input_tokens = cache_read_input_tokens + ?,
			cache_creation_input_tokens = cache_creation_input_tokens + ?,
			cache_input_tokens = cache_input_tokens + ?,
			cache_usage_requests = cache_usage_requests + ?
	`, date, r.Model, r.Provider, success, 1-success, tokens, r.Latency,
		r.CacheReadInputTokens, r.CacheCreationInputTokens, r.CacheInputTokens, cacheUsageRequests,
		success, 1-success, tokens, r.Latency, r.CacheReadInputTokens, r.CacheCreationInputTokens, r.CacheInputTokens, cacheUsageRequests)

	if err != nil {
		return err
	}
	return tx.Commit()
}

// GetRecentLogs 获取最近的请求日志
func (s *SQLiteStore) GetRecentLogs(limit int) ([]metrics.RequestRecord, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, request_id, method, path, model, provider, provider_attempts, status_code, latency_ms, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_known, is_stream, error
		FROM request_logs
		ORDER BY timestamp DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []metrics.RequestRecord
	for rows.Next() {
		var r metrics.RequestRecord
		var ts string
		var attemptsJSON string
		if err := rows.Scan(&ts, &r.RequestID, &r.Method, &r.Path, &r.Model, &r.Provider, &attemptsJSON, &r.StatusCode, &r.Latency, &r.InputTokens, &r.OutputTokens, &r.CacheReadInputTokens, &r.CacheCreationInputTokens, &r.CacheInputTokens, &r.CacheUsageKnown, &r.IsStream, &r.Error); err != nil {
			continue
		}
		r.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		if attemptsJSON != "" {
			_ = json.Unmarshal([]byte(attemptsJSON), &r.ProviderAttempts)
		}
		records = append(records, r)
	}
	return records, nil
}

// GetDailyStats 获取指定日期按模型聚合的统计（供重启恢复今日聚合用）。
// 新主键下同一模型可能有多行（不同渠道），这里在 SQL 层按模型合并。
func (s *SQLiteStore) GetDailyStats(date string) ([]metrics.ModelStats, error) {
	rows, err := s.db.Query(`
		SELECT model, MAX(provider), SUM(requests), SUM(successes), SUM(errors), SUM(total_tokens),
			SUM(cache_read_input_tokens), SUM(cache_creation_input_tokens), SUM(cache_input_tokens), SUM(cache_usage_requests),
			CASE WHEN SUM(requests) > 0 THEN SUM(total_latency_ms) / SUM(requests) ELSE 0 END as avg_latency
		FROM daily_stats
		WHERE date = ?
		GROUP BY model
		ORDER BY SUM(requests) DESC
	`, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []metrics.ModelStats
	for rows.Next() {
		var ms metrics.ModelStats
		if err := rows.Scan(&ms.Model, &ms.Provider, &ms.Requests, &ms.Successes, &ms.Errors, &ms.TotalTokens, &ms.CacheReadInputTokens, &ms.CacheCreationInputTokens, &ms.CacheInputTokens, &ms.CacheUsageRequests, &ms.AvgLatency); err != nil {
			continue
		}
		stats = append(stats, ms)
	}
	return stats, nil
}

// GetDailyStatRows 获取 [from, to]（闭区间，YYYY-MM-DD）的按天 × 模型 × 渠道
// 聚合行，供历史统计聚合。
func (s *SQLiteStore) GetDailyStatRows(from, to string) ([]metrics.DailyStatRow, error) {
	rows, err := s.db.Query(`
		SELECT date, model, provider, requests, successes, errors, total_tokens, total_latency_ms,
			cache_read_input_tokens, cache_creation_input_tokens, cache_input_tokens, cache_usage_requests
		FROM daily_stats
		WHERE date >= ? AND date <= ?
		ORDER BY date, model
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []metrics.DailyStatRow
	for rows.Next() {
		var r metrics.DailyStatRow
		if err := rows.Scan(&r.Date, &r.Model, &r.Provider, &r.Requests, &r.Successes, &r.Errors, &r.TotalTokens, &r.TotalLatencyMS, &r.CacheReadInputTokens, &r.CacheCreationInputTokens, &r.CacheInputTokens, &r.CacheUsageRequests); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetTotalRequests 获取今日总请求数
func (s *SQLiteStore) GetTotalRequests(date string) (int, error) {
	var total int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(requests), 0) FROM daily_stats WHERE date = ?`, date).Scan(&total)
	return total, err
}

// Cleanup 清理旧数据：request_logs 保留 logDays 天，daily_stats 保留
// statsDays 天（按天聚合行小而价值随时间增长，保留期应远长于原始日志）。
// DELETE 按批执行，避免单条长写事务在单连接下阻塞其他写入方过久。
func (s *SQLiteStore) Cleanup(logDays, statsDays int) error {
	const batchSize = 500
	var totalLogs, totalStats int64
	logCutoff := time.Now().AddDate(0, 0, -logDays).Format("2006-01-02")
	for {
		result, err := s.db.Exec(`DELETE FROM request_logs WHERE rowid IN (SELECT rowid FROM request_logs WHERE timestamp < ? LIMIT ?)`, logCutoff, batchSize)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		totalLogs += n
		if n < batchSize {
			break
		}
	}
	if totalLogs > 0 {
		s.logger.Printf("Cleaned up %d old log records", totalLogs)
	}

	statsCutoff := time.Now().AddDate(0, 0, -statsDays).Format("2006-01-02")
	for {
		result, err := s.db.Exec(`DELETE FROM daily_stats WHERE rowid IN (SELECT rowid FROM daily_stats WHERE date < ? LIMIT ?)`, statsCutoff, batchSize)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		totalStats += n
		if n < batchSize {
			break
		}
	}
	if totalStats > 0 {
		s.logger.Printf("Cleaned up %d old daily stats", totalStats)
	}
	return nil
}

// Close 关闭数据库连接
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
