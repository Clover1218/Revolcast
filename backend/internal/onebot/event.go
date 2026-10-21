// internal/onebot/event.go
package onebot

import "encoding/json"

type Event struct {
	PostType    string          `json:"post_type"`    // message / notice / request / meta_event
	MessageType string          `json:"message_type"` // group / private （仅 post_type=message）
	SubType     string          `json:"sub_type"`     // normal / anonymous / notice 子类型等
	SelfID      int64           `json:"self_id"`
	UserID      int64           `json:"user_id"`
	GroupID     int64           `json:"group_id,omitempty"`
	MessageID   int64           `json:"message_id,omitempty"`
	RawMessage  string          `json:"raw_message,omitempty"` // 纯文本，便于关键字路由
	Message     json.RawMessage `json:"message"`               // string 或 segment 数组
	Time        int64           `json:"time"`
	OperatorID  int64           `json:"operator_id,omitempty"` // notice 事件的操作者
	Duration    int64           `json:"duration,omitempty"`    // 禁言时长
	TargetID    int64           `json:"target_id,omitempty"`   // 被禁言/被踢者
}

func Parse(raw json.RawMessage) (*Event, error) {
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
