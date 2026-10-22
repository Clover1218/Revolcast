package main

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/config"
	"revolcast-backend/internal/pkg/logger"
	"revolcast-backend/internal/queue"
	"revolcast-backend/internal/repository"
	"revolcast-backend/internal/transport"

	"go.uber.org/zap"
)

func main() {
	// 加载配置（config.yaml + REVOLCAST_ 环境变量覆盖）
	cfg, err := config.LoadConfig(".")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	MongoDB, err := repository.NewMongoDB(&cfg.MongoDB)
	if err != nil {
		panic("MongoDB 连接失败" + err.Error())
	}

	PostgreSQL, err := repository.NewPostgresDB(&cfg.Database)
	if err != nil {
		panic("PostgreSQL 连接失败" + err.Error())
	}

	Redis, err := repository.NewRedisClient(&cfg.Redis)
	if err != nil {
		panic("Redis 连接失败" + err.Error())
	}
	_ = MongoDB // 当前 BIZ-001 未使用 MongoDB；保留连接以备后续业务
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

	// 业务队列 + Handler。Hub 负责按 bot_id 把指令回传给对应 Adapter。
	hub := transport.NewHub(baseLogger)
	pgRepo := repository.NewQQBindRepo(PostgreSQL)
	redisRepo := repository.NewRedisRepo(Redis)
	bindSvc := biz.NewBindCodeService(pgRepo, redisRepo, biz.Config{
		BindCodeTTL:     5 * time.Minute,
		RateLimitWindow: 5 * time.Minute,
		RateLimitMax:    3,
	})

	// HTTP 鉴权接口（BIZ-002 / BIZ-003 / BIZ-004）：复用同一份 DB/Redis/Logger。
	userRepo := repository.NewUserRepo(PostgreSQL)
	sessionRepo := repository.NewSessionRepo(PostgreSQL)
	authSvc := biz.NewAuthService(userRepo, redisRepo, sessionRepo, biz.AuthConfig{SessionTTL: 30 * 24 * time.Hour})
	authHandler := transport.NewAuthHandler(authSvc, baseLogger, cfg.App.Env == "production")

	// HTTP 用户接口（BIZ-006 网站赎回 QQ 绑定码 / BIZ-007 网站解绑 QQ）：复用同一份 DB/Redis/Logger。
	qqBindSvc := biz.NewQQBindService(sessionRepo, userRepo, pgRepo, redisRepo)
	userHandler := transport.NewUserHandler(qqBindSvc, baseLogger, cfg.App.Env == "production")
	pool := queue.NewPool(rootCtx, queue.Config{
		Workers:        cfg.Queue.Workers,
		QueueSize:      cfg.Queue.QueueSize,
		EnqueueTimeout: time.Duration(cfg.Queue.EnqueueTimeout) * time.Millisecond,
		TaskTimeout:    time.Duration(cfg.Queue.TaskTimeout) * time.Millisecond,
	}, transport.NewBizHandler(hub, baseLogger, bindSvc), baseLogger)
	hub.SetPool(pool)
	go hub.Run()

	// 同一 Server 同时承载 WS（/ws）与 HTTP API（/api/v1/*），复用监听与优雅关闭。
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", transport.ServeWS(hub, &cfg.WS, baseLogger))
	mux.Handle("/api/v1/auth/check-name", transport.CORSMiddleware(http.HandlerFunc(authHandler.CheckName)))
	mux.Handle("/api/v1/auth/register", transport.CORSMiddleware(http.HandlerFunc(authHandler.Register)))
	mux.Handle("/api/v1/auth/login", transport.CORSMiddleware(http.HandlerFunc(authHandler.Login)))
	mux.Handle("/api/v1/auth/logout", transport.CORSMiddleware(http.HandlerFunc(authHandler.Logout)))
	mux.Handle("/api/v1/user/bind-QQ", transport.CORSMiddleware(http.HandlerFunc(userHandler.BindQQ)))
	mux.Handle("/api/v1/user/unbind-QQ", transport.CORSMiddleware(http.HandlerFunc(userHandler.UnbindQQ)))
	srv := &http.Server{
		Addr:         cfg.WS.Addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

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
