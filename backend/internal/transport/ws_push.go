package transport

import (
	"encoding/json"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"

	"go.uber.org/zap"
)

// buildGameEventPush 把已落库的事件视图转换为推送给 Web 端的 Envelope 字节。
//
// 消息类型沿用统一协议：type = event.game.<event_type>，trace_id 沿用产生该事件的链路，
// payload 为 protocol.GameEventPushPayload（服务端已完成字段与时间归一化）。
func buildGameEventPush(traceID string, view *biz.GameEventView, serverCode string) ([]byte, error) {
	payload := protocol.GameEventPushPayload{
		ServerCode: serverCode,
		ServerID:   view.ServerID,
		EventType:  view.EventType,
		Player:     view.Player,
		OccurredAt: view.OccurredAt.UTC().Format(time.RFC3339Nano),
		ReceivedAt: view.ReceivedAt.UTC().Format(time.RFC3339Nano),
		Data:       buildGameEventPushData(view),
	}

	env := protocol.Envelope{
		Version: "v1",
		Type:    "event.game." + view.EventType,
		TraceID: traceID,
		Payload: mustJSON(payload),
	}
	return json.Marshal(env)
}

// buildGameEventPushData 按事件类型填充推送内容（与落库 data 子文档同源，但只下发前端需要的字段）：
//   - join/quit: 空对象
//   - chat:      message / raw_message（文本组件 JSON）/ message_id
//   - command:   command / message_id（command 的 raw_message 是 Java toString，不下发）
func buildGameEventPushData(view *biz.GameEventView) protocol.GameEventPushData {
	switch view.EventType {
	case "player_chat":
		return protocol.GameEventPushData{
			Message:    view.Message,
			RawMessage: view.RawMessage,
			MessageID:  view.MessageID,
		}
	case "player_command":
		return protocol.GameEventPushData{
			Command:   view.Command,
			MessageID: view.MessageID,
		}
	}
	return protocol.GameEventPushData{}
}

// pushGameEventToWeb 把落库成功的事件推送给订阅该服务器（server_code）的 Web 连接。
//
// 推送条件：事件已成功写入 MongoDB，且当前存在该服务器的 Web 订阅者（按 server_id 定位 server_code）。
// 不广播给其他服务器的 Web 连接；单个连接队列满只丢弃该连接，不影响落库结果与其他人。
func pushGameEventToWeb(hub *Hub, traceID string, view *biz.GameEventView, logger *zap.Logger) {
	if view == nil {
		return
	}

	// TODO(visibility): 数据库表设计文档 §1.1 规则 6 要求「可见性规则必须在 WebSocket 推送处统一执行」，
	// 即 visibility=0（隐身角色）的事件不应进入公开推送。当前 server_game_bindings 尚无仓储实现，
	// 因此暂未过滤；角色绑定能力落地后应在此处按 (server_id, player.uuid) 查询并跳过隐身角色。
	serverCode, ok := hub.WebServerCode(view.ServerID)
	if !ok {
		// 无 Web 订阅者：无需构造推送消息，直接返回（常见路径，避免无谓序列化）。
		return
	}

	data, err := buildGameEventPush(traceID, view, serverCode)
	if err != nil {
		logger.Warn("构造游戏事件推送失败", zap.String("server_code", serverCode), zap.Error(err))
		return
	}

	delivered := hub.PushToWeb(serverCode, data)
	logger.Debug("游戏事件已推送至 Web 客户端",
		zap.String("server_code", serverCode),
		zap.String("event_type", view.EventType),
		zap.Int("delivered", delivered))
}
