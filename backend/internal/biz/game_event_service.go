package biz

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/repository"
)

// 已知游戏事件子类型（sub_type），与 docs/鹊桥与服务端WS连接格式详解.md 一致。
// 未知 sub_type 视为非法参数，由 Ingest 返回 ErrInvalidParam（调用方记日志后丢弃）。
var knownGameEventSubTypes = map[string]struct{}{
	"player_join":    {},
	"player_quit":    {},
	"player_chat":    {},
	"player_command": {},
}

// GameEventStore 是 GameEventService 对 game_events 存储的依赖（便于测试注入 fake）。
// 接口定义在 biz 层，由 repository.GameEventRepo 实现；biz 不反向依赖 repository 的具体类型实现。
type GameEventStore interface {
	InsertGameEvent(ctx context.Context, doc *repository.GameEventDoc) error
}

// 编译期断言：repository.GameEventRepo 必须满足 biz.GameEventStore。
var _ GameEventStore = (*repository.GameEventRepo)(nil)

// GameEventService 负责把鹊桥原始事件转换为 game_events 文档并落库。
// 平台无关，不依赖 ZeroBot / WS 运行时上下文（见 AGENTS.md §3.1）。
type GameEventService struct {
	store GameEventStore
}

// NewGameEventService 构造游戏事件服务。store 不允许为 nil。
func NewGameEventService(store GameEventStore) *GameEventService {
	return &GameEventService{store: store}
}

// Ingest 接收已鉴权服务器（serverID 来自 WS 已鉴权身份，不得由 server_name 反查）上报的原始事件，
// 转换后写入 MongoDB。receivedAt 为后端收到事件的时间（来自 WS readPump，UTC）。
//
// 未知 sub_type 或空事件返回 ErrInvalidParam；写入失败包裹为 ErrInternal 并保留根因（供日志输出完整链）。
func (s *GameEventService) Ingest(ctx context.Context, serverID int64, receivedAt time.Time, ev *protocol.GameEventPayload) error {
	if ev == nil {
		return Wrap(ErrInvalidParam, nil, "游戏事件为空")
	}
	if _, ok := knownGameEventSubTypes[ev.SubType]; !ok {
		return Wrap(ErrInvalidParam, nil, fmt.Sprintf("未知游戏事件子类型: %s", ev.SubType))
	}

	doc := &repository.GameEventDoc{
		EventType:  ev.SubType,
		ServerName: ev.ServerName,
		ServerID:   serverID,
		Player: repository.GameEventPlayer{
			UUID:     ev.Player.UUID,
			Nickname: ev.Player.Nickname,
			IsOp:     ev.Player.IsOp,
		},
		// 事件发生时间：原始 timestamp（秒级）→ UTC ISO8601。
		OccurredAt: time.Unix(ev.Timestamp, 0).UTC().Format(time.RFC3339Nano),
		// 后端收到时间：WS 读取时刻（已是 UTC）。
		ReceivedAt: receivedAt.UTC().Format(time.RFC3339Nano),
		Data:       buildGameEventData(ev),
	}

	if err := s.store.InsertGameEvent(ctx, doc); err != nil {
		return Wrap(ErrInternal, err, "写入游戏事件失败")
	}
	return nil
}

// buildGameEventData 按事件类型组装 data 子文档：
//   - join/quit: {}（原始事件不携带额外内容）
//   - chat:     {message, raw_message, message_id}
//   - command:  {command, raw_message, message_id}（raw_message 为 Java 对象 toString，仅留存）
func buildGameEventData(ev *protocol.GameEventPayload) bson.M {
	switch ev.SubType {
	case "player_join", "player_quit":
		return bson.M{}
	case "player_chat":
		return bson.M{
			"message":     ev.Message,
			"raw_message": ev.RawMessage,
			"message_id":  ev.MessageID,
		}
	case "player_command":
		return bson.M{
			"command":     ev.Command,
			"raw_message": ev.RawMessage,
			"message_id":  ev.MessageID,
		}
	}
	return bson.M{}
}
