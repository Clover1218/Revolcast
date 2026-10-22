// internal/protocol/event.go
package protocol

import "encoding/json"

// EventKind 是 Adapter 能识别的“粗分类”，只用于日志和基础路由提示。
// Backend 不应依赖它做业务决策——真正的解析基于 Raw。
const (
	EventKindGroupMessage   = "group_message"   // message/group
	EventKindPrivateMessage = "private_message" // message/private
	EventKindGroupNotice    = "group_notice"    // notice/group_*
	EventKindGroupRequest   = "group_request"   // request/group
	EventKindFriendRequest  = "friend_request"  // request/friend
	EventKindMeta           = "meta_event"      // 心跳/生命周期
)

type EventPayload struct {
	// Adapter 填充的连接级上下文（不可被 Raw 覆盖）
	Kind       string `json:"kind"`        // 粗分类，见上方常量
	ClientID   string `json:"client_id"`   // 来源客户端身份 ID，Backend 必须校验与连接一致
	SelfID     int64  `json:"self_id"`     // 机器人自身 QQ 号（从 OneBot 事件取，没有则填 0）
	ReceivedAt int64  `json:"received_at"` // Adapter 收到事件的 Unix ms

	// 原始 OneBot 事件，不做任何拆解。
	// Backend 用统一的 OneBotEvent struct 去 unmarshal 这个 Raw。
	Raw json.RawMessage `json:"raw"`
}
