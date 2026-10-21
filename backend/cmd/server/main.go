package main

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"revolcast-backend/internal/config"
	"revolcast-backend/internal/pkg/logger"
	"revolcast-backend/internal/queue"
	"revolcast-backend/internal/transport"

	"go.uber.org/zap"
)

func main() {
	// 加载配置（config.yaml + REVOLCAST_ 环境变量覆盖）
	cfg, err := config.LoadConfig(".")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	// 初始化日志
	log, err := logger.New(&cfg.Log)
	if err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer log.Sync()

	baseLogger := log.With(zap.String("module", "BackendServer"))
	baseLogger.Info("Revolcast 启动中",
		zap.String("service", cfg.App.Name),
		zap.String("env", cfg.App.Env),
	)

	// 信号驱动的优雅退出根 ctx：收到 SIGINT/SIGTERM 后取消。
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 业务队列 + Handler。Hub 负责按 bot_id 把 Action 回传给对应 Adapter。
	hub := transport.NewHub(baseLogger)
	pool := queue.NewPool(rootCtx, queue.Config{
		Workers:        cfg.Queue.Workers,
		QueueSize:      cfg.Queue.QueueSize,
		EnqueueTimeout: time.Duration(cfg.Queue.EnqueueTimeout) * time.Millisecond,
		TaskTimeout:    time.Duration(cfg.Queue.TaskTimeout) * time.Millisecond,
	}, transport.NewBizHandler(hub, baseLogger), baseLogger)
	hub.SetPool(pool)
	go hub.Run()

	http.HandleFunc("/ws", transport.ServeWS(hub, &cfg.WS, baseLogger))
	srv := &http.Server{Addr: cfg.WS.Addr}

	// 优雅关闭信号：三段式 —— 停收新连接 → 断开已连 WS → 排空队列 → 关 DB。
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-rootCtx.Done()
		baseLogger.Info("收到退出信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// 1. 停收新连接（关闭监听器；已 hijack 的 WS 连接不受影响）。
		if err := srv.Shutdown(shutdownCtx); err != nil {
			baseLogger.Error("HTTP 关闭失败", zap.Error(err))
		}
		// 2. 断开所有已连接 Adapter，使 readPump 停止产生新任务。
		hub.CloseAll()
		// 3. 排空业务队列（drain + 超时保护），这是防止消息丢失的关键。
		if err := pool.Close(shutdownCtx); err != nil {
			baseLogger.Error("业务队列关闭超时", zap.Error(err))
		}
		// 4. 关闭 DB（当前 DB 未启用，预留位置）。

		baseLogger.Info("优雅关闭完成")
	}()

	baseLogger.Info("后端 WS 服务启动", zap.String("addr", cfg.WS.Addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		baseLogger.Fatal("WS 服务启动失败", zap.Error(err))
	}

	// ListenAndServe 返回说明已触发关闭，等待优雅关闭 goroutine 收尾（排空队列等）再退出。
	<-shutdownDone
}
