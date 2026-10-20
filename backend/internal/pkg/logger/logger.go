package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"revolcast-backend/internal/config"
)

// New 根据 LogConfig 初始化一个 zap.Logger。
//
// 支持：
//   - Format: "json"（默认，面向 ELK / Loki 采集）或 "console"
//   - Output: "console" | "file" | "both"
//
// 调用方在进程退出前应执行 logger.Sync() 刷盘，避免丢失最后几条日志。
func New(cfg *config.LogConfig) (*zap.Logger, error) {
	// 解析日志级别；解析失败时降级为 info
	var level zapcore.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	// 统一时间格式与字段名，方便日志系统解析
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encCfg.EncodeCaller = zapcore.ShortCallerEncoder

	var encoder zapcore.Encoder
	if cfg.Format == "console" {
		encoder = zapcore.NewConsoleEncoder(encCfg)
	} else {
		encoder = zapcore.NewJSONEncoder(encCfg) // 默认 JSON，便于 ELK / Loki
	}

	// 输出目标
	var writer zapcore.WriteSyncer
	switch cfg.Output {
	case "file":
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		writer = zapcore.AddSync(f)
	case "both":
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		writer = zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(f))
	default: // console
		writer = zapcore.AddSync(os.Stdout)
	}

	core := zapcore.NewCore(encoder, writer, level)
	logger := zap.New(core, zap.AddCaller())

	return logger, nil
}
