package protocol

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// 本文件定义 QQ 相关消息的标准化 DTO，依据 docs/Websocket消息结构设计文档.md。
// 入站（Adapter → Backend）与出站（Backend → Adapter）共用同一份 QQSegment 定义。

// QQSegment 是消息段（文本 / @ / 图片 / 表情 / 文件）。
// Data 使用 json.RawMessage，以容纳未来可能出现的非字符串字段
// （例如某些实现 image 带 width/height 数字字段）；
// 现阶段 OneBot v11 的 data 实际均为字符串，Adapter 在转换时按需 unmarshal。
type QQSegment struct {
	Type string                     `json:"type"` // text / at / image / face / file
	Data map[string]json.RawMessage `json:"data"`
}

// NewTextSegment 构造纯文本消息段，Backend 下发 command 时优先用这个，避免手写 map。
func NewTextSegment(text string) QQSegment {
	raw, _ := json.Marshal(text)
	return QQSegment{Type: "text", Data: map[string]json.RawMessage{"text": raw}}
}

// NewAtSegment 构造 @ 某人消息段。
func NewAtSegment(qq string) QQSegment {
	raw, _ := json.Marshal(qq)
	return QQSegment{Type: "at", Data: map[string]json.RawMessage{"qq": raw}}
}

// QQMessagePayload 是 Adapter → Backend 的 QQ 消息（私聊/群聊）标准化 payload。
// 注意：这不是出站 command 的 payload，两者字段完全不同，不要复用。
type QQMessagePayload struct {
	Platform     string            `json:"platform"` // 固定 "qq"
	MessageID    string            `json:"message_id"`
	SelfID       string            `json:"self_id"` // 接收消息的机器人 QQ 号
	Conversation QQConversation    `json:"conversation"`
	Sender       QQSender          `json:"sender"`
	Message      QQMessage         `json:"message"`
	IsToMe       bool              `json:"is_to_me"`
	PlatformData map[string]string `json:"platform_data,omitempty"` // 经筛选的平台扩展信息
	EventTime    int64             `json:"event_time"`              // UTC 毫秒
	ReceivedAt   int64             `json:"received_at"`             // Adapter 收到时间，UTC 毫秒
}

// QQConversation 会话信息。
type QQConversation struct {
	Type string `json:"type"` // group / private
	ID   string `json:"id"`   // 群聊为群号，私聊为对方 QQ 号
}

// QQSender 发送者信息。
type QQSender struct {
	ID       string `json:"id"` // 发送者 QQ 号（字符串，避免大数精度问题）
	Nickname string `json:"nickname,omitempty"`
	Card     string `json:"card,omitempty"` // 群名片，仅群聊通常具有
}

// QQMessage 消息内容。
type QQMessage struct {
	Text     string      `json:"text"` // 文本表示，供命令识别与意图判断
	Segments []QQSegment `json:"segments"`
}

// QQSendPrivateMsgPayload 是 Backend → Adapter 的私聊发送指令 payload。
type QQSendPrivateMsgPayload struct {
	CommandID       string      `json:"command_id"`       // Backend 生成，Adapter 原样回传
	ReceiverID      string      `json:"receiver_id"`      // 接收者 QQ 号
	MessageSegments []QQSegment `json:"message_segments"` // 消息段数组，保持顺序
}

// QQSendPrivateMessageResultPayload 是 Adapter → Backend 的私聊发送执行结果。
type QQSendPrivateMessageResultPayload struct {
	CommandID         string `json:"command_id"`
	Status            string `json:"status"` // success / failed
	PlatformMessageID string `json:"platform_message_id,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
}

// QQ 消息类型常量（与协议文档保持一致）。
const (
	TypeQQMessagePrivate           = "event.qq.message.private"
	TypeQQMessageGroup             = "event.qq.message.group"
	TypeQQSendPrivateMessage       = "command.qq.send_private_message"
	TypeQQSendPrivateMessageResult = "result.qq.send_private_message"

	CommandStatusSuccess = "success"
	CommandStatusFailed  = "failed"

	// 统一错误码（见 WebSocket 消息结构设计文档 §4，业务层不得仅凭 error_message 判断分支）。
	ErrorCodeQQAlreadyBound = "QQ_ALREADY_BOUND"
	ErrorCodeRateLimited    = "RATE_LIMITED"
	ErrorCodeInternal       = "INTERNAL_ERROR"
)

// NewCommandID 生成 v4 风格 UUID 字符串，用作出站指令的 command_id。
// 使用 crypto/rand，避免引入额外依赖；仅作唯一标识，非安全凭据。
func NewCommandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
