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
	WS       WSConfig       `mapstructure:"ws"`
	NapCat   NapCatConfig   `mapstructure:"napcat"`
	Bot      BotConfig      `mapstructure:"bot"`
	Database DatabaseConfig `mapstructure:"database"`
	MongoDB  MongoDBConfig  `mapstructure:"mongodb"`
	Redis    RedisConfig    `mapstructure:"redis"`
}

// WSConfig 与 Backend 建立 WS 连接所需的鉴权与标识。
type WSConfig struct {
	Addr  string `mapstructure:"addr"`   // Backend WS 地址，e.g. "localhost:8080"
	Token string `mapstructure:"token"`  // 与 Backend ws.token 一致的鉴权串
	BotID string `mapstructure:"bot_id"` // 本 Adapter 的稳定逻辑 ID，用于指令回传路由
}

// NapCatConfig 是 OneBot 实现端（NapCat）的 WebSocket 服务端地址。
// Adapter 以「WS 客户端」身份主动连它，因此 NapCat 侧必须开启
// 「WebSocket 服务器」并监听 0.0.0.0（监听 127.0.0.1 时容器连不进来）。
//
// 对应环境变量：
//
//	REVOLCAST_NAPCAT_WS_URL、REVOLCAST_NAPCAT_TOKEN
type NapCatConfig struct {
	WSURL string `mapstructure:"ws_url"` // e.g. ws://host.docker.internal:6701
	Token string `mapstructure:"token"`  // NapCat 侧配置的 access_token；未配置则留空
}

// BotConfig 是 ZeroBot 的运行参数（登录哪个 QQ 由 NapCat 决定，这里只管机器人行为）。
//
// 两个 []string 字段刻意不用 []int64：viper 的环境变量覆盖只能给出字符串，
// 而 mapstructure 在未开启 WeaklyTypedInput 时不会把 "123" 转成 int64。
// 因此统一按字符串收，由 main 再解析校验（config.yaml 里也必须写带引号的字符串）。
//
// 对应环境变量：
//
//	REVOLCAST_BOT_NICKNAME、REVOLCAST_BOT_SUPERUSERS（逗号分隔）、REVOLCAST_BOT_COMMAND_PREFIX
type BotConfig struct {
	NickName      []string `mapstructure:"nickname"`       // 触发「@ 机器人」的昵称列表
	SuperUsers    []string `mapstructure:"superusers"`     // 超级用户 QQ 号，逗号分隔
	CommandPrefix string   `mapstructure:"command_prefix"` // 命令前缀，留空 = 无前缀
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
	v.SetDefault("ws.addr", "localhost:8080")
	v.SetDefault("ws.token", "123")
	v.SetDefault("ws.bot_id", "revolcast-qq-bot")
	// NapCat（OneBot WS 服务端）：Adapter 是客户端，容器化后地址通常是
	// ws://host.docker.internal:6701（NapCat 跑在宿主机）。
	v.SetDefault("napcat.ws_url", "ws://127.0.0.1:6701")
	v.SetDefault("napcat.token", "")
	// ZeroBot 行为参数
	v.SetDefault("bot.nickname", []string{"bot"})
	v.SetDefault("bot.superusers", []string{})
	v.SetDefault("bot.command_prefix", "")

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
