package console

type Summary struct {
	TodayRequests int      `json:"today_requests"`
	TodayTokens   int64    `json:"today_tokens"`
	SuccessRate   float64  `json:"success_rate"`
	ActiveModels  int      `json:"active_models"`
	AvgLatency    float64  `json:"avg_latency_ms"`
	Uptime        string   `json:"uptime"`
	CacheHitRate  *float64 `json:"cache_hit_rate"`
	CacheRead     int64    `json:"cache_read_input_tokens"`
	CacheCreated  int64    `json:"cache_creation_input_tokens"`
}
type Provider struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	State       string  `json:"state"`
	Enabled     *bool   `json:"enabled"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	AvgLatency  float64 `json:"avg_latency"`
	HasProbe    bool    `json:"has_probe"`
	ProbeStatus string  `json:"probe_status"`
	ProbeDetail string  `json:"probe_detail"`
	LastProbe   string  `json:"last_probe_at"`
	NextRetry   string  `json:"next_retry_at"`
}
type Model struct {
	Health       ModelHealth `json:"health"`
	Name         string      `json:"model"`
	Provider     string      `json:"provider"`
	Providers    []string    `json:"providers"`
	Status       string      `json:"status"`
	Requests     int         `json:"requests"`
	Tokens       int64       `json:"total_tokens"`
	AvgLatency   float64     `json:"avg_latency"`
	Capabilities []string    `json:"capabilities"`
	Modalities   []string    `json:"input_modalities"`
	CacheHitRate *float64    `json:"cache_hit_rate"`
}

type ModelHealth struct {
	Status         string `json:"status"`
	Available      bool   `json:"available"`
	Reason         string `json:"reason"`
	LastProbeAt    string `json:"last_probe_at"`
	ProbeExpiresAt string `json:"probe_expires_at"`
}
type Dashboard struct {
	Summary   Summary    `json:"summary"`
	Providers []Provider `json:"providers"`
	Models    []Model    `json:"models"`
}

// StatsHistory 对应网关 GET /admin/stats/history：按天序列 + 模型/渠道排行。
type StatsHistory struct {
	From      string            `json:"from"`
	To        string            `json:"to"`
	Days      []HistoryDay      `json:"days"`
	Models    []HistoryModel    `json:"models"`
	Providers []HistoryProvider `json:"providers"`
}
type HistoryDay struct {
	Date         string   `json:"date"`
	Requests     int      `json:"requests"`
	Successes    int      `json:"successes"`
	Errors       int      `json:"errors"`
	TotalTokens  int64    `json:"total_tokens"`
	AvgLatencyMS float64  `json:"avg_latency_ms"`
	SuccessRate  float64  `json:"success_rate"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
}
type HistoryModel struct {
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}
type HistoryProvider struct {
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}
type Route struct {
	Model     string `json:"model"`
	Providers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Status  string `json:"status"`
	} `json:"providers"`
}
type Attempt struct {
	Provider string  `json:"provider"`
	Status   string  `json:"status"`
	Code     int     `json:"status_code"`
	Latency  float64 `json:"latency_ms"`
	Error    string  `json:"error"`
}
type Log struct {
	Timestamp    string    `json:"timestamp"`
	RequestID    string    `json:"request_id"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Model        string    `json:"model"`
	Provider     string    `json:"provider"`
	Code         int       `json:"status_code"`
	Latency      float64   `json:"latency_ms"`
	Input        int64     `json:"input_tokens"`
	Output       int64     `json:"output_tokens"`
	CacheRead    int64     `json:"cache_read_input_tokens"`
	CacheCreated int64     `json:"cache_creation_input_tokens"`
	Stream       bool      `json:"is_stream"`
	Error        string    `json:"error"`
	Attempts     []Attempt `json:"provider_attempts"`
}
type ClientConfig struct {
	Path    string `json:"path"`
	Exists  bool   `json:"file_exists"`
	InSync  bool   `json:"in_sync"`
	Desired []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"desired"`
	Current        []string `json:"current_ids"`
	Missing        []string `json:"missing_ids"`
	Stale          []string `json:"stale_ids"`
	Skipped        []string `json:"skipped_entries"`
	MissingEntries []string `json:"missing_entries"`
	Error          string   `json:"error"`
}

// CCSwitchCandidate 是 cc-switch 迁移扫描里的一条候选（网关 Admin API 返回，
// 不含密钥）。
type CCSwitchCandidate struct {
	CCName    string   `json:"cc_name"`
	Name      string   `json:"name"`
	AppType   string   `json:"app_type"`
	Type      string   `json:"type"`
	URL       string   `json:"url"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	// Loopback 为 true 表示上游 base URL 是回环地址，仅在网关主机可达时有效。
	Loopback bool   `json:"loopback"`
	Status   string `json:"status"` // importable / skipped
	Reason   string `json:"reason,omitempty"`
}

// CCSwitchScan 是 GET /admin/ccswitch 的响应。
type CCSwitchScan struct {
	Home       string              `json:"home"`
	Source     string              `json:"source"`
	Candidates []CCSwitchCandidate `json:"candidates"`
	Importable int                 `json:"importable"`
	Skipped    int                 `json:"skipped"`
	Uploaded   bool                `json:"uploaded"`
	Error      string              `json:"error"`
}

// CCSwitchHint 是概览页的迁移引导状态：网关刚起步、又能拿到 cc-switch
// 配置（网关主机扫描或本机检测）时提示一键迁移。
type CCSwitchHint struct {
	Visible    bool
	Importable int  // 网关侧扫描到的可导入数；本机检测时为 0
	Remote     bool // true = 网关主机没有 cc-switch，检测来自本机
}

// CCSwitchImportResult 是 POST /admin/ccswitch/import 的响应。
type CCSwitchImportResult struct {
	Source        string              `json:"source"`
	Candidates    []CCSwitchCandidate `json:"candidates"`
	ProvidersYAML string              `json:"providers_yaml"`
	EnvFile       string              `json:"env_file"`
	Wrote         bool                `json:"wrote"`
	Imported      []string            `json:"imported"`
	UpdatedKeys   []string            `json:"updated_keys"`
	Error         string              `json:"error"`
}
type Skill struct {
	ID          string          `json:"id"`
	Name        string          `json:"displayName"`
	Emoji       string          `json:"emoji"`
	Description string          `json:"description"`
	Tags        []string        `json:"tags"`
	Author      string          `json:"author"`
	GithubURL   string          `json:"githubUrl"`
	Updated     string          `json:"lastUpdated"`
	Enabled     bool            `json:"enabled"`
	Installed   map[string]bool `json:"installed"`
}
type Skills struct {
	Configured bool     `json:"configured"`
	Hint       string   `json:"hint"`
	Repo       string   `json:"repo"`
	Manifest   string   `json:"manifest_path"`
	Targets    []string `json:"targets"`
	Enabled    []string `json:"enabled"`
	InSync     bool     `json:"in_sync"`
	Skills     []Skill  `json:"skills"`
	Local      []struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Source string `json:"source"`
	} `json:"local_skills"`
	Sync *struct {
		Targets []struct {
			Target  string   `json:"target"`
			Linked  []string `json:"linked"`
			Removed []string `json:"removed"`
			Skipped []string `json:"skipped"`
			Errors  []string `json:"errors"`
		} `json:"targets"`
	} `json:"sync"`
	Error string `json:"error"`
}

// MarketSkill 是技能市场目录里的一条技能（网关代理远端商城的浏览结果）。
type MarketSkill struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description"`
	Category     string   `json:"category"`
	Tags         []string `json:"tags"`
	Version      string   `json:"version"`
	InstallCount int      `json:"install_count"`
	SizeBytes    int64    `json:"tarball_size"`
}

// MarketPage 是技能市场浏览响应；运营性失败经 Error 带内返回。
type MarketPage struct {
	Enabled bool          `json:"enabled"`
	Hint    string        `json:"hint"`
	Skills  []MarketSkill `json:"skills"`
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	Error   string        `json:"error"`
}

// MarketSection 是商城的主题分类。
type MarketSection struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// MarketInstallEvent 是技能安装进度流的单个事件。
type MarketInstallEvent struct {
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MarketInstalled 是本机已安装的市场技能记录（含 EasyAgent 安装的）。
type MarketInstalled struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
}
type Memory struct {
	ID         int     `json:"id"`
	Scope      string  `json:"scope_type"`
	ScopeKey   string  `json:"scope_key"`
	Statement  string  `json:"statement"`
	Status     string  `json:"status"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Hits       int     `json:"hit_count"`
	Updated    string  `json:"updated_at"`
}
type Prompt struct {
	Prompt  string `json:"prompt"`
	Custom  string `json:"custom"`
	Default string `json:"default"`
}
type Event struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Tool    string `json:"tool"`
}

type ChatModelOption struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Channel  string `json:"channel,omitempty"`
}
