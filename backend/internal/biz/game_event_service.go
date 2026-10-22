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

// GameEventView 是已落库游戏事件的规范化视图，供 Web 端 WebSocket 推送使用。
// 时间转换规则与落库文档同源（由 Ingest 统一产出），避免推送与存储两处各写一套导致漂移。
type GameEventView struct {
	ServerID   int64                  // 已鉴权的 game_servers.id（来自 WS 连接身份）
	ServerName string                 // 事件上报的服务器名快照，仅展示
	EventType  string                 // player_join / player_quit / player_chat / player_command
	Player     protocol.QueqiaoPlayer // 玩家快照
	OccurredAt time.Time              // 事件发生时间（由原始秒级 timestamp 还原）
	ReceivedAt time.Time              // 后端收到时间
	Message    string                 // 仅 player_chat
	RawMessage string                 // player_chat: 文本组件 JSON；player_command: 无业务价值
	MessageID  string                 // 聊天/命令事件
	Command    string                 // 仅 player_command（不含前导 /）
}

// Ingest 接收已鉴权服务器（serverID 来自 WS 已鉴权身份，不得由 server_name 反查）上报的原始事件，
// 转换后写入 MongoDB，并返回规范化视图供调用方（Web 推送）复用。
// 参数 receivedAt 为后端收到事件的时间（来自 WS readPump，UTC）。
//
// 未知 sub_type 或空事件返回 ErrInvalidParam；写入失败包裹为 ErrInternal 并保留根因（供日志输出完整链）。
// 返回值 nil 表示事件未被接受（此时不会写入存储），调用方不得据此推送。
func (s *GameEventService) Ingest(ctx context.Context, serverID int64, receivedAt time.Time, ev *protocol.GameEventPayload) (*GameEventView, error) {
	if ev == nil {
		return nil, Wrap(ErrInvalidParam, nil, "游戏事件为空")
	}
	if _, ok := knownGameEventSubTypes[ev.SubType]; !ok {
		return nil, Wrap(ErrInvalidParam, nil, fmt.Sprintf("未知游戏事件子类型: %s", ev.SubType))
	}

	view := &GameEventView{
		ServerID:   serverID,
		ServerName: ev.ServerName,
		EventType:  ev.SubType,
		Player:     ev.Player,
		// 事件发生时间：原始 timestamp（秒级）；后端收到时间：WS 读取时刻。
		// ReceivedAt 截断到毫秒：MongoDB BSON Date 精度为毫秒，截断后「推送值」与「落库值」严格一致，
		// 前端按游标补齐时不会因精度差产生重复或遗漏。
		OccurredAt: time.Unix(ev.Timestamp, 0).UTC(),
		ReceivedAt: receivedAt.UTC().Truncate(time.Millisecond),
		Message:    ev.Message,
		RawMessage: ev.RawMessage,
		MessageID:  ev.MessageID,
		Command:    ev.Command,
	}

	doc := &repository.GameEventDoc{
		EventType:  view.EventType,
		ServerName: view.ServerName,
		ServerID:   view.ServerID,
		Player: repository.GameEventPlayer{
			UUID:     view.Player.UUID,
			Nickname: view.Player.Nickname,
			IsOp:     view.Player.IsOp,
		},
		// 时间字段以 BSON Date 写入（见 repository.GameEventDoc 注释）：字符串无法支撑游标分页。
		OccurredAt: view.OccurredAt,
		ReceivedAt: view.ReceivedAt,
		Data:       buildGameEventData(view),
	}

	if err := s.store.InsertGameEvent(ctx, doc); err != nil {
		return nil, Wrap(ErrInternal, err, "写入游戏事件失败")
	}
	return view, nil
}

// buildGameEventData 按事件类型组装 data 子文档：
//   - join/quit: {}（原始事件不携带额外内容）
//   - chat:     {message, raw_message, message_id}
//   - command:  {command, raw_message, message_id}（raw_message 为 Java 对象 toString，仅留存）
func buildGameEventData(v *GameEventView) bson.M {
	switch v.EventType {
	case "player_join", "player_quit":
		return bson.M{}
	case "player_chat":
		return bson.M{
			"message":     v.Message,
			"raw_message": v.RawMessage,
			"message_id":  v.MessageID,
		}
	case "player_command":
		return bson.M{
			"command":     v.Command,
			"raw_message": v.RawMessage,
			"message_id":  v.MessageID,
		}
	}
	return bson.M{}
}
