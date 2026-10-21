package protocol

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
)

// 本文件定义 QQ 相关消息的标准化 DTO，依据 docs/Websocket消息结构设计文档.md。
// 与 Backend 端定义保持一致；Adapter 负责把原生 OneBot 事件转换为这些结构。

// QQSegment 消息段（文本 / @ / 图片 / 表情 / 文件）。
// 入站/出站共用。Data 使用 json.RawMessage，便于容纳非字符串字段；
// 现阶段 OneBot v11 的 data 实际为字符串，转换时按需 unmarshal。
type QQSegment struct {
	Type string                     `json:"type"`
	Data map[string]json.RawMessage `json:"data"`
}

// NewTextSegment 构造纯文本消息段。
func NewTextSegment(text string) QQSegment {
	raw, _ := json.Marshal(text)
	return QQSegment{Type: "text", Data: map[string]json.RawMessage{"text": raw}}
}

// QQMessagePayload 是 Adapter → Backend 的 QQ 消息标准化 payload。
// 注意：这不是出站 command 的 payload，两者字段完全不同，不要复用。
type QQMessagePayload struct {
	Platform     string            `json:"platform"`
	MessageID    string            `json:"message_id"`
	SelfID       string            `json:"self_id"`
	Conversation QQConversation    `json:"conversation"`
	Sender       QQSender          `json:"sender"`
	Message      QQMessage         `json:"message"`
	IsToMe       bool              `json:"is_to_me"`
	PlatformData map[string]string `json:"platform_data,omitempty"`
	EventTime    int64             `json:"event_time"`  // UTC 毫秒
	ReceivedAt   int64             `json:"received_at"` // Adapter 收到时间，UTC 毫秒
}

type QQConversation struct {
	Type string `json:"type"` // group / private
	ID   string `json:"id"`   // 群聊为群号，私聊为对方 QQ 号
}

type QQSender struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname,omitempty"`
	Card     string `json:"card,omitempty"`
}

type QQMessage struct {
	Text     string      `json:"text"`
	Segments []QQSegment `json:"segments"`
}

// QQSendPrivateMsgPayload 是 Backend → Adapter 的私聊发送指令 payload。
type QQSendPrivateMsgPayload struct {
	CommandID       string      `json:"command_id"`
	ReceiverID      string      `json:"receiver_id"`
	MessageSegments []QQSegment `json:"message_segments"`
}

// QQSendPrivateMessageResultPayload 是 Adapter → Backend 的执行结果，必须原样回传 command_id。
type QQSendPrivateMessageResultPayload struct {
	CommandID         string `json:"command_id"`
	Status            string `json:"status"` // success / failed
	PlatformMessageID string `json:"platform_message_id,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
}

const (
	CommandStatusSuccess = "success"
	CommandStatusFailed  = "failed"
)

// NewCommandID 生成 v4 风格 UUID 字符串（仅作唯一标识，非安全凭据）。
func NewCommandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
