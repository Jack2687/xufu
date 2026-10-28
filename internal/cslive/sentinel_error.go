// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	time "time"
)

// Package sentinel 实现固定浏览器画像的证明生成器与受限寄存器 VM。

// Error 对外只暴露稳定错误码；Cause 仅用于离线诊断，不进入安全日志。
type sentinel_Error struct {
	Code  string
	Cause error
}

func (e *sentinel_Error) Error() string { return e.Code }
func (e *sentinel_Error) Unwrap() error { return e.Cause }

func sentinel_checkDeadline(deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return &sentinel_Error{Code: "deadline_exceeded"}
	}
	return nil
}

func sentinel_resourceError(reason string) error {
	return &sentinel_Error{Code: "sentinel_turnstile_error", Cause: fmt.Errorf("%s", reason)}
}
