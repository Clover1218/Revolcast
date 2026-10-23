package protocol

import "encoding/json"

// 本文件定义「连接端 → 鹊桥」的下行指令 DTO，依据 docs/鹊桥与服务端WS连接格式详解.md
// 「连接端 → 鹊桥」章节。
//
// 关键约定：鹊桥（运行在 MC 服务端的 mod）只认原生 JSON —— 上行事件是裸 JSON，
// 下行指令同样是裸 JSON，**不套统一 Envelope**。Envelope 仅用于 Adapter / Web 两个接入端
// （见 AGENTS.md §4 与《Websocket消息结构设计文档》）。
//
// 下行失败语义：发送只保证「已投递给该服务器的 WS 连接」，不保证玩家收到；
// 业务成败不由下行结果决定（见 transport.GameNotifier）。

// 鹊桥下行 API 名。
const (
	// GameAPISendPrivateMsg 向游戏内单个玩家发送私聊消息。
	GameAPISendPrivateMsg = "send_private_msg"
	// GameAPIBroadcast 在服务器内广播消息（本期未使用，保留以明确 api 取值）。
	GameAPIBroadcast = "broadcast"
	// GameAPISendRCONCommand 在服务器内执行一条 RCON 命令（BIZ-012 状态采集用）。
	GameAPISendRCONCommand = "send_rcon_command"
)

// GameRCONListUUIDs 是状态采集使用的 RCON 命令：回显包含在线人数与带 UUID 的玩家列表。
// 回显格式见 docs/鹊桥与服务端WS连接格式详解.md「发送RCON命令」。
const GameRCONListUUIDs = "list uuids"

// Minecraft 文本组件颜色（仅列出本项目用到的取值）。
const (
	GameColorRed    = "red"
	GameColorGreen  = "green"
	GameColorYellow = "yellow"
	GameColorAqua   = "aqua"
	GameColorGray   = "gray"
	GameColorWhite  = "white"
)

// GamePrivateMessageRequest 是「连接端 → 鹊桥」的私聊消息请求体。
//
// 示例（鹊桥文档「私聊消息 · 请求体」）：
//
//	{"api":"send_private_msg","data":{"uuid":null,"nickname":"17TheWord","message":[{"text":"hi","color":"white"}]},"echo":"1"}
//
// echo 由调用方生成，鹊桥在响应中原样回传，用于日志关联；本期不等待回执。
type GamePrivateMessageRequest struct {
	API  string                 `json:"api"`
	Data GamePrivateMessageData `json:"data"`
	Echo string                 `json:"echo"`
}

// GamePrivateMessageData 是私聊消息的数据体。
// uuid 与 nickname 至少提供一个；两者都提供时鹊桥优先使用 uuid。
type GamePrivateMessageData struct {
	UUID     string              `json:"uuid,omitempty"`
	Nickname string              `json:"nickname,omitempty"`
	Message  []GameTextComponent `json:"message"`
}

// GameTextComponent 是 Minecraft 文本组件的最小实现（纯文本 + 颜色）。
// 鹊桥会在消息前自动添加 `[鹊桥]` 前缀。
type GameTextComponent struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
}

// NewGameText 构造一个文本组件。color 为空时由客户端使用默认颜色。
func NewGameText(text, color string) GameTextComponent {
	return GameTextComponent{Text: text, Color: color}
}

// GameRCONCommandRequest 是「连接端 → 鹊桥」的 RCON 命令请求体。
//
// 示例（鹊桥文档「发送RCON命令 · 请求体」）：
//
//	{"api":"send_rcon_command","data":{"command":"list uuids"},"echo":"1"}
//
// 与 send_private_msg 的差异：本 API 的**回执 `data` 是命令回显字符串**，
// 且调用方需要按 echo 关联请求与回执（见 transport 的 echo 等待表）。
type GameRCONCommandRequest struct {
	API  string              `json:"api"`
	Data GameRCONCommandData `json:"data"`
	Echo string              `json:"echo"`
}

// GameRCONCommandData 是 RCON 命令请求的数据体。
type GameRCONCommandData struct {
	Command string `json:"command"`
}

// NewGameRCONRequest 构造一条 RCON 命令请求；echo 由调用方生成，鹊桥在回执中原样返回。
func NewGameRCONRequest(command, echo string) GameRCONCommandRequest {
	return GameRCONCommandRequest{
		API:  GameAPISendRCONCommand,
		Data: GameRCONCommandData{Command: command},
		Echo: echo,
	}
}

// GameServerResponse 是鹊桥对下行指令的响应体（只取本端需要的字段）。
//
// 示例：{"code":200,"api":"send_private_msg","post_type":"response","status":"SUCCESS","message":"success","echo":"1"}
//
// 该结构仅用于 transport 层识别「这条上行消息是回执而不是事件」，不进入业务队列。
type GameServerResponse struct {
	PostType string `json:"post_type"` // 固定 "response"
	API      string `json:"api"`       // 对应请求的 api
	Code     int    `json:"code"`
	Status   string `json:"status"` // SUCCESS / FAILED
	Message  string `json:"message"`
	Echo     string `json:"echo"`
	// Data 是回执数据体，**类型随 api 变化**：send_rcon_command 为字符串（命令回显），
	// send_private_msg 为对象。因此必须用 RawMessage 承载，按 api 分别解码。
	Data json.RawMessage `json:"data"`
}

// RCONEcho 把回执的 data 解码为命令回显字符串（仅适用于 api=send_rcon_command）。
// data 缺失（null / 未携带）或不是字符串时返回 ok=false，调用方据此判定采集失败。
func (r GameServerResponse) RCONEcho() (string, bool) {
	if len(r.Data) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(r.Data, &s); err != nil {
		return "", false
	}
	return s, true
}

// GameServerPostTypeResponse 是响应体的 post_type 固定取值。
const GameServerPostTypeResponse = "response"

// GameServerStatusSuccess 是响应体的成功状态取值。
const GameServerStatusSuccess = "SUCCESS"
