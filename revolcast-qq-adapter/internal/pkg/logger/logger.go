package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"revolcast-qq-adapter/internal/config"
)

func New(cfg *config.LogConfig) (*zap.Logger, error) {
	var level zapcore.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encCfg.EncodeCaller = zapcore.ShortCallerEncoder

	var encoder zapcore.Encoder
	if cfg.Format == "console" {
		encoder = zapcore.NewConsoleEncoder(encCfg)
	} else {
		encoder = zapcore.NewJSONEncoder(encCfg)
	}

	// 构建 WriteSyncer
	var writer zapcore.WriteSyncer

	// 核心改造：使用 lumberjack 实现文件轮转
	if cfg.Output == "file" || cfg.Output == "both" {
		lj := &lumberjack.Logger{
			Filename:   cfg.FilePath, // 日志文件路径
			MaxSize:    100,          // 单个文件最大 MB，超过则切割
			MaxBackups: 7,            // 最多保留 7 个旧文件
			MaxAge:     30,           // 旧文件最多保留 30 天
			Compress:   true,         // 是否压缩旧文件（节省磁盘）
			LocalTime:  true,         // 使用本地时间命名切割文件
		}

		if cfg.Output == "both" {
			// 同时输出到控制台和文件
			writer = zapcore.NewMultiWriteSyncer(
				zapcore.AddSync(os.Stdout),
				zapcore.AddSync(lj),
			)
		} else {
			// 仅输出到文件
			writer = zapcore.AddSync(lj)
		}
	} else {
		// 仅输出到控制台
		writer = zapcore.AddSync(os.Stdout)
	}

	core := zapcore.NewCore(encoder, writer, level)
	logger := zap.New(core, zap.AddCaller())

	return logger, nil
}
