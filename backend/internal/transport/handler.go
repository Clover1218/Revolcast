package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"revolcast-backend/internal/biz"
	"revolcast-backend/internal/protocol"
	"revolcast-backend/internal/queue"

	"go.uber.org/zap"
)

// commandPrefix 是私聊业务指令的统一前缀（忽略大小写）。
//
// 为什么要前缀门禁：QQ Adapter **不过滤**私聊消息，会把所有私聊原样转发给 Backend
// （业务决策只在 Backend，见 AGENTS.md §3.2）。因此这里必须先按前缀筛掉普通聊天，
// 否则机器人会回复用户的每一句私聊。
const commandPrefix = "revolcast"

// commandHelpText 是 `revolcast` 开头但无法识别时的统一帮助文案，便于用户自查。
const commandHelpText = `未知指令。当前可用指令：
revolcast server list —— 查看可用服务器列表
revolcast server <代号> bind —— 生成游戏角色绑定码
revolcast user bind —— 生成 QQ 绑定码`

// serverListMaxItems 单条私聊消息最多展示的服务器条目数（超长消息在 QQ 端会被折叠）。
const serverListMaxItems = 20

// emptyServerDescription 服务器无简介时的占位文案。
const emptyServerDescription = "暂无简介"

// commandKind 是私聊指令的识别结果。
type commandKind int

const (
	// commandNone 不是 Revolcast 指令（普通私聊），必须静默丢弃。
	commandNone commandKind = iota
	// commandUserBind `revolcast user bind`（BIZ-001）。
	commandUserBind
	// commandServerList `revolcast server list`（BIZ-008）。
	commandServerList
	// commandServerBind `revolcast server <code> bind`（BIZ-009）。
	commandServerBind
	// commandHelp `revolcast` 开头但无法识别。
	commandHelp
)

// outbound 是业务处理后需要下发给对应 Adapter 的指令。
type outbound struct {
	Type    string // 协议 type，例如 command.qq.send_private_message
	Payload any    // 对应 payload 结构体，由 Handler 序列化为 Envelope.Payload
}

// NewBizHandler 返回业务处理器，供 queue.Pool 消费 Task。
// 它解析 payload、按 type 路由到具体业务逻辑，并把指令回传给对应 Adapter。
// gameBindSvc 负责服务器列表（BIZ-008）与游戏角色绑定码（BIZ-009）；
// gameEventSvc 负责落库鹊桥上报的游戏事件（player_join/quit/chat/command）。
func NewBizHandler(hub *Hub, logger *zap.Logger, svc *biz.BindCodeService, gameBindSvc *biz.GameBindService, gameEventSvc *biz.GameEventService) queue.Handler {
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
		out, err := route(ctx, task, svc, gameBindSvc, log)
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
// 当前支持 QQ 私聊的四类指令：user bind / server list / server <code> bind / help。
// 其它类型暂不打扰，待后续业务在此扩展 case。
func route(ctx context.Context, task queue.Task, svc *biz.BindCodeService, gameBindSvc *biz.GameBindService, log *zap.Logger) (*outbound, error) {
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

	kind, arg := parseCommand(payload.Message.Text)
	if kind == commandNone {
		// 非 Revolcast 指令：静默丢弃（Adapter 会转发全部私聊，这里必须兜住）。
		return nil, nil
	}

	// 只有需要业务数据的指令才要求可解析的 QQ 号；帮助文案不依赖它。
	qqUIN, uinErr := strconv.ParseInt(payload.Sender.ID, 10, 64)
	if uinErr != nil && kind != commandHelp {
		log.Warn("无法解析发送者 QQ 号", zap.String("sender_id", payload.Sender.ID))
		return nil, nil
	}

	reply := ""
	switch kind {
	case commandHelp:
		reply = commandHelpText

	case commandUserBind:
		code, berr := svc.GenerateBindCode(ctx, qqUIN, task.ClientID, task.TraceID)
		if berr == nil {
			reply = fmt.Sprintf("你的 QQ 绑定码为：%s\n请在 Revolcast 网站个人中心的「QQ 绑定」页面输入，5 分钟内有效。", code)
			break
		}
		logBizError(log, "生成 QQ 绑定码失败", berr, zap.Int64("qq_uin", qqUIN))
		reply = biz.ResolveErrorMeta(berr).WSMessage

	case commandServerList:
		servers, lErr := gameBindSvc.ListAvailableServers(ctx)
		if lErr != nil {
			logBizError(log, "查询可用服务器列表失败", lErr)
			reply = biz.ResolveErrorMeta(lErr).WSMessage
			break
		}
		reply = renderServerList(servers)

	case commandServerBind:
		outcome, gErr := gameBindSvc.GenerateBindCode(ctx, qqUIN, arg, task.TraceID)
		if gErr != nil {
			logBizError(log, "生成游戏角色绑定码失败", gErr,
				zap.Int64("qq_uin", qqUIN),
				zap.String("server_code", arg),
			)
			if biz.ResolveErrorMeta(gErr).WSCode == protocol.ErrorCodeServerNotFound {
				// 对用户给出可执行指引，而不是干巴巴一句「未找到」。
				reply = fmt.Sprintf("未找到代号为「%s」的可用服务器。\n请发送「revolcast server list」查看当前可用服务器。", arg)
			} else {
				reply = biz.ResolveErrorMeta(gErr).WSMessage
			}
			break
		}
		reply = renderGameBindOutcome(outcome)
	}

	// 所有已识别指令都向用户回一条私聊消息，避免静默无响应。
	// 文案不泄露内部错误细节（细节只进日志）。
	payloadOut := protocol.QQSendPrivateMsgPayload{
		CommandID:       protocol.NewCommandID(),
		ReceiverID:      payload.Sender.ID,
		MessageSegments: []protocol.QQSegment{protocol.NewTextSegment(reply)},
	}
	return &outbound{Type: protocol.TypeQQSendPrivateMessage, Payload: payloadOut}, nil
}

// parseCommand 识别私聊指令。
//
// 规则：大小写不敏感、空白不敏感（strings.Fields 切词），参数字数必须完全匹配。
// 返回 arg 仅 commandServerBind 有值（服务器代号，已转小写）。
func parseCommand(text string) (kind commandKind, arg string) {
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(tokens) == 0 || tokens[0] != commandPrefix {
		return commandNone, ""
	}

	switch {
	case len(tokens) == 3 && tokens[1] == "user" && tokens[2] == "bind":
		return commandUserBind, ""
	case len(tokens) == 3 && tokens[1] == "server" && tokens[2] == "list":
		return commandServerList, ""
	case len(tokens) == 4 && tokens[1] == "server" && tokens[3] == "bind":
		return commandServerBind, tokens[2]
	default:
		// `revolcast` 开头但指令不完整/不存在：回帮助文案（不静默）。
		return commandHelp, ""
	}
}

// renderServerList 渲染「当前可用服务器」文案（每台服务器两行：名称+简介、代号）。
// 超过 serverListMaxItems 时截断并提示总数。
func renderServerList(servers []biz.AvailableServer) string {
	if len(servers) == 0 {
		return "当前没有可用的服务器。"
	}

	shown := servers
	if len(shown) > serverListMaxItems {
		shown = shown[:serverListMaxItems]
	}

	var b strings.Builder
	b.WriteString("当前可用服务器：")
	for i, s := range shown {
		if i == 0 {
			b.WriteString("\n")
		} else {
			b.WriteString("\n\n")
		}
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = emptyServerDescription
		}
		b.WriteString(s.Name)
		b.WriteString("：")
		b.WriteString(desc)
		b.WriteString("\n代号：")
		b.WriteString(s.Code)
	}

	if total := len(servers); total > len(shown) {
		fmt.Fprintf(&b, "\n\n（共 %d 个可用服务器，仅显示前 %d 个，请访问网站查看完整列表。）", total, len(shown))
	}
	return b.String()
}

// renderGameBindOutcome 渲染 BIZ-009 的三种结论文案。
func renderGameBindOutcome(o *biz.GameBindOutcome) string {
	if o == nil {
		return biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
	switch o.Kind {
	case biz.GameBindOutcomeCode:
		return fmt.Sprintf(
			"你的游戏角色绑定码为：%s\n服务器：%s（%s）\n请在目标服务器内输入该绑定码完成角色绑定，%d 分钟内有效。",
			o.Code, o.Server.Name, o.Server.Code, remainingMinutes(o.ExpiresAt),
		)
	case biz.GameBindOutcomeAlreadyBound:
		return fmt.Sprintf(
			"你在服务器「%s」已绑定角色：%s\n一个账号在同一个服务器只能绑定一个角色。如需更换，请联系管理员。",
			o.Server.Name, o.GameName,
		)
	case biz.GameBindOutcomeQQNotBound:
		return "你的 QQ 尚未绑定 Revolcast 账号。\n请先私聊发送「revolcast user bind」获取 QQ 绑定码，并在网站个人中心完成 QQ 绑定后再试。"
	default:
		return biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
}

// remainingMinutes 把到期时间换算为「还剩几分钟」，最少 1（避免出现「0 分钟内有效」）。
// 从结果推导而非常量，保证文案与配置的 TTL 不漂移。
func remainingMinutes(expiresAt time.Time) int {
	m := int(time.Until(expiresAt).Minutes())
	if m < 1 {
		m = 1
	}
	return m
}

// logBizError 统一记录业务失败日志：对外只暴露 error_code，对内保留完整错误链。
func logBizError(log *zap.Logger, msg string, err error, fields ...zap.Field) {
	fields = append(fields,
		zap.String("error_code", biz.ResolveErrorMeta(err).WSCode),
		zap.Error(err),
	)
	log.Warn(msg, fields...)
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
