package queue

import (
	"encoding/json"
	"time"
)

// Task 是 readPump（生产者）投递给业务层（消费者）的最小单元。
//
// Payload 保持原始 JSON（json.RawMessage），业务层按 Type 自行反序列化，
// 这样 queue 包对业务零耦合，也方便后续把某个 Type 单独分流到不同 Worker 池。
type Task struct {
	TraceID    string          // 链路追踪 ID，贯穿日志与 Action 回传闭环
	ClientID   string          // 来源客户端身份 ID，用于把 Action / 指令回传给正确的接入端
	Type       string          // envelope.type，例如 "event.message.group"
	Payload    json.RawMessage // envelope.payload 原始字节
	ReceivedAt time.Time       // 入队时间，用于统计排队延迟
}

// Decode 把 Payload 反序列化为具体业务结构体，供 Handler 直接使用。
func (t Task) Decode(v any) error {
	return json.Unmarshal(t.Payload, v)
}
