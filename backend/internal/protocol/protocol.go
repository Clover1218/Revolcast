package protocol

import "encoding/json"

type Envelope struct {
	Version string          `json:"version"`  // 版本，默认v1
	Type    string          `json:"type"`     // 种类
	TraceID string          `json:"trace_id"` // 链路追踪 ID
	Payload json.RawMessage `json:"payload"`  // 实际的数据体
}
