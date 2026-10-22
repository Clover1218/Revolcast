package repository

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// gameEventsCollection 是原始游戏事件审计日志集合名（与 server_messages 不是同一集合）。
// 设计依据：docs/鹊桥与服务端WS连接格式详解.md「建议转换存入MongoDB格式」公共约定。
const gameEventsCollection = "game_events"

// GameEventPlayer 玩家快照（仅保留审计所需字段）。
type GameEventPlayer struct {
	UUID     string `bson:"uuid"`
	Nickname string `bson:"nickname"`
	IsOp     bool   `bson:"is_op"`
}

// GameEventDoc 是 game_events 集合的文档结构，对应文档「建议转换存入MongoDB格式」。
// _id 由 MongoDB 自动生成，此处不声明该字段。
type GameEventDoc struct {
	EventType  string          `bson:"event_type"`  // player_join / player_quit / player_chat / player_command
	ServerName string          `bson:"server_name"` // 原样提取自事件的 server_name，仅展示/自检
	ServerID   int64           `bson:"server_id"`   // game_servers.id，来自 WS 已鉴权身份
	Player     GameEventPlayer `bson:"player"`      // {uuid, nickname, is_op} 快照
	OccurredAt string          `bson:"occurred_at"` // 事件发生时间，UTC ISO8601（由原始 timestamp 转换）
	ReceivedAt string          `bson:"received_at"` // 后端收到时间，UTC ISO8601
	Data       bson.M          `bson:"data"`        // 按事件类型携带的内容（join/quit={}，chat/command 见文档）
}

// GameEventRepo 封装 game_events 集合的写入与索引管理（MongoDB）。
// 库表结构以 docs/鹊桥与服务端WS连接格式详解.md 为准。
type GameEventRepo struct {
	coll *mongo.Collection
}

// NewGameEventRepo 基于已连接的 Database 构造仓库。
func NewGameEventRepo(db *mongo.Database) *GameEventRepo {
	return &GameEventRepo{coll: db.Collection(gameEventsCollection)}
}

// InsertGameEvent 写入一条游戏事件审计日志；_id 由 MongoDB 自动生成。
func (r *GameEventRepo) InsertGameEvent(ctx context.Context, doc *GameEventDoc) error {
	if _, err := r.coll.InsertOne(ctx, doc); err != nil {
		return fmt.Errorf("写入游戏事件失败: %w", err)
	}
	return nil
}

// EnsureIndexes 幂等地为 game_events 创建索引。多次调用安全，索引已存在时 MongoDB 会跳过。
// 索引覆盖：按服务器检索（server_id）、按事件类型检索（event_type）、按服务器+时间范围审计（server_id+occurred_at）。
func (r *GameEventRepo) EnsureIndexes(ctx context.Context) error {
	models := []mongo.IndexModel{
		{Keys: bson.D{{Key: "server_id", Value: 1}}},
		{Keys: bson.D{{Key: "event_type", Value: 1}}},
		{Keys: bson.D{{Key: "server_id", Value: 1}, {Key: "occurred_at", Value: 1}}},
	}
	if _, err := r.coll.Indexes().CreateMany(ctx, models); err != nil {
		return fmt.Errorf("创建 game_events 索引失败: %w", err)
	}
	return nil
}
