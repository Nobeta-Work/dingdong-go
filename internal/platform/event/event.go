// Package event 定义跨服务事件信封，不包含订单或库存的业务实现。
package event

import (
	"encoding/json"
	"time"
)

// Envelope 遵循 PRD 的统一事件格式；重投必须沿用原 EventID。
type Envelope struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   string          `json:"aggregateId"`
	OccurredAt    time.Time       `json:"occurredAt"`
	SchemaVersion int             `json:"schemaVersion"`
	Payload       json.RawMessage `json:"payload"`
	TraceID       string          `json:"traceId"`
}
