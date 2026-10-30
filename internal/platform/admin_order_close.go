package platform

import (
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"unicode/utf8"
)

// This closes local fulfillment and releases this platform's held service fee.
// It never cancels or refunds a provider transaction, or erases its submitted flag.
func (a *App) adminCloseOrder(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason   string `json:"reason"`
		Accepted bool   `json:"accepted"`
	}
	if !read(w, r, &input) {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if !input.Accepted || utf8.RuneCountInString(input.Reason) < 2 || utf8.RuneCountInString(input.Reason) > 300 {
		fail(w, 400, "请填写关闭原因并确认停止本站办理、释放冻结服务费；该操作不撤销银行卡付款")
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		paymentError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); err != nil {
		paymentError(w, err)
		return
	}
	var status, state, fee, attempt, code string
	var submitted bool
	err = tx.QueryRow(ctx, `SELECT o.status,p.state,p.fee_state,p.attempt_id,p.error_code,p.submitted
 FROM orders o JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1 FOR UPDATE OF o,p`, id).Scan(&status, &state, &fee, &attempt, &code, &submitted)
	if err == pgx.ErrNoRows {
		fail(w, 404, "付款订单不存在")
		return
	}
	if err != nil {
		paymentError(w, err)
		return
	}
	if code == "admin_closed" {
		out(w, 200, M{"ok": true, "replayed": true})
		return
	}
	if status == "succeeded" || state == "succeeded" || fee != "held" || state == "failed" || state == "cancelled" {
		fail(w, 409, "当前订单已结束或服务费已结算/释放，不能重复关闭退款")
		return
	}
	// Preserve the original result category for investigation, before marking local
	// processing cancelled. This snapshot is admin-only; no credentials are copied.
	if _, err = tx.Exec(ctx, `INSERT INTO order_closures(order_id,actor_id,reason,prior_status,prior_payment_state,payment_submitted)
 VALUES($1,$2,$3,$4,$5,$6)`, id, current(r).ID, input.Reason, status, state, submitted); err != nil {
		paymentError(w, err)
		return
	}
	if err = paymentFunds(ctx, tx, id, attempt, "release"); err != nil {
		paymentError(w, err)
		return
	}
	statements := []string{
		"UPDATE ledger SET description='管理员关闭订单，释放冻结服务费；不代表银行卡撤销付款',actor_id=$2 WHERE reference='payment:'||(SELECT attempt_id FROM payment_runs WHERE order_id=$1)||':release'",
		"UPDATE payment_runs SET state='cancelled',fee_state='released',error_code='admin_closed',legacy_checkpoint_enc='',updated_at=now() WHERE order_id=$1",
		"UPDATE orders SET status='cancelled',updated_at=now() WHERE id=$1",
		"UPDATE jobs SET state='failed',error_code='admin_closed',lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1",
		"UPDATE subscription_protocol SET lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1",
		"UPDATE subscription_runs SET credentials_enc='',lease_token=NULL,lease_until=NULL,renewal_state='not_started',updated_at=now() WHERE order_id=$1",
		"UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1",
		"DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)",
	}
	for i, sql := range statements {
		args := []any{id}
		if i == 0 {
			args = append(args, current(r).ID)
		}
		if _, err = tx.Exec(ctx, sql, args...); err != nil {
			paymentError(w, err)
			return
		}
	}
	if err = paymentEventTx(ctx, tx, id, "admin_closed", "admin_closed"); err == nil {
		err = paymentTraceTx(ctx, tx, id, "complete", "completed", "admin_closed")
	}
	if err == nil {
		err = a.audit(ctx, tx, r, "order.closed", id, fmt.Sprintf("停止本站办理并释放冻结服务费；原付款状态=%s，已提交=%t；原因：%s", state, submitted, input.Reason))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		paymentError(w, err)
		return
	}
	// Durable fencing precedes process cancellation, so late callbacks cannot
	// overwrite closure or settle/release the service fee a second time.
	a.paymentMu.Lock()
	if s := a.payments[id]; s != nil {
		s.cancel()
	}
	if v := a.paymentActions[id]; v != nil {
		v.mu.Lock()
		v.cancel()
		v.frame = nil
		v.state = "closed"
		v.mu.Unlock()
	}
	a.paymentMu.Unlock()
	out(w, 200, M{"ok": true, "released": true, "payment_submitted": submitted})
}
