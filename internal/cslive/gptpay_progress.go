// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	time "time"
)

// 每笔交易维护自己的进度；不解析日志，也不向接收器暴露可变的交易上下文。
type gptpay_progressTracker struct {
	sink      model_ProgressSink
	traceID   string
	operation string
	started   time.Time
	current   model_ProgressStage
	states    map[model_ProgressStage]model_ProgressStatus
	cp        *model_Checkpoint
}

func (p *gptpay_progressTracker) emit(stage model_ProgressStage, status model_ProgressStatus, code string) {
	if p == nil {
		return
	}
	// 一次业务阶段只报告一次同种状态；已确认的成功不被后续异常覆盖。
	if previous := p.states[stage]; previous == status || previous == model_ProgressSucceeded {
		return
	}
	p.current = stage
	p.states[stage] = status
	if p.sink == nil {
		return
	}
	if code != "" {
		code = observability_SafeErrorCode(code)
	}
	value := model_Progress{
		TraceID: p.traceID, Operation: p.operation, Stage: stage, Status: status,
		Payment: model_PaymentState(p.cp.Payment), Code: code,
		ElapsedMS: max(int64(0), time.Since(p.started).Milliseconds()),
	}
	// 通知只用于展示，接收方异常不得中断付款或阻止检查点落盘。
	func() {
		defer func() { _ = recover() }()
		p.sink(value)
	}()
}

func (p *gptpay_progressTracker) finish(code string) {
	if p == nil {
		return
	}
	if p.cp.Payment == "charged" {
		p.emit(model_ProgressStagePayment, model_ProgressSucceeded, "")
		if p.cp.CancelRequested {
			status := model_ProgressPending
			if p.cp.Renewal == "off" {
				status = model_ProgressSucceeded
				code = ""
			} else if p.states[model_ProgressStageCancellation] == model_ProgressRunning && p.cp.Renewal == "on" && code != "cancellation_pending" {
				status = model_ProgressFailed
			}
			p.emit(model_ProgressStageCancellation, status, code)
		}
		return
	}
	if p.current == "" || p.states[p.current] != model_ProgressRunning {
		return
	}
	status := model_ProgressFailed
	if p.current == model_ProgressStagePayment {
		switch model_PaymentState(p.cp.Payment) {
		case model_PaymentUnknown, model_PaymentPending, model_PaymentAuthorized, model_PaymentActionRequired:
			status = model_ProgressPending
		}
	}
	p.emit(p.current, status, code)
}
