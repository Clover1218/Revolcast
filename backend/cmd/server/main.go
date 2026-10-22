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

	// game_events 集合索引：服务启动时幂等创建（server_id / event_type / server_id+occurred_at）。
	// 失败非致命（索引也可由迁移脚本/DBA 单独维护），仅告警不阻塞启动。
	gameEventRepo := repository.NewGameEventRepo(MongoDB)
	idxCtx, idxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := gameEventRepo.EnsureIndexes(idxCtx); err != nil {
		baseLogger.Error("创建 game_events 索引失败（非致命，继续启动）", zap.Error(err))
	}
	idxCancel()

	// 业务队列 + Handler。Hub 按 client_id 把指令回传给对应接入端（Adapter / MC 等）。
	hub := transport.NewHub(baseLogger)
	pgRepo := repository.NewQQBindRepo(PostgreSQL)
	redisRepo := repository.NewRedisRepo(Redis)
	bindSvc := biz.NewBindCodeService(pgRepo, redisRepo, biz.Config{
		BindCodeTTL:     5 * time.Minute,
		RateLimitWindow: 5 * time.Minute,
		RateLimitMax:    3,
	})
	// 游戏事件入库业务（game_events）：消费 WS 上行事件并写入 MongoDB 审计日志。
	gameEventSvc := biz.NewGameEventService(gameEventRepo)

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
	}, transport.NewBizHandler(hub, baseLogger, bindSvc, gameEventSvc), baseLogger)
	hub.SetPool(pool)
	go hub.Run()

	// 游戏服务器业务（game_servers）：WS 接入鉴权与 HTTP 管理接口共用同一 service。
	// 管理接口当前不鉴权（角色系统待引入，见 biz.GameServerService 的 TODO）。
	gameServerRepo := repository.NewGameServerRepo(PostgreSQL)
	gameServerSvc := biz.NewGameServerService(gameServerRepo, redisRepo, biz.GameServerConfig{})

	// 同一 Server 同时承载 WS（/api/v1/adapter/ws、/api/v1/game-servers/ws）与 HTTP API（/api/v1/*），复用监听与优雅关闭。
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/adapter/ws", transport.ServeWS(hub, &cfg.WS, baseLogger, "adapter"))
	// 游戏服务器接入：以 per-server API Secret 鉴权，clientID = game_servers.id（见 ServeGameServerWS）。
	mux.HandleFunc("/api/v1/game-servers/ws", transport.ServeGameServerWS(hub, gameServerSvc, baseLogger))
	mux.Handle("/api/v1/auth/check-name", transport.CORSMiddleware(http.HandlerFunc(authHandler.CheckName)))
	mux.Handle("/api/v1/auth/register", transport.CORSMiddleware(http.HandlerFunc(authHandler.Register)))
	mux.Handle("/api/v1/auth/login", transport.CORSMiddleware(http.HandlerFunc(authHandler.Login)))
	mux.Handle("/api/v1/auth/logout", transport.CORSMiddleware(http.HandlerFunc(authHandler.Logout)))
	mux.Handle("/api/v1/user/bind-QQ", transport.CORSMiddleware(http.HandlerFunc(userHandler.BindQQ)))
	mux.Handle("/api/v1/user/unbind-QQ", transport.CORSMiddleware(http.HandlerFunc(userHandler.UnbindQQ)))

	// HTTP 游戏服务器管理接口（game_servers 增删改查，详见《Revolcast HTTP 接口设计文档》）。
	gameServerHandler := transport.NewGameServerHandler(gameServerSvc, baseLogger)
	mux.Handle("/api/v1/game-servers", transport.CORSMiddleware(http.HandlerFunc(gameServerHandler.Collection)))
	mux.Handle("/api/v1/game-servers/{code}", transport.CORSMiddleware(http.HandlerFunc(gameServerHandler.Item)))
	mux.Handle("/api/v1/game-servers/{code}/rotate-secret", transport.CORSMiddleware(http.HandlerFunc(gameServerHandler.RotateSecret)))

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
