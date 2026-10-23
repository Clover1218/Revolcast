package transport

import (
	"encoding/json"

	"revolcast-backend/internal/protocol"

	"go.uber.org/zap"
)

// GameNotifier 是后端向「游戏内玩家」发送私聊通知的唯一入口。
//
// 职责边界：
//   - 只负责把一条纯文本渲染成鹊桥私聊指令并投递给对应服务器连接；
//   - 不决定文案（文案由业务调用方渲染）、不重试、不等待回执；
//   - 发送失败只记日志，**不影响业务结论**（绑定/解绑以数据库事务结果为准）。
//
// 为什么不放在 biz 层：biz 不应依赖 WebSocket 运行时或鹊桥协议（见 AGENTS.md §3.1）；
// 与 QQ 指令的文案渲染一样，游戏内文案属于接入层的表达。
//
// 后续新增的游戏内通知（绑定成功、解绑、系统提示）都复用 SendText，不要另拼鹊桥 JSON。
type GameNotifier struct {
	hub *Hub
	log *zap.Logger
}

// NewGameNotifier 构造游戏内通知器。hub 为 nil 时所有发送都静默失败（便于测试装配）。
func NewGameNotifier(hub *Hub, logger *zap.Logger) *GameNotifier {
	return &GameNotifier{hub: hub, log: logger.With(zap.String("module", "GameNotifier"))}
}

// SendText 向指定游戏服务器上的玩家发送一条私聊文本。
//
// serverID 必须是 WS 已鉴权的 game_servers.id（来自事件连接身份），不接受客户端自报。
// player.UUID 优先；为空时回退 nickname（鹊桥要求两者至少一个非空）。
// 返回是否成功投递到该服务器的发送队列（不代表玩家已收到）。
func (n *GameNotifier) SendText(serverID int64, player protocol.QueqiaoPlayer, color, text string) bool {
	if n == nil || n.hub == nil {
		return false
	}
	if serverID <= 0 || text == "" || (player.UUID == "" && player.Nickname == "") {
		n.log.Warn("游戏内通知参数不完整，丢弃",
			zap.Int64("server_id", serverID),
			zap.String("player_uuid", player.UUID),
			zap.String("player_name", player.Nickname))
		return false
	}

	echo := protocol.NewCommandID()
	req := protocol.GamePrivateMessageRequest{
		API: protocol.GameAPISendPrivateMsg,
		Data: protocol.GamePrivateMessageData{
			UUID:     player.UUID,
			Nickname: player.Nickname,
			Message:  []protocol.GameTextComponent{protocol.NewGameText(text, color)},
		},
		Echo: echo,
	}
	data, err := json.Marshal(req)
	if err != nil {
		// 结构固定（字符串数组），序列化不会失败；兜底记日志避免静默。
		n.log.Warn("序列化游戏内通知失败", zap.Int64("server_id", serverID), zap.Error(err))
		return false
	}

	if !n.hub.SendToGameServerByID(serverID, data) {
		n.log.Warn("游戏内通知下发失败：服务器不在线或发送队列已满",
			zap.Int64("server_id", serverID),
			zap.String("player_uuid", player.UUID),
			zap.String("echo", echo))
		return false
	}

	n.log.Debug("游戏内通知已投递",
		zap.Int64("server_id", serverID),
		zap.String("player_uuid", player.UUID),
		zap.String("echo", echo))
	return true
}
