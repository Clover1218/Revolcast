package transport

import (
	"context"
	"encoding/json"

	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// NewBizHandler 返回业务处理器，供 queue.Pool 消费 Task。
// 它解析 payload、按 type 路由到具体业务逻辑，并把 Action 指令回传给对应 Adapter。
//
// 后续开发：在 route() 里按 task.Type 增加 case 即可，例如：
//   - "event.message.group"   -> 写库 + 自动回复（对接第 3 步 DB 封装）
//   - "event.notice.group_ban" -> 记录禁言事件
//   - 第 2 步的 Action DTO 解析也在此接入
func NewBizHandler(hub *Hub, logger *zap.Logger) queue.Handler {
	return func(ctx context.Context, task queue.Task) error {
		log := logger.With(
			zap.String("trace_id", task.TraceID),
			zap.String("bot_id", task.BotID),
			zap.String("msg_type", task.Type),
		)

		// 业务分发：按 envelope.type 路由，返回需要下发的 Action（可为 nil）。
		action, err := route(ctx, task, log)
		if err != nil {
			return err
		}
		if action == nil {
			return nil
		}

		// 把 Action 指令回传给对应 Adapter（非阻塞，避免拖住 Worker）。
		respEnv := protocol.Envelope{
			Type:    "action",
			TraceID: task.TraceID,
			Payload: action,
		}
		respData, err := json.Marshal(respEnv)
		if err != nil {
			return err
		}
		if !hub.SendToBot(task.BotID, respData) {
			log.Warn("下发 Action 失败：Adapter 不在线或发送队列已满")
		}
		return nil
	}
}

// route 按消息类型做业务路由。当前为占位实现，仅回显收到。
// TODO(step-2/step-3): 接入 Action DTO 与 MongoDB/Redis 持久化。
func route(ctx context.Context, task queue.Task, log *zap.Logger) (json.RawMessage, error) {
	log.Info("处理业务任务", zap.ByteString("payload", task.Payload))
	// ctx 已带 TaskTimeout，长耗时操作（DB 写入等）请监听 ctx.Done()。
	return json.RawMessage(`{"action":"no_op","message":"后端已收到"}`), nil
}
