package platform

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

var paymentPhases = map[string]string{
	"accepted": "申请受理与资料校验", "queue": "等待办理资源", "funds": "服务费处理", "complete": "订阅流程结束",
	"proxy": "分配办理网络", "checkout": "生成结账信息", "network": "核对出口一致性",
	"opening": "打开付款页面", "billing": "填写账单资料", "pricing": "核对金额与税费",
	"confirming": "提交付款", "submitted": "付款请求已提交", "checking": "核实付款结果",
	"result": "付款处理结果", "restart": "重建会话与结账信息", "renewal": "取消自动续费",
}
var traceCodes = map[string]string{
	"hold": "服务费已冻结", "release": "服务费已解冻", "charge": "服务费已结算", "kept": "按客户选择保留自动续费",
	"waiting_proxy": "等待对应地区的可用代理", "retry": "按配置间隔等待重试", "network_restarted": "未提交扣款，重建网络会话后重新提链",
	"": "", "network": "网络连接异常", "upstream_busy": "上游繁忙", "upstream_unavailable": "上游暂不可用",
	"invalid_session": "账号授权失效", "invalid_response": "响应未能确认", "access_denied": "上游拒绝访问",
	"proxy_country_mismatch": "出口地区不匹配", "proxy_unavailable": "办理网络不可用", "request_rejected": "请求被拒绝",
	"disabled": "已确认关闭", "pending": "等待再次核实", "review": "需要核实", "completed": "已完成",
}

// Critical milestones share the business transaction so committed financial or
// terminal outcomes cannot be shown differently from their diagnostic record.
func paymentTraceTx(ctx context.Context, tx pgx.Tx, id, phase, state, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,elapsed_ms)
 SELECT $1,$2,$3,$4,COALESCE((SELECT network_restarts+1 FROM payment_runs WHERE order_id=$1),1),0`, id, phase, state, code)
	return err
}

// Only fixed labels/codes are stored. No upstream URL, raw error or credential is
// accepted by this path. A short timeout prevents diagnostics holding business work.
func (a *App) paymentTrace(ctx context.Context, id, phase, state, code string, elapsed time.Duration) {
	if _, ok := paymentPhases[phase]; !ok {
		return
	}
	switch state {
	case "running", "completed", "failed", "review", "waiting":
	default:
		return
	}
	if _, ok := traceCodes[code]; !ok {
		if _, ok = paymentMessages[code]; !ok {
			code = "unclassified"
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
	defer cancel()
	a.DB.Exec(save, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,elapsed_ms)
 SELECT $1,$2,$3,$4,COALESCE((SELECT network_restarts+1 FROM payment_runs WHERE order_id=$1),1),$5
 WHERE EXISTS(SELECT 1 FROM orders WHERE id=$1)`, id, phase, state, code, max(int64(0), elapsed.Milliseconds()))
}

func (a *App) paymentTraceDetails(ctx context.Context, id string) []M {
	rows, e := a.query(ctx, "SELECT phase,state,code,round,elapsed_ms,created_at FROM payment_trace_events WHERE order_id=$1 ORDER BY id", id)
	if e != nil {
		return []M{}
	}
	for _, row := range rows {
		row["label"] = paymentPhases[str(row, "phase")]
		code := str(row, "code")
		description, ok := traceCodes[code]
		if !ok {
			description = paymentMessages[code]
		}
		if description == "" && code != "" {
			description = "阶段未完成，请结合前后记录核实"
		}
		row["description"] = description
	}
	return rows
}
