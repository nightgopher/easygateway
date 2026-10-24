package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easyagent/sdk/skillmarket"
)

// SkillsInstallJobsHandler 把技能安装做成网关侧任务：提交后立刻返回任务号，
// 安装由网关后台独立执行（进度记录在任务上），桌面端轮询任务状态。
// 客户端断开、切换连接甚至关闭桌面端都不影响安装本身。
type SkillsInstallJobsHandler struct {
	client *skillmarket.Client
	logger *log.Logger

	mu   sync.Mutex
	jobs map[string]*installJob
	// order 保持提交顺序（最旧在前），用于清理历史任务。
	order []string
}

// installJob 是一次安装任务的进度与结果。
type installJob struct {
	ID          string    `json:"job_id"`
	SkillID     int       `json:"skill_id"`
	Phase       string    `json:"phase"` // resolving / downloading / verifying / extracting / done / failed
	BytesDone   int64     `json:"bytes_done,omitempty"`
	BytesTotal  int64     `json:"bytes_total,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
}

func (j *installJob) running() bool {
	switch j.Phase {
	case "resolving", "downloading", "verifying", "extracting":
		return true
	}
	return false
}

const installJobsRetain = 20

// NewSkillsInstallJobsHandler 创建安装任务 handler；client 为 nil（市场未启用）时
// 所有端点返回 enabled=false。
func NewSkillsInstallJobsHandler(client *skillmarket.Client, logger *log.Logger) *SkillsInstallJobsHandler {
	return &SkillsInstallJobsHandler{client: client, logger: logger, jobs: map[string]*installJob{}}
}

// HandleInstallJobStart POST /admin/skills/market/install-jobs {"id":N} —
// 提交后台安装任务，立刻返回任务号。同一时刻只允许一个安装任务。
func (h *SkillsInstallJobsHandler) HandleInstallJobStart(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体应为 {\"id\": <技能 ID>}"})
		return
	}
	if h.hasRunning() {
		c.JSON(http.StatusConflict, gin.H{"error": "已有技能安装任务进行中，请等待完成后再提交"})
		return
	}

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成任务号失败: " + err.Error()})
		return
	}
	job := &installJob{
		ID:        "ij-" + hex.EncodeToString(raw[:]),
		SkillID:   body.ID,
		Phase:     "resolving",
		StartedAt: time.Now(),
	}
	h.record(job)

	go h.run(job)
	c.JSON(http.StatusAccepted, gin.H{"enabled": true, "job_id": job.ID})
}

// HandleInstallJobGet GET /admin/skills/market/install-jobs/:id — 查询单个任务。
func (h *SkillsInstallJobsHandler) HandleInstallJobGet(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	job, ok := h.lookup(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"enabled": true, "error": "安装任务不存在（可能来自网关重启前的会话），请刷新后重新安装"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "job": job})
}

// HandleInstallJobList GET /admin/skills/market/install-jobs — 最近任务列表（旧→新）。
func (h *SkillsInstallJobsHandler) HandleInstallJobList(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	jobs := make([]*installJob, 0, len(h.order))
	for _, id := range h.order {
		jobs = append(jobs, h.jobs[id])
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "jobs": jobs})
}

func (h *SkillsInstallJobsHandler) enabled(c *gin.Context) bool {
	if h.client != nil {
		return true
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": false,
		"hint":    "技能市场未启用：去掉 SKILLS_MARKET_ENABLED=false 或确认 SKILLS_MARKET_SKILLS_DIR 可用后重启网关",
	})
	return false
}

func (h *SkillsInstallJobsHandler) hasRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.order) - 1; i >= 0; i-- {
		if job := h.jobs[h.order[i]]; job != nil && job.running() {
			return true
		}
	}
	return false
}

func (h *SkillsInstallJobsHandler) lookup(id string) (*installJob, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	job, ok := h.jobs[id]
	if !ok {
		return nil, false
	}
	// 响应在锁外编码，不能暴露后台安装协程仍在修改的共享对象。
	snapshot := *job
	return &snapshot, true
}

// record 登记新任务并清理超出保留数量的已完结任务。
func (h *SkillsInstallJobsHandler) record(job *installJob) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobs[job.ID] = job
	h.order = append(h.order, job.ID)
	for len(h.order) > installJobsRetain {
		oldest := h.order[0]
		if job := h.jobs[oldest]; job != nil && !job.running() {
			delete(h.jobs, oldest)
			h.order = h.order[1:]
			continue
		}
		break
	}
}

func (h *SkillsInstallJobsHandler) update(job *installJob, mutate func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	mutate()
}

// run 在后台执行安装；进度写入任务，结果（done/failed）为终态。
func (h *SkillsInstallJobsHandler) run(job *installJob) {
	// 独立于请求的 context：任务的生命周期归网关所有，客户端断开不取消。
	item, err := h.client.InstallWithProgress(context.Background(), job.SkillID, func(p skillmarket.InstallProgress) {
		h.update(job, func() {
			job.Phase = string(p.Phase)
			job.BytesDone, job.BytesTotal = p.BytesDone, p.BytesTotal
			if p.DisplayName != "" {
				job.DisplayName = p.DisplayName
			}
		})
	})
	h.update(job, func() {
		if err != nil {
			job.Phase = "failed"
			job.Error = marketErrorMessage(err)
		} else {
			job.Phase = "done"
			job.DisplayName = item.DisplayName
		}
		job.FinishedAt = time.Now()
	})
	if err != nil {
		h.logger.Printf("skill market install job %s (id=%d): %v", job.ID, job.SkillID, err)
	} else {
		h.logger.Printf("skill market install job %s done: %s", job.ID, item.Name)
	}
}
