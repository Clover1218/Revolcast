package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// bindCommand 触发 BIZ-001 的私聊命令（业务流程设计文档 §1）。
// 命令格式与优先级为草案示例，落地前需评审确认。
const bindCommand = "revolcast user bind"

// outbound 是业务处理后需要下发给对应 Adapter 的指令。
type outbound struct {
	Type    string // 协议 type，例如 command.qq.send_private_message
	Payload any    // 对应 payload 结构体，由 Handler 序列化为 Envelope.Payload
}

// NewBizHandler 返回业务处理器，供 queue.Pool 消费 Task。
// 它解析 payload、按 type 路由到具体业务逻辑，并把指令回传给对应 Adapter。
// gameEventSvc 负责落库鹊桥上报的游戏事件（player_join/quit/chat/command）。
func NewBizHandler(hub *Hub, logger *zap.Logger, svc *biz.BindCodeService, gameEventSvc *biz.GameEventService) queue.Handler {
	return func(ctx context.Context, task queue.Task) error {
		log := logger.With(
			zap.String("trace_id", task.TraceID),
			zap.String("client_id", task.ClientID),
			zap.String("msg_type", task.Type),
		)

		// 游戏服务器（mc_server）上报的事件：直接落库，落库成功后推送给订阅的 Web 客户端。
		if isGameEventType(task.Type) {
			return handleGameEvent(ctx, task, hub, gameEventSvc, log)
		}

		// 业务分发：按 envelope.type 路由，返回需要下发的指令（可为 nil）。
		out, err := route(ctx, task, svc, log)
		if err != nil {
			return err
		}
		if out == nil {
			return nil
		}

		// 把指令回传给对应 Adapter（非阻塞，避免拖住 Worker）。
		respEnv := protocol.Envelope{
			Version: "v1",
			Type:    out.Type,
			TraceID: task.TraceID,
			Payload: mustJSON(out.Payload),
		}
		respData, err := json.Marshal(respEnv)
		if err != nil {
			return err
		}
		if !hub.SendToClient(task.ClientID, respData) {
			log.Warn("下发指令失败：Adapter 不在线或发送队列已满")
		}
		return nil
	}
}

// route 按消息类型做业务路由。
// 当前实现 BIZ-001（event.qq.message.private + "revolcast user bind"）；
// 其它类型暂不打扰，待后续业务在此扩展 case。
func route(ctx context.Context, task queue.Task, svc *biz.BindCodeService, log *zap.Logger) (*outbound, error) {
	if task.Type != protocol.TypeQQMessagePrivate {
		// 当前仅处理 QQ 私聊；群聊/其它事件后续扩展。
		return nil, nil
	}

	var payload protocol.QQMessagePayload
	if err := task.Decode(&payload); err != nil {
		log.Warn("解析 QQ 消息 payload 失败", zap.Error(err))
		return nil, nil
	}
	if payload.Conversation.Type != "private" {
		return nil, nil
	}

	text := strings.TrimSpace(payload.Message.Text)
	if !strings.EqualFold(text, bindCommand) {
		// 非绑定命令的私聊消息，忽略（不回复）。
		return nil, nil
	}

	qqUIN, err := strconv.ParseInt(payload.Sender.ID, 10, 64)
	if err != nil {
		log.Warn("无法解析发送者 QQ 号", zap.String("sender_id", payload.Sender.ID))
		return nil, nil
	}

	code, berr := svc.GenerateBindCode(ctx, qqUIN, task.ClientID, task.TraceID)

	// 所有私聊命令路径都向用户回一条私聊消息（成功或友好提示），
	// 避免用户得到静默无响应。文案不泄露内部错误细节（日志最小化）。
	var reply string
	if berr == nil {
		reply = fmt.Sprintf("你的 QQ 绑定码为：%s\n请在 Revolcast 网站个人中心的「QQ 绑定」页面输入，5 分钟内有效。", code)
	} else {
		// 对外：由 biz 注册表统一映射（error_code + 用户友好文案），不泄露内部细节；
		// 对内：完整错误链通过 zap.Error 输出到日志，error_code 便于聚合告警。
		meta := biz.ResolveErrorMeta(berr)
		log.Warn("生成绑定码失败",
			zap.Int64("qq_uin", qqUIN),
			zap.String("error_code", meta.WSCode),
			zap.Error(berr),
		)
		reply = meta.WSMessage
	}

	payloadOut := protocol.QQSendPrivateMsgPayload{
		CommandID:       protocol.NewCommandID(),
		ReceiverID:      payload.Sender.ID,
		MessageSegments: []protocol.QQSegment{protocol.NewTextSegment(reply)},
	}
	return &outbound{Type: protocol.TypeQQSendPrivateMessage, Payload: payloadOut}, nil
}

// mustJSON 序列化 payload；业务层结构体可序列化，失败仅返回 nil（由调用方记录）。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// isGameEventType 判断 task.Type 是否为鹊桥游戏事件类型（event.game.*）。
func isGameEventType(t string) bool {
	switch t {
	case protocol.TypeGamePlayerJoin,
		protocol.TypeGamePlayerQuit,
		protocol.TypeGamePlayerChat,
		protocol.TypeGamePlayerCommand:
		return true
	}
	return false
}

// handleGameEvent 处理鹊桥上报的游戏事件：解析 payload → 从 WS 已鉴权身份取 serverID → 落库 → 推送 Web 订阅者。
// 解析失败或来源 client_id 非 server_id 时记日志并丢弃（不向前抛，避免污染队列指标）。
// 业务错误（未知 sub_type / Mongo 写入失败）由 biz 内部 Wrap 携带根因，这里记 Warn 日志（含 trace）便于排查。
//
// 推送时机：仅在落库成功（Ingest 返回 view）后推送，避免把未持久化的事件散播给前端；
// 推送失败不影响返回结果（落库是权威事实，推送是尽力而为）。
func handleGameEvent(ctx context.Context, task queue.Task, hub *Hub, svc *biz.GameEventService, log *zap.Logger) error {
	var payload protocol.GameEventPayload
	if err := task.Decode(&payload); err != nil {
		log.Warn("解析游戏事件 payload 失败", zap.Error(err))
		return nil
	}

	// server_id 来自 WS 已鉴权身份（与连接身份一致），不从 server_name 反查。
	serverID, err := strconv.ParseInt(task.ClientID, 10, 64)
	if err != nil {
		log.Warn("游戏事件来源 client_id 非 server_id，丢弃",
			zap.String("client_id", task.ClientID))
		return nil
	}

	view, err := svc.Ingest(ctx, serverID, task.ReceivedAt, &payload)
	if err != nil {
		log.Warn("处理游戏事件失败",
			zap.String("error_code", biz.ResolveErrorMeta(err).WSCode),
			zap.Error(err))
		return err
	}

	pushGameEventToWeb(hub, task.TraceID, view, log)
	return nil
}
