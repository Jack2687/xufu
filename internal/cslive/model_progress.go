// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

// ProgressStage 是面向集成方的业务阶段，与内部检查点阶段独立。
type model_ProgressStage string

const (
	model_ProgressStageSession      model_ProgressStage = "session"
	model_ProgressStageCheckout     model_ProgressStage = "checkout"
	model_ProgressStagePayment      model_ProgressStage = "payment"
	model_ProgressStageCancellation model_ProgressStage = "cancellation"
)

type model_ProgressStatus string

const (
	model_ProgressRunning   model_ProgressStatus = "running"
	model_ProgressSucceeded model_ProgressStatus = "succeeded"
	model_ProgressFailed    model_ProgressStatus = "failed"
	model_ProgressPending   model_ProgressStatus = "pending"
)

// Progress 只含进度和资金状态，不携带 Session、卡片、链接或恢复凭据。
// Status 与 Code 描述当前 Stage；取消失败不代表 Payment 失败。
type model_Progress struct {
	TraceID   string               `json:"trace_id"`
	Operation string               `json:"operation"`
	Stage     model_ProgressStage  `json:"stage"`
	Status    model_ProgressStatus `json:"status"`
	Payment   model_PaymentState   `json:"payment"`
	Code      string               `json:"code,omitempty"`
	ElapsedMS int64                `json:"elapsed_ms"`
}

// ProgressSink 在交易内同步按序调用，须快速返回；共享接收器须保证并发安全。
// 回调 panic 被隔离。进度通知不能替代 CheckpointSink 的同步持久化。
type model_ProgressSink func(model_Progress)
