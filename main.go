package main

import (
	"fmt"
	"github.com/kisara71/luma/internal/agent"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/llm"
	"github.com/kisara71/luma/internal/logger"
	"github.com/kisara71/luma/internal/memory"
	"github.com/kisara71/luma/internal/server"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Luma 运行失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := "config/config.yaml"
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}

	// 初始化日志系统
	logger.Init(cfg.App.LogLevel, cfg.App.Debug)
	defer func() { _ = zap.L().Sync() }()

	zap.L().Info("配置已加载", zap.String("path", configPath))

	// 创建 Embedding 客户端
	embeddingClient, err := llm.NewEmbeddingClient()
	if err != nil {
		return fmt.Errorf("创建 Embedding 客户端: %w", err)
	}

	// 创建记忆管理器
	memoryMgr, err := memory.NewManager(embeddingClient)
	if err != nil {
		return fmt.Errorf("创建记忆管理器: %w", err)
	}
	defer func() {
		if err := memoryMgr.Close(); err != nil {
			zap.L().Error("关闭记忆系统失败", zap.Error(err))
		}
	}()
	zap.L().Info("记忆系统已初始化")

	// 创建 Agent
	companion, err := agent.New(memoryMgr)
	if err != nil {
		return fmt.Errorf("创建 Agent: %w", err)
	}
	defer companion.Stop()
	if err := companion.Start(); err != nil {
		return fmt.Errorf("启动 Agent: %w", err)
	}

	// 启动HTTP服务（用于健康检查等）
	httpServer := server.NewServer(memoryMgr)
	defer httpServer.Stop()
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.Start() }()

	// 等待退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	zap.L().Info("Luma 已上线，按 Ctrl+C 退出")
	select {
	case <-quit:
		zap.L().Info("正在关闭...")
		return nil
	case err := <-serverErr:
		return err
	}
}
