package transport

import (
	"context"
	"encoding/json"
	"errors"
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
revolcast server <代号> status —— 查看该服务器当前在线人数
revolcast server <代号> bind —— 生成游戏角色绑定码
revolcast server <代号> unbind —— 解除该服务器的角色绑定
revolcast user bind —— 生成 QQ 绑定码`

// 游戏内命令文案（通过鹊桥 send_private_msg 下发，客户端会自动加 [鹊桥] 前缀）。
const (
	// gameBindUsageText 游戏内 `revolcast bind` 参数不完整时的用法提示。
	gameBindUsageText = "用法：/revolcast bind <8 位绑定码>\n绑定码请在 QQ 私聊发送「revolcast server <代号> bind」获取。"
	// gameRateLimitedText 游戏内绑定被限流时的提示（与 QQ 端文案口气不同）。
	gameRateLimitedText = "操作过于频繁，请 5 分钟后再试。"
)

// serverListMaxItems 单条私聊消息最多展示的服务器条目数（超长消息在 QQ 端会被折叠）。
const serverListMaxItems = 20

// serverStatusMaxNames 单条「服务器状态」回执最多展示的玩家昵称数。
// 人数多时全量列举在 QQ 端反而不易读，超出部分只提示总数。
const serverStatusMaxNames = 30

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
	// commandServerUnbind `revolcast server <code> unbind`（BIZ-011）。
	commandServerUnbind
	// commandServerStatus `revolcast server <code> status`（BIZ-012）。
	commandServerStatus
	// commandHelp `revolcast` 开头但无法识别。
	commandHelp
)

// gameCommandKind 是游戏内命令（鹊桥 player_command 事件）的识别结果。
// 后续新增游戏内命令时在此扩展，不要在 handleGameCommand 里散落字符串比较。
type gameCommandKind int

const (
	// gameCommandNone 不是 Revolcast 游戏内命令：静默丢弃（玩家普通指令不打扰）。
	gameCommandNone gameCommandKind = iota
	// gameCommandBind `/revolcast bind <code>`（BIZ-010）。
	gameCommandBind
	// gameCommandBindUsage `revolcast bind` 参数不完整：回用法提示。
	gameCommandBindUsage
)

// gameBindService 是 Handler 对游戏角色绑定业务的依赖。
//
// 用接口而非 *biz.GameBindService：transport 只关心这四个业务动作，
// 且便于在测试中注入 fake（不需要构造完整的 biz 依赖图）。
// 生产实现为 *biz.GameBindService（见 cmd/server/main.go）。
type gameBindService interface {
	ListAvailableServers(ctx context.Context) ([]biz.AvailableServer, error)
	GenerateBindCode(ctx context.Context, qqUIN int64, serverCode, traceID string) (*biz.GameBindOutcome, error)
	BindGameRole(ctx context.Context, serverID int64, player protocol.QueqiaoPlayer, rawCode, traceID string) (*biz.GameRoleBindOutcome, error)
	UnbindGameRole(ctx context.Context, qqUIN int64, serverCode, traceID string) (*biz.GameRoleUnbindOutcome, error)
}

// gameStatusService 是 Handler 对服务器在线状态查询业务（BIZ-012）的依赖。
//
// 只读 Redis 快照，不触发即时采集（决策 D4）：查询路径成本恒定为一次 GET。
// 生产实现为 *biz.GameStatusService。
type gameStatusService interface {
	// GetStatus 返回状态快照；nil 表示该服务器当前没有快照（未连接 / 刚掉线）。
	GetStatus(ctx context.Context, serverCode string) (*biz.ServerStatusView, error)
}

// gameNotifier 是「向游戏内玩家发私聊」的依赖，生产实现为 *GameNotifier。
type gameNotifier interface {
	SendText(serverID int64, player protocol.QueqiaoPlayer, color, text string) bool
}

// outbound 是业务处理后需要下发给对应 Adapter 的指令。
type outbound struct {
	Type    string // 协议 type，例如 command.qq.send_private_message
	Payload any    // 对应 payload 结构体，由 Handler 序列化为 Envelope.Payload
}

// NewBizHandler 返回业务处理器，供 queue.Pool 消费 Task。
// 它解析 payload、按 type 路由到具体业务逻辑，并把指令回传给对应 Adapter。
// gameBindSvc 负责服务器列表（BIZ-008）、游戏角色绑定码（BIZ-009）、
// 游戏内绑定（BIZ-010）与 QQ 解绑（BIZ-011）；
// statusSvc 负责服务器在线状态查询（BIZ-012，只读 Redis 快照）；
// gameEventSvc 负责落库鹊桥上报的游戏事件（player_join/quit/chat/command）；
// notifier 是游戏内私聊通知的唯一出口（绑定结果、解绑通知）。
func NewBizHandler(
	hub *Hub,
	logger *zap.Logger,
	svc *biz.BindCodeService,
	gameBindSvc gameBindService,
	statusSvc gameStatusService,
	gameEventSvc *biz.GameEventService,
	notifier gameNotifier,
) queue.Handler {
	return func(ctx context.Context, task queue.Task) error {
		log := logger.With(
			zap.String("trace_id", task.TraceID),
			zap.String("client_id", task.ClientID),
			zap.String("msg_type", task.Type),
		)

		// 游戏服务器（mc_server）上报的事件：落库审计，必要时处理游戏内命令，
		// 再把可公开的事件推送给订阅的 Web 客户端。
		if isGameEventType(task.Type) {
			return handleGameEvent(ctx, task, hub, gameEventSvc, gameBindSvc, notifier, log)
		}

		// 业务分发：按 envelope.type 路由，返回需要下发的指令（可为 nil）。
		out, err := route(ctx, task, svc, gameBindSvc, statusSvc, notifier, log)
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
// 当前支持 QQ 私聊的六类指令：user bind / server list / server <code> status /
// server <code> bind / server <code> unbind / help。其它类型暂不打扰，待后续业务在此扩展 case。
func route(
	ctx context.Context,
	task queue.Task,
	svc *biz.BindCodeService,
	gameBindSvc gameBindService,
	statusSvc gameStatusService,
	notifier gameNotifier,
	log *zap.Logger,
) (*outbound, error) {
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

	case commandServerStatus:
		// 只读 Redis 快照（不触发即时采集，见决策 D4）。
		if statusSvc == nil {
			reply = biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
			break
		}
		view, sErr := statusSvc.GetStatus(ctx, arg)
		if sErr != nil {
			logBizError(log, "查询服务器在线状态失败", sErr, zap.String("server_code", arg))
			// Redis 故障与「没数据」必须区分：否则用户会误以为服务器掉线。
			reply = "状态查询暂时不可用，请稍后再试。"
			break
		}
		reply = renderServerStatus(arg, view)

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

	case commandServerUnbind:
		outcome, uErr := gameBindSvc.UnbindGameRole(ctx, qqUIN, arg, task.TraceID)
		if uErr != nil {
			logBizError(log, "解除游戏角色绑定失败", uErr,
				zap.Int64("qq_uin", qqUIN),
				zap.String("server_code", arg),
			)
			if biz.ResolveErrorMeta(uErr).WSCode == protocol.ErrorCodeServerNotFound {
				reply = fmt.Sprintf("未找到代号为「%s」的游戏服务器。\n请发送「revolcast server list」查看当前可用服务器。", arg)
			} else {
				reply = biz.ResolveErrorMeta(uErr).WSMessage
			}
			break
		}
		reply = renderGameRoleUnbindOutcome(outcome)
		// 尽力而为：若该服务器当前在线，向被解绑的角色发一条游戏内提示。
		// 发送失败不影响 QQ 回执（解绑已落库），只记日志。
		notifyGameRoleUnbound(notifier, outcome)
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
	case len(tokens) == 4 && tokens[1] == "server" && tokens[3] == "status":
		return commandServerStatus, tokens[2]
	case len(tokens) == 4 && tokens[1] == "server" && tokens[3] == "bind":
		return commandServerBind, tokens[2]
	case len(tokens) == 4 && tokens[1] == "server" && tokens[3] == "unbind":
		return commandServerUnbind, tokens[2]
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

// renderServerStatus 渲染「服务器在线状态」文案（BIZ-012）。
//
// view 为 nil 表示该服务器当前没有状态快照（未连接 / 刚掉线 / 从未采集成功）；
// 这与「有快照但 0 人在线」是两种不同结论，必须分开表述。
func renderServerStatus(code string, view *biz.ServerStatusView) string {
	if view == nil {
		return fmt.Sprintf(
			"未找到「%s」的服务器状态，该服务器可能尚未连接。\n请稍后再试，或联系管理员确认该服务器是否在线。", code)
	}

	// 昵称过滤为空串（防御脏数据），但不改变「有几个人」这个结论。
	names := make([]string, 0, len(view.Players))
	for _, p := range view.Players {
		if n := strings.TrimSpace(p.Nickname); n != "" {
			names = append(names, n)
		}
	}

	if view.Online <= 0 && len(names) == 0 {
		return "当前服务器没有玩家在线。"
	}
	if len(names) == 0 {
		// 人数 > 0 但昵称全部无法展示（例如昵称含分隔符被解析器跳过）：只报人数。
		return fmt.Sprintf("当前服务器有 %d 人在线。", view.Online)
	}

	shown := names
	truncated := false
	if len(shown) > serverStatusMaxNames {
		shown = shown[:serverStatusMaxNames]
		truncated = true
	}

	text := fmt.Sprintf("当前服务器有 %d 人：%s", view.Online, strings.Join(shown, "，"))
	if truncated {
		text += fmt.Sprintf("……（仅显示前 %d 人）", serverStatusMaxNames)
	}
	return text
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

// renderGameRoleBindOutcome 把 BIZ-010 的结论渲染为游戏内私聊文案。
// 返回 (颜色, 文本)；空文本表示不回复（当前所有分支都有文案）。
func renderGameRoleBindOutcome(o *biz.GameRoleBindOutcome) (color, text string) {
	if o == nil {
		return protocol.GameColorRed, "✘ " + biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
	serverLabel := o.ServerName
	if o.ServerCode != "" {
		serverLabel = fmt.Sprintf("%s（%s）", o.ServerName, o.ServerCode)
	}

	switch o.Kind {
	case biz.GameRoleBindOutcomeBound, biz.GameRoleBindOutcomeIdempotent:
		// 幂等与首次成功使用同一文案：重复输入同一码时玩家看到的就是「成功」。
		unbindHint := ""
		if o.ServerCode != "" {
			unbindHint = fmt.Sprintf("\n如需解绑，请在 QQ 私聊发送：revolcast server %s unbind", o.ServerCode)
		}
		return protocol.GameColorGreen, fmt.Sprintf(
			"✔ 绑定成功\n角色：%s\n服务器：%s%s", o.GameName, serverLabel, unbindHint,
		)

	case biz.GameRoleBindOutcomeCodeInvalid:
		hint := "请在 QQ 中私聊发送「revolcast server <代号> bind」重新获取绑定码。"
		if o.ServerCode != "" {
			hint = fmt.Sprintf("请在 QQ 中私聊发送「revolcast server %s bind」重新获取绑定码。", o.ServerCode)
		}
		return protocol.GameColorRed, "✘ 绑定码无效或已过期。\n" + hint

	case biz.GameRoleBindOutcomeAccountBound:
		return protocol.GameColorYellow, fmt.Sprintf(
			"你在本服务器已绑定角色：%s\n一个账号在同一服务器只能绑定一个角色。如需更换，请先在 QQ 私聊解绑。",
			o.GameName,
		)

	case biz.GameRoleBindOutcomeRoleTaken:
		return protocol.GameColorRed,
			"✘ 该角色已被其他 Revolcast 账号绑定。\n请先在原账号解绑，或联系服务器管理员处理。"

	case biz.GameRoleBindOutcomeServerUnavailable:
		return protocol.GameColorRed, "✘ 该服务器当前不可用，绑定未生效。"

	case biz.GameRoleBindOutcomePlayerInvalid:
		return protocol.GameColorRed, "✘ 无法识别你的游戏角色，请联系服务器管理员。"

	default:
		return protocol.GameColorRed, "✘ " + biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
}

// renderGameRoleUnbindOutcome 把 BIZ-011 的结论渲染为 QQ 私聊文案。
func renderGameRoleUnbindOutcome(o *biz.GameRoleUnbindOutcome) string {
	if o == nil {
		return biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
	switch o.Kind {
	case biz.GameRoleUnbindOutcomeDone:
		return fmt.Sprintf(
			"已解除在服务器「%s」的角色绑定：%s\n如需重新绑定，请重新获取绑定码后在游戏内输入。",
			o.ServerName, o.GameName,
		)
	case biz.GameRoleUnbindOutcomeNotBound:
		return fmt.Sprintf("你在服务器「%s」没有已绑定的角色，无需解绑。", o.ServerName)
	case biz.GameRoleUnbindOutcomeQQNotBound:
		return "你的 QQ 尚未绑定 Revolcast 账号。\n请先私聊发送「revolcast user bind」获取 QQ 绑定码，并在网站个人中心完成 QQ 绑定后再试。"
	default:
		return biz.ResolveErrorMeta(biz.ErrInternal).WSMessage
	}
}

// notifyGameRoleUnbound 在解绑成功后尽力通知游戏内角色（离线 / 未连接只记日志）。
// 通知是附属效果：解绑结果已由数据库 UPDATE 决定，不因通知失败回滚或改文案。
func notifyGameRoleUnbound(notifier gameNotifier, o *biz.GameRoleUnbindOutcome) {
	if notifier == nil || o == nil || o.Kind != biz.GameRoleUnbindOutcomeDone || o.GameUUID == "" {
		return
	}
	notifier.SendText(o.ServerID,
		protocol.QueqiaoPlayer{UUID: o.GameUUID, Nickname: o.GameName},
		protocol.GameColorGreen,
		"你的 Revolcast 账号与该角色的绑定已解除。")
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

// handleGameEvent 处理鹊桥上报的游戏事件：解析 payload → 从 WS 已鉴权身份取 serverID → 落库
// →（仅 player_command）处理游戏内命令 → 推送可公开的事件给 Web 订阅者。
// 解析失败或来源 client_id 非 server_id 时记日志并丢弃（不向前抛，避免污染队列指标）。
// 业务错误（未知 sub_type / Mongo 写入失败）由 biz 内部 Wrap 携带根因，这里记 Warn 日志（含 trace）便于排查。
//
// 顺序：**先落库再处理业务**。游戏内命令是审计对象，即使绑定逻辑失败也不能丢审计记录。
// 可见性：player_command 不进入公开事件流（推送与 HTTP 查询双侧拦截）。
func handleGameEvent(
	ctx context.Context,
	task queue.Task,
	hub *Hub,
	svc *biz.GameEventService,
	bindSvc gameBindService,
	notifier gameNotifier,
	log *zap.Logger,
) error {
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

	// 游戏内命令：落库之后进入业务分发；处理结果通过游戏内私聊回执。
	// player_command 不对外推送，因此无论是否识别成功都不继续走 Web 推送。
	if view.EventType == protocol.SubTypePlayerCommand {
		handleGameCommand(ctx, serverID, task.TraceID, &payload, bindSvc, notifier, log)
		return nil
	}

	pushGameEventToWeb(hub, task.TraceID, view, log)
	return nil
}

// handleGameCommand 分发游戏内业务命令，并把结果渲染成私聊文本回给玩家。
//
// 可扩展点：后续新增游戏内命令时在此补充 case（识别逻辑统一放 parseGameCommand），
// 不要在 parseGameServerEvent 或 ws 层做业务判断。
// 未识别的命令静默返回（玩家普通指令不打扰，也不产生业务日志噪音）。
func handleGameCommand(
	ctx context.Context,
	serverID int64,
	traceID string,
	ev *protocol.GameEventPayload,
	svc gameBindService,
	notifier gameNotifier,
	log *zap.Logger,
) {
	kind, arg := parseGameCommand(ev.Command)
	if kind == gameCommandNone {
		return
	}

	cmdLog := log.With(
		zap.Int64("server_id", serverID),
		zap.String("player_uuid", ev.Player.UUID),
		zap.String("player_name", ev.Player.Nickname),
		zap.String("command", ev.Command),
	)

	color, text := protocol.GameColorYellow, ""
	switch kind {
	case gameCommandBind:
		cmdLog.Info("收到游戏内绑定命令")
		outcome, err := svc.BindGameRole(ctx, serverID, ev.Player, arg, traceID)
		if err != nil {
			logBizError(cmdLog, "游戏内绑定角色失败", err)
			if errors.Is(err, biz.ErrRateLimited) {
				color, text = protocol.GameColorYellow, gameRateLimitedText
			} else {
				color, text = protocol.GameColorRed, "✘ "+biz.ResolveErrorMeta(err).WSMessage
			}
			break
		}
		color, text = renderGameRoleBindOutcome(outcome)

	case gameCommandBindUsage:
		color, text = protocol.GameColorYellow, gameBindUsageText

	default:
		return
	}

	if text == "" || notifier == nil {
		return
	}
	notifier.SendText(serverID, ev.Player, color, text)
}

// parseGameCommand 识别游戏内命令（鹊桥 player_command 事件里的 command 字段，不含前导 `/`）。
//
// 规则：大小写不敏感、空白不敏感；兼容 Forge 命名空间形式 `revolcast:bind <code>`。
// 返回值 arg 仅 gameCommandBind 有意义（绑定码原文，归一化由 biz.NormalizeGameBindCode 负责）。
func parseGameCommand(raw string) (kind gameCommandKind, arg string) {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "/")
	tokens := strings.Fields(strings.ToLower(s))
	if len(tokens) == 0 {
		return gameCommandNone, ""
	}

	// 兼容 `revolcast:bind <code>`：把 `revolcast:<sub>` 拆成两个 token，后续逻辑与 `revolcast <sub>` 一致。
	if head := tokens[0]; strings.ContainsRune(head, ':') {
		prefix, sub, _ := strings.Cut(head, ":")
		if prefix != commandPrefix || sub == "" {
			return gameCommandNone, ""
		}
		tokens = append([]string{commandPrefix, sub}, tokens[1:]...)
	} else if head != commandPrefix {
		// 非 Revolcast 命令：静默丢弃（玩家日常指令不打扰）。
		return gameCommandNone, ""
	}

	if len(tokens) < 2 {
		return gameCommandNone, ""
	}
	switch tokens[1] {
	case "bind":
		if len(tokens) != 3 {
			return gameCommandBindUsage, ""
		}
		return gameCommandBind, tokens[2]
	default:
		// 未实现的游戏内命令：静默（不回复，避免占用玩家聊天）。
		return gameCommandNone, ""
	}
}
