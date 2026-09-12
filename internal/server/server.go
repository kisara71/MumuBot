package server

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/memory"
	"github.com/kisara71/luma/internal/session"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Server HTTP服务
type Server struct {
	memoryMgr *memory.Manager
	server    *http.Server
}

// NewServer 创建HTTP服务
func NewServer(memoryMgr *memory.Manager) *Server {
	cfg := config.Get()
	if !cfg.App.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	s := &Server{memoryMgr: memoryMgr}
	r := gin.Default()

	// 健康检查
	r.GET("/health", s.healthCheck)

	// API 路由
	api := r.Group("/api")
	{
		// 记忆相关
		api.GET("/memories", s.listMemories)
		api.GET("/memories/:id", s.getMemory)
		api.DELETE("/memories/:id", s.deleteMemory)

		// 用户画像
		api.GET("/users", s.listUsers)
		api.GET("/users/:user_id", s.getUser)

		// 消息记录
		api.GET("/messages", s.listMessages)

		// 统计信息
		api.GET("/stats", s.getStats)

		// 状态
		api.GET("/status", s.getStatus)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: r,
	}
	return s
}

// Start 启动HTTP服务并将启动或运行错误交给进程生命周期管理。
func (s *Server) Start() error {
	zap.L().Info("HTTP 服务启动", zap.String("addr", s.server.Addr))
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP 服务异常: %w", err)
	}
	return nil
}

// Stop 停止HTTP服务
func (s *Server) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(ctx)
	}
}

// healthCheck 健康检查
func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"name":   "luma",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// parsePageParams 解析分页参数
func parsePageParams(c *gin.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return
}

// listMemories 列出记忆
func (s *Server) listMemories(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.DefaultQuery("user_id", "0"), 10, 64)
	if userID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id 必填"})
		return
	}
	memType := c.DefaultQuery("type", "")
	page, pageSize := parsePageParams(c)

	memories, total, err := s.memoryMgr.ListMemoriesByConversation(session.NewRef(userID), memType, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":      memories,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// getMemory 获取单个记忆
func (s *Server) getMemory(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 ID"})
		return
	}

	var mem memory.Memory
	if err := s.memoryMgr.GetDB().First(&mem, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "记忆不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": mem})
}

// deleteMemory 删除记忆
func (s *Server) deleteMemory(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 ID"})
		return
	}

	if err := s.memoryMgr.DeleteMemory(c.Request.Context(), uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "删除成功"})
}

// listUsers 列出用户画像
func (s *Server) listUsers(c *gin.Context) {
	page, pageSize := parsePageParams(c)
	profiles, total, err := s.memoryMgr.ListUserProfiles(page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":      profiles,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// getUser 获取单个用户画像
func (s *Server) getUser(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的用户 ID"})
		return
	}

	var profile memory.UserProfile
	query := s.memoryMgr.GetDB().Where("user_id = ?", userID)

	if err := query.First(&profile).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": profile})
}

// listMessages 列出消息记录
func (s *Server) listMessages(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.DefaultQuery("user_id", "0"), 10, 64)
	if userID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id 必填"})
		return
	}
	page, pageSize := parsePageParams(c)

	messages, total, err := s.memoryMgr.ListMessageLogsByConversation(session.NewRef(userID), page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":      messages,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// getStats 获取统计信息
func (s *Server) getStats(c *gin.Context) {
	stats := s.memoryMgr.GetStats()
	c.JSON(http.StatusOK, gin.H{"data": stats})
}

// getStatus 获取状态
func (s *Server) getStatus(c *gin.Context) {
	stats := s.memoryMgr.GetStats()
	cfg := config.Get()

	c.JSON(http.StatusOK, gin.H{
		"status":  "running",
		"persona": cfg.Persona.Name,
		"users":   len(cfg.Users),
		"uptime":  time.Now().Format(time.RFC3339),
		"stats":   stats,
		"config": gin.H{
			"proactive_check_interval": cfg.Agent.ProactiveCheckInterval,
			"llm_model":                cfg.LLM.Model,
		},
	})
}
