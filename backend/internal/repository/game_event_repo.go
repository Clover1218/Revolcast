package repository

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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

// GameEventDoc 是 game_events 集合的**写入**模型，对应文档「建议转换存入MongoDB格式」。
// _id 由 MongoDB 自动生成，此处不声明该字段。
//
// 时间字段为 BSON Date（time.Time），**不得再写 ISO8601 字符串**：
// 字符串比较是字典序，`...34.5Z` 会大于 `...34.522048Z`（实际时间更早），无法支撑游标分页。
// 详见 docs/Revolcast_HTTP接口设计文档.md §13.7。
type GameEventDoc struct {
	EventType  string          `bson:"event_type"`  // player_join / player_quit / player_chat / player_command
	ServerName string          `bson:"server_name"` // 原样提取自事件的 server_name，仅展示/自检
	ServerID   int64           `bson:"server_id"`   // game_servers.id，来自 WS 已鉴权身份
	Player     GameEventPlayer `bson:"player"`      // {uuid, nickname, is_op} 快照
	OccurredAt time.Time       `bson:"occurred_at"` // 事件发生时间（由原始 timestamp 转换），UTC
	ReceivedAt time.Time       `bson:"received_at"` // 后端收到时间，UTC（毫秒精度）
	Data       bson.M          `bson:"data"`        // 按事件类型携带的内容（join/quit={}，chat/command 见文档）
}

// GameEventData 是 data 子文档的**读取**模型：缺省键解码为零值，无需按事件类型分支。
type GameEventData struct {
	Message    string `bson:"message"`
	RawMessage string `bson:"raw_message"`
	MessageID  string `bson:"message_id"`
	Command    string `bson:"command"`
}

// GameEventRecord 是 game_events 的**读取**模型（含 _id）。
// _id 仅用于游标定位（同一 received_at 下的稳定 tie-break），不作为对外字段。
type GameEventRecord struct {
	ID         primitive.ObjectID `bson:"_id"`
	EventType  string             `bson:"event_type"`
	ServerName string             `bson:"server_name"`
	ServerID   int64              `bson:"server_id"`
	Player     GameEventPlayer    `bson:"player"`
	OccurredAt time.Time          `bson:"occurred_at"`
	ReceivedAt time.Time          `bson:"received_at"`
	Data       GameEventData      `bson:"data"`
}

// GameEventDirection 是事件查询方向。
type GameEventDirection int

const (
	// GameEventLatest 取最新 N 条（默认）。
	GameEventLatest GameEventDirection = iota
	// GameEventBefore 取严格早于游标的事件（加载更早历史）。
	GameEventBefore
	// GameEventAfter 取严格晚于游标的事件（断线补齐 / 增量同步）。
	GameEventAfter
)

// GameEventCursorPoint 是游标定位点，语义为「received_at + _id」组成的全序位置。
type GameEventCursorPoint struct {
	ReceivedAt time.Time
	ID         primitive.ObjectID
}

// GameEventQuery 是事件查询条件。ServerID 必须由已校验的 server_code 解析得到，
// 不得直接采用前端传入的 server_id。
type GameEventQuery struct {
	ServerID  int64
	Direction GameEventDirection
	// Cursor 仅在 Direction 为 Before / After 时有效。
	Cursor GameEventCursorPoint
	// Limit 是期望返回条数（>0）；仓库内部多取 1 条用于探测是否还有下一页。
	Limit int
}

// GameEventRepo 封装 game_events 集合的写入、查询与索引管理（MongoDB）。
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

// QueryGameEvents 按服务器 + 游标条件分页查询事件，结果统一按 `received_at ASC, _id ASC` 返回。
// 第二个返回值表示「沿当前方向是否还有下一页」，由多取 1 条探测得出（不做额外 count）。
//
// 排序策略（见 HTTP 接口文档 §13.6）：
//   - Latest / Before：降序取最近记录（可命中同一复合索引的反向扫描），返回前反转为升序；
//   - After：直接升序，天然满足返回顺序。
//
// 查询失败包裹为 error 返回，由 biz 层决定对外错误码（不在此暴露 Mongo 细节）。
func (r *GameEventRepo) QueryGameEvents(ctx context.Context, q GameEventQuery) ([]*GameEventRecord, bool, error) {
	if q.Limit <= 0 {
		return nil, false, fmt.Errorf("游戏事件查询 limit 非法: %d", q.Limit)
	}
	descending := q.Direction != GameEventAfter
	direction := int32(1)
	if descending {
		direction = -1
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "received_at", Value: direction}, {Key: "_id", Value: direction}}).
		// 多取 1 条仅用于探测 has_more，避免额外 count 查询。
		SetLimit(int64(q.Limit + 1))

	cur, err := r.coll.Find(ctx, gameEventFilter(q), opts)
	if err != nil {
		return nil, false, fmt.Errorf("查询游戏事件失败: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	recs := make([]*GameEventRecord, 0, q.Limit+1)
	for cur.Next(ctx) {
		var rec GameEventRecord
		if err := cur.Decode(&rec); err != nil {
			return nil, false, fmt.Errorf("解码游戏事件失败: %w", err)
		}
		recs = append(recs, &rec)
	}
	if err := cur.Err(); err != nil {
		return nil, false, fmt.Errorf("遍历游戏事件失败: %w", err)
	}

	hasMore := len(recs) > q.Limit
	if hasMore {
		recs = recs[:q.Limit]
	}
	if descending {
		// 降序结果反转为升序，保证对外顺序唯一（received_at ASC, _id ASC）。
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
	}
	return recs, hasMore, nil
}

// gameEventFilter 构造查询条件：必含 server_id 等值过滤 + 按方向的严格游标条件。
//
// 游标条件（严格，不含游标本身）：
//   - before: received_at < t OR (received_at = t AND _id < id)
//   - after:  received_at > t OR (received_at = t AND _id > id)
//
// 仅比较时间戳会在同一毫秒内的多条事件上产生重复或遗漏，故必须带 _id tie-break。
func gameEventFilter(q GameEventQuery) bson.D {
	filter := bson.D{{Key: "server_id", Value: q.ServerID}}
	switch q.Direction {
	case GameEventBefore, GameEventAfter:
		var cmpOp string
		if q.Direction == GameEventBefore {
			cmpOp = "$lt"
		} else {
			cmpOp = "$gt"
		}
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "received_at", Value: bson.D{{Key: cmpOp, Value: q.Cursor.ReceivedAt}}}},
			bson.D{
				{Key: "received_at", Value: q.Cursor.ReceivedAt},
				{Key: "_id", Value: bson.D{{Key: cmpOp, Value: q.Cursor.ID}}},
			},
		}})
	}
	return filter
}

// EnsureIndexes 幂等地为 game_events 创建索引。多次调用安全，索引已存在时 MongoDB 会跳过。
func (r *GameEventRepo) EnsureIndexes(ctx context.Context) error {
	if _, err := r.coll.Indexes().CreateMany(ctx, gameEventIndexModels()); err != nil {
		return fmt.Errorf("创建 game_events 索引失败: %w", err)
	}
	return nil
}

// gameEventIndexModels 是 game_events 的索引定义（独立函数便于单测校验键序）。
//
// 覆盖：
//   - {server_id}：按服务器检索；
//   - {event_type}：按事件类型检索；
//   - {server_id, occurred_at}：按事件发生时间范围审计；
//   - {server_id, received_at, _id}：支撑 HTTP 事件查询的游标分页
//     （等值前缀 → 范围/排序 → 同毫秒 tie-break；升序与降序复用同一索引的反向扫描，无需另建降序索引）。
func gameEventIndexModels() []mongo.IndexModel {
	return []mongo.IndexModel{
		{Keys: bson.D{{Key: "server_id", Value: 1}}},
		{Keys: bson.D{{Key: "event_type", Value: 1}}},
		{Keys: bson.D{{Key: "server_id", Value: 1}, {Key: "occurred_at", Value: 1}}},
		{Keys: bson.D{{Key: "server_id", Value: 1}, {Key: "received_at", Value: 1}, {Key: "_id", Value: 1}}},
	}
}
