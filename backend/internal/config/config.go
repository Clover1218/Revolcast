package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config 是 Revolcast 的全局配置根结构。
// 所有字段通过 viper 从 config.yaml 加载，并支持 REVOLCAST_ 前缀的环境变量覆盖。
type Config struct {
	App      AppConfig      `mapstructure:"app"`
	Log      LogConfig      `mapstructure:"log"`
	Database DatabaseConfig `mapstructure:"database"`
	MongoDB  MongoDBConfig  `mapstructure:"mongodb"`
	Redis    RedisConfig    `mapstructure:"redis"`
	WS       WSConfig       `mapstructure:"ws"`
}

type AppConfig struct {
	Name string `mapstructure:"name"`
	Env  string `mapstructure:"env"`
	Host string `mapstructure:"host"`
	Port string `mapstructure:"port"`
}

// LogConfig 日志配置：支持控制台 / 文件 / 同时输出（JSON 格式，便于接入 ELK / Loki）。
type LogConfig struct {
	Level    string `mapstructure:"level"`    // debug / info / warn / error
	Format   string `mapstructure:"format"`   // json / console
	Output   string `mapstructure:"output"`   // console / file / both
	FilePath string `mapstructure:"filepath"` // 当 output=file 或 both 时生效
}

type WSConfig struct {
	Addr  string `mapstructure:"addr"`  // e.g. ":8080"
	Token string `mapstructure:"token"` // e.g. "zerobot_secret_key"
}

type DatabaseConfig struct {
	Host     string `mapstructure:"pghost"`
	Port     int    `mapstructure:"pgport"`
	User     string `mapstructure:"pguser"`
	Password string `mapstructure:"pgpassword"`
	DBName   string `mapstructure:"dbname"`
	SSLMode  string `mapstructure:"sslmode"`
}

type MongoDBConfig struct {
	Host     string `mapstructure:"mongohost"`
	Port     int    `mapstructure:"mongoport"`
	User     string `mapstructure:"mongouser"`
	Password string `mapstructure:"mongopassword"`
	DBName   string `mapstructure:"dbname"`
}

type RedisConfig struct {
	Host     string `mapstructure:"redishost"`
	Port     int    `mapstructure:"redisport"`
	Password string `mapstructure:"redispassword"`
	DB       int    `mapstructure:"db"`
}

// LoadConfig 加载配置：优先级为 环境变量(REVOLCAST_ 前缀) > config.yaml > 默认值。
// path 指定 config.yaml 所在目录（可传 "." 表示当前工作目录）。
func LoadConfig(path string) (*Config, error) {
	v := viper.New()

	// 1. 默认值（保证即使没有 config.yaml 也能启动，常见于容器化 / 12-factor 场景）
	v.SetDefault("app.name", "Revolcast")
	v.SetDefault("app.env", "development")
	v.SetDefault("app.host", "0.0.0.0")
	v.SetDefault("app.port", "8080")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "console")
	v.SetDefault("log.filepath", "logs/revolcast.log")
	v.SetDefault("database.pghost", "localhost")
	v.SetDefault("database.pgport", 5432)
	v.SetDefault("database.pguser", "revolcast")
	v.SetDefault("database.pgpassword", "revolcast")
	v.SetDefault("database.dbname", "revolcast")
	v.SetDefault("database.sslmode", "disable")
	v.SetDefault("mongodb.mongohost", "localhost")
	v.SetDefault("mongodb.mongoport", 27017)
	v.SetDefault("mongodb.mongouser", "revolcast")
	v.SetDefault("mongodb.mongopassword", "revolcast")
	v.SetDefault("mongodb.dbname", "revolcast")
	v.SetDefault("redis.redishost", "localhost")
	v.SetDefault("redis.redisport", 6379)
	v.SetDefault("redis.redispassword", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("ws.addr", ":8080")
	v.SetDefault("ws.token", "123")

	// 2. 从 config.yaml 读取
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(path)
	v.AddConfigPath(".")

	// 3. 环境变量覆盖：REVOLCAST_DATABASE_PGHOST -> database.pghost
	v.SetEnvPrefix("REVOLCAST")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("读取 config.yaml 失败: %w", err)
		}
		// 配置文件不存在时，只要环境变量 / 默认值齐全仍可运行
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	return &cfg, nil
}
