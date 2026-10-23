package storage

import (
	"database/sql"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/weijian/go-llm-gateway/internal/metrics"
)

func logDiscard() *log.Logger { return log.New(io.Discard, "", 0) }

// TestDailyStatsMigratesLegacyPrimaryKey 用旧 (date, model) 主键建库，
// 验证启动迁移后数据保留且新主键支持同模型多渠道分行。
func TestDailyStatsMigratesLegacyPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE daily_stats (
		date TEXT NOT NULL, model TEXT NOT NULL, provider TEXT DEFAULT '',
		requests INTEGER DEFAULT 0, successes INTEGER DEFAULT 0, errors INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0, total_latency_ms REAL DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0, cache_creation_input_tokens INTEGER DEFAULT 0,
		cache_input_tokens INTEGER DEFAULT 0, cache_usage_requests INTEGER DEFAULT 0,
		PRIMARY KEY (date, model))`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO daily_stats (date, model, provider, requests, total_tokens)
		VALUES ('2026-10-01', 'glm-4.7', 'glm', 7, 700)`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	db.Close()

	store, err := NewSQLiteStore(path, logDiscard())
	if err != nil {
		t.Fatalf("migrate on open: %v", err)
	}
	defer store.Close()

	rows, err := store.GetDailyStatRows("2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatalf("GetDailyStatRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 7 || rows[0].Provider != "glm" {
		t.Fatalf("legacy row lost after migration: %+v", rows)
	}

	// 同模型不同渠道在新主键下应是独立两行。
	rec := metrics.RequestRecord{Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local),
		Method: "POST", Path: "/v1/messages", Model: "glm-4.7", Provider: "backup", StatusCode: 200, Latency: 10}
	if err := store.SaveRecord(rec); err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}
	rows, err = store.GetDailyStatRows("2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatalf("GetDailyStatRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (glm + backup), got %+v", rows)
	}
}

// TestGetDailyStatsAggregatesAcrossProviders 验证重启恢复用的单日查询在
// 新主键下按模型合并多渠道行。
func TestGetDailyStatsAggregatesAcrossProviders(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), logDiscard())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	base := metrics.RequestRecord{Timestamp: time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local),
		Method: "POST", Path: "/v1/messages", Model: "glm-4.7", StatusCode: 200, Latency: 20, InputTokens: 5, OutputTokens: 5}
	for _, provider := range []string{"glm", "glm", "backup"} {
		base.Provider = provider
		if err := store.SaveRecord(base); err != nil {
			t.Fatalf("SaveRecord: %v", err)
		}
	}
	stats, err := store.GetDailyStats("2026-10-02")
	if err != nil {
		t.Fatalf("GetDailyStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("model rows should merge across providers, got %+v", stats)
	}
	if stats[0].Requests != 3 || stats[0].TotalTokens != 30 {
		t.Errorf("merged totals wrong: %+v", stats[0])
	}
}

// TestCleanupSplitsRetention 验证日志与按天聚合使用不同保留窗口。
func TestCleanupSplitsRetention(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), logDiscard())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	old := metrics.RequestRecord{Timestamp: time.Now().AddDate(0, 0, -40), Method: "POST", Path: "/v1/messages",
		Model: "glm-4.7", Provider: "glm", StatusCode: 200, Latency: 5, InputTokens: 1, OutputTokens: 1}
	if err := store.SaveRecord(old); err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}

	// 日志保 30 天（40 天前的应删），聚合保 365 天（应留）。
	if err := store.Cleanup(30, 365); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	rows, err := store.GetDailyStatRows("2000-01-01", "2100-01-01")
	if err != nil {
		t.Fatalf("GetDailyStatRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("daily_stats older than log retention must survive, got %+v", rows)
	}
	logs, err := store.GetRecentLogs(10)
	if err != nil {
		t.Fatalf("GetRecentLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("request_logs beyond 30d should be deleted, got %d", len(logs))
	}
}
