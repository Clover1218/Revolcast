package protocol

import "time"

// 本文件定义鹊桥（连接端）上报的原始游戏事件 DTO，依据 docs/鹊桥与服务端WS连接格式详解.md。
//
// 关键约定：这些事件以「原始 JSON 直发」（不带统一 Envelope 包装），由 transport 层
// 按 payload 内的 sub_type 派生出 task.Type（event.game.<sub_type>），再交由 biz 层转换落库。
// 不要把这些原始事件与统一 Envelope（version/type/trace_id/payload）混为一谈。

// QueqiaoPlayer 是原始事件中的玩家对象。仅抽取审计所需字段（uuid/nickname/is_op），
// 其余坐标/血量等字段在鹊桥原始 JSON 中存在但本集合不留存。
type QueqiaoPlayer struct {
	Nickname string `json:"nickname"`
	UUID     string `json:"uuid"`
	IsOp     bool   `json:"is_op"`
}

// GameEventPayload 是鹊桥上报的原始游戏事件（PlayerJoin/Quit/Chat/Command 共用同一结构）。
// 公共字段（timestamp/post_type/event_name/server_name/.../sub_type/player）所有事件均携带；
// message/raw_message/message_id/command 仅聊天/命令事件携带，缺省为零值（空串）。
type GameEventPayload struct {
	Timestamp     int64         `json:"timestamp"`      // 事件时间戳（秒级 Unix）
	PostType      string        `json:"post_type"`      // notice / message
	EventName     string        `json:"event_name"`     // PlayerJoinEvent 等
	ServerName    string        `json:"server_name"`    // 仅展示/自检，不参与 server_id 解析
	ServerVersion string        `json:"server_version"` // 1.20.1
	ServerType    string        `json:"server_type"`    // forge
	SubType       string        `json:"sub_type"`       // player_join / player_quit / player_chat / player_command
	Player        QueqiaoPlayer `json:"player"`

	// 以下字段仅特定事件携带，缺省为空串。
	MessageID  string `json:"message_id"`  // 聊天/命令事件
	RawMessage string `json:"raw_message"` // 聊天：Minecraft 文本组件 JSON；命令：Java 对象 toString（无业务价值）
	Message    string `json:"message"`     // 聊天事件：纯文本
	Command    string `json:"command"`     // 命令事件：玩家输入的命令（不含前导 /）
}

// 游戏事件类型常量（内部路由用 task.Type，符合 AGENTS.md event.* 约定）。
// 由 transport 层按 sub_type 派生：event.game.<sub_type>。
const (
	TypeGamePlayerJoin    = "event.game.player_join"
	TypeGamePlayerQuit    = "event.game.player_quit"
	TypeGamePlayerChat    = "event.game.player_chat"
	TypeGamePlayerCommand = "event.game.player_command"
)

// GameEventPushPayload 是「游戏事件落库成功后推送给 Web 端（已登录用户）」的 Envelope.Payload。
//
// 与鹊桥原始事件的差异：
//   - 增加 id / cursor：与 HTTP 事件查询（GET .../events 的 items[]）**完全一致**的事件身份，
//     二者都取自同一条持久化记录，前端据此去重与续页；
//   - 增加 server_code / server_id：前端以 server_code 订阅，事件归属由服务端已鉴权身份派生；
//   - 时间为 UTC RFC3339 固定毫秒字符串（见 FormatEventTime），与 HTTP 查询输出同一格式，
//     且与 MongoDB BSON Date 的毫秒精度一致；
//   - 只暴露审计与展示所需字段，不透传 server_version / post_type 等适配层字段。
//
// 消息外层仍为统一 Envelope：type = event.game.<event_type>，trace_id 沿用产生该事件的链路。
// trace_id 仅用于链路追踪，**不得**作为事件身份或去重键。
type GameEventPushPayload struct {
	ID         string            `json:"id"`          // 持久化文档 _id 的 24 位十六进制，事件统一身份
	Cursor     string            `json:"cursor"`      // 分页游标，与 HTTP items[].cursor 相同
	ServerCode string            `json:"server_code"` // 服务器短标识，与订阅路径参数一致
	ServerID   int64             `json:"server_id"`   // game_servers.id
	EventType  string            `json:"event_type"`  // player_join / player_quit / player_chat / player_command
	Player     QueqiaoPlayer     `json:"player"`      // 玩家快照（uuid/nickname/is_op）
	OccurredAt string            `json:"occurred_at"` // 事件发生时间，UTC RFC3339 毫秒
	ReceivedAt string            `json:"received_at"` // 后端收到时间，UTC RFC3339 毫秒（与落库值同一时刻）
	Data       GameEventPushData `json:"data"`        // 按事件类型填充，join/quit 为空对象
}

// EventTimeLayout 是游戏事件时间字段对外的**唯一**格式：UTC，固定 3 位毫秒。
//
// 不用 RFC3339Nano：它裁剪尾部 0，同一字段可能出现 0/3/6/9 位小数，既与 BSON Date 的毫秒精度
// 不对齐，也让前端拿到长度不一的字符串（按字符串比较排序会出错）。
const EventTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatEventTime 把事件时间格式化为对外统一格式（HTTP 与 WebSocket 共用）。
// 输入为 UTC 时刻；会先 Truncate 到毫秒，保证与写入 MongoDB 前的截断、以及游标里的 t 完全一致。
func FormatEventTime(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(EventTimeLayout)
}

// GameEventPushData 是推送给前端的差异化事件内容（零值字段省略）。
type GameEventPushData struct {
	Message   string `json:"message,omitempty"`    // 仅 player_chat：纯文本
	Command   string `json:"command,omitempty"`    // 仅 player_command：命令内容（不含前导 /）
	MessageID string `json:"message_id,omitempty"` // 聊天/命令事件
	// RawMessage 仅 player_chat 下发（Minecraft 文本组件 JSON，可用于富文本渲染）；
	// player_command 的 raw_message 是 Java 对象 toString，无业务价值，不下发。
	RawMessage string `json:"raw_message,omitempty"`
}
