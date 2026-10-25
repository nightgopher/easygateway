package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/provider"
)

// HandleUsage GET /admin/usage?days=30 或 ?from=2026-09-01&to=2026-09-30
// 返回区间内的用量成本报表：按「模型 × 渠道」与渠道两个维度的 token 用量
// 与美元成本。单价来自 providers.yaml 模型条目的 pricing 字段；未配置单价
// 的模型不计成本并列入 unpriced_models，而不是按 0 冒充免费。
// days 缺省 30，与 stats/history 的区间参数语义一致。
func (h *AdminHandler) HandleUsage(c *gin.Context) {
	if h.dailyStats == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage stats unavailable: persistence disabled"})
		return
	}

	now := time.Now()
	from, to := now.AddDate(0, 0, -29).Format("2006-01-02"), now.Format("2006-01-02")
	if c.Query("from") != "" || c.Query("to") != "" {
		var err error
		from, to, err = parseStatsRange(c.Query("from"), c.Query("to"), now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else if raw := c.Query("days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be a positive integer"})
			return
		}
		if days > statsHistoryMaxDays {
			days = statsHistoryMaxDays
		}
		from = now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}

	rows, err := h.dailyStats.GetDailyStatRows(from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 单价查找：先按请求模型名，再按渠道实例名（渠道模型的实例名就是上游
	// 模型 ID，链别名如 coding 因此也能借成员模型的价格折算）。
	pricing := make(map[string]*provider.ModelPricing)
	for _, info := range h.router.ListModelInfos() {
		if info.Pricing != nil && info.Pricing.Priced() {
			pricing[info.ID] = info.Pricing
		}
	}
	usage := metrics.AggregateUsage(rows, func(key string) *provider.ModelPricing {
		return pricing[key]
	})
	usage.From, usage.To = from, to
	c.JSON(http.StatusOK, usage)
}
