package main

import (
	"net/http"
	"revolcast-backend/internal/config"
	"revolcast-backend/internal/pkg/logger"
	_ "revolcast-backend/internal/repository"
	"revolcast-backend/internal/transport"

	"go.uber.org/zap"
)

func main() {
	// 加载配置（config.yaml + REVOLCAST_ 环境变量覆盖）
	cfg, err := config.LoadConfig(".")
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	// 2初始化日志
	log, err := logger.New(&cfg.Log)
	if err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer log.Sync()

	log.Info("Revolcast 启动中",
		zap.String("service", cfg.App.Name),
		zap.String("env", cfg.App.Env),
	)

	// // PostgreSQL
	// pgPool, err := repository.NewPostgresDB(&cfg.Database)
	// if err != nil {
	// 	log.Fatal("PostgreSQL 连接失败", zap.Error(err))
	// }
	// defer pgPool.Close()
	// log.Info("PostgreSQL 连接成功", zap.String("host", cfg.Database.Host))

	// // MongoDB
	// mongoDB, err := repository.NewMongoDB(&cfg.MongoDB)
	// if err != nil {
	// 	log.Fatal("MongoDB 连接失败", zap.Error(err))
	// }
	// log.Info("MongoDB 连接成功", zap.String("host", cfg.MongoDB.Host), zap.String("db", mongoDB.Name()))

	// // Redis
	// rdb, err := repository.NewRedisClient(&cfg.Redis)
	// if err != nil {
	// 	log.Fatal("Redis 连接失败", zap.Error(err))
	// }
	// defer rdb.Close()
	// log.Info("Redis 连接成功", zap.String("host", cfg.Redis.Host))

	// WS服务器
	baseLogger := log.With(zap.String("module", "BackendServer"))
	hub := transport.NewHub(baseLogger)
	go hub.Run()
	http.HandleFunc("/ws", transport.ServeWS(hub, &cfg.WS, baseLogger))

	baseLogger.Info("后端 WS 服务启动", zap.String("addr", cfg.WS.Addr))
	if err := http.ListenAndServe(cfg.WS.Addr, nil); err != nil {
		baseLogger.Fatal("WS 服务启动失败", zap.Error(err))
	}

}
