package protocol

import "encoding/json"

type Envelope struct {
	Type    string          `json:"type"`     // "event", "action", "ping", "pong"
	TraceID string          `json:"trace_id"` // 链路追踪 ID
	Payload json.RawMessage `json:"payload"`  // 实际的数据体
}
