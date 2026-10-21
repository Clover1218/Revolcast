package protocol

// ===== 传输控制帧（无方向前缀，保留字，不允许被业务占用）=====
const (
	TypeHello = "hello"
	TypePing  = "ping"
	TypePong  = "pong"
)

// ===== QQ Adapter → Backend =====
const (
	TypeEventQQMessageGroup   = "event.qq.message.group"
	TypeEventQQMessagePrivate = "event.qq.message.private"
	TypeEventQQNotice         = "event.qq.notice"  // 所有 notice 子类型统一
	TypeEventQQRequest        = "event.qq.request" // 所有 request 子类型统一
	TypeEventQQMeta           = "event.qq.meta"    // 心跳等，Adapter 可先过滤
	TypeEventQQUnknown        = "event.qq.unknown"
)

// ===== Backend → QQ Adapter =====
const (
	TypeCommandQQSendGroupMsg   = "command.qq.send_group_msg"
	TypeCommandQQSendPrivateMsg = "command.qq.send_private_msg"
	TypeCommandQQSetGroupBan    = "command.qq.set_group_ban"
	TypeCommandQQDeleteMsg      = "command.qq.delete_msg"
	TypeCommandQQGetGroupMember = "command.qq.get_group_member"
)

// ===== QQ Adapter → Backend（回执）=====
const (
	TypeResultQQ = "result.qq"
)

// ===== MC Server → Backend =====
const (
	TypeEventMCPlayerJoin    = "event.mc.player.join"
	TypeEventMCPlayerLeave   = "event.mc.player.leave"
	TypeEventMCServerMessage = "event.mc.server.message"
	TypeEventMCServerStatus  = "event.mc.server.status"
	TypeEventMCBindVerify    = "event.mc.bind.verify"
	TypeEventMCUnknown       = "event.mc.unknown"
)

// ===== Backend → MC Server =====
const (
	TypeCommandMCBroadcast  = "command.mc.broadcast"
	TypeCommandMCPlayerKick = "command.mc.player.kick"
	TypeCommandMCPlayerMsg  = "command.mc.player.send_private"
)

// ===== MC Server → Backend（回执）=====
const (
	TypeResultMC = "result.mc"
)

// ===== Frontend → Backend =====
const (
	TypeRequestFEServerStatus   = "request.fe.server.status"
	TypeRequestFEMessageHistory = "request.fe.message.history"
)

// ===== Backend → Frontend =====
const (
	TypeResponseFEServerStatus   = "response.fe.server.status"
	TypeResponseFEMessageHistory = "response.fe.message.history"
	TypeEventFEServerStatus      = "event.fe.server.status" // 服务端主动推送
)
