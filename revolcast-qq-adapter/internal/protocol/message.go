package protocol

import "encoding/json"

// Envelope 统一通信信封
type Envelope struct {
	Version string          `json:"version"`
	Type    string          `json:"type"`
	TraceID string          `json:"trace_id"` // 链路追踪 ID
	Payload json.RawMessage `json:"payload"`  // 实际的数据体
}
