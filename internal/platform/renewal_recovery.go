package platform

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type renewalRecovery struct {
	orders  []string
	started bool
	taskID  string
}

// Called under the shared admission lock. Only paid orders whose renewal worker
// has stopped may hand off. Never reset a cancellation fence on new credentials.
func renewalRecoveryTx(ctx context.Context, tx pgx.Tx, user, email, accountHash string) (renewalRecovery, error) {
	result := renewalRecovery{}
	rows, err := tx.Query(ctx, `SELECT o.id,o.user_id,COALESCE(p.state,''),COALESCE(s.renewal_state,''),
 COALESCE(s.account_hash,''),COALESCE(s.cancel_started,false),s.lease_token IS NOT NULL,COALESCE(s.renewal_task_id,'')
 FROM orders o LEFT JOIN payment_runs p ON p.order_id=o.id LEFT JOIN subscription_runs s ON s.order_id=o.id
 WHERE o.workflow='subscription' AND (o.email=$1 OR s.account_hash=$2)
 AND (o.status IN ('queued','processing','awaiting_payment','review') OR o.renewal IN ('review','retrying'))
 ORDER BY o.id FOR UPDATE OF o`, email, accountHash)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner, payment, state, oldHash, task string
		var started, leased bool
		if err = rows.Scan(&id, &owner, &payment, &state, &oldHash, &started, &leased, &task); err != nil {
			return result, err
		}
		if owner != user {
			return result, businessError{409, "该账号已有其他用户的未结束申请"}
		}
		if payment != "succeeded" || (state != "review" && state != "delegated") || leased {
			return result, businessError{409, "该账号有办理中或付款结果待核实的订阅申请，请先处理原申请"}
		}
		if oldHash != "" && oldHash != accountHash {
			return result, businessError{409, "请使用原订阅账号对应空间的 Session"}
		}
		// Older rows did not retain a workspace fingerprint. A no-write failure
		// can bind fresh authorization from the same customer/email. An uncertain
		// legacy write requires manual identity verification, never a second POST.
		if oldHash == "" && started {
			return result, businessError{409, "原申请已提交过关闭操作，需先由运营核对原账号空间，请勿重复提交"}
		}
		if task != "" {
			if result.taskID != "" && result.taskID != task {
				return result, businessError{409, "存在多条续费记录，请联系运营核实"}
			}
			result.taskID = task
		}
		result.orders = append(result.orders, id)
		result.started = result.started || started
	}
	return result, rows.Err()
}

func (r renewalRecovery) attach(ctx context.Context, tx pgx.Tx, taskID, accountHash string) error {
	if len(r.orders) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE subscription_runs SET renewal_state='delegated',renewal_task_id=$2,
 account_hash=$3,credentials_enc='',updated_at=now() WHERE order_id=ANY($1)`, r.orders, taskID, accountHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET status='succeeded',renewal='retrying',updated_at=now() WHERE id=ANY($1)", r.orders); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO order_events(order_id,stage,message)
 SELECT unnest($1::text[]),'renewal','订阅已成功，已转入续费管理继续处理；不会再次付款或收取服务费。'`, r.orders)
	return err
}

func syncRenewalRecoveryTx(ctx context.Context, tx pgx.Tx, taskID, state string) error {
	if state != "disabled" && state != "review" && state != "no_subscription" {
		return nil
	}
	renewal, message := "review", "订阅成功，取消自动续费失败。请在续费管理更新 Session 继续核实，无需重新付款。"
	if state == "disabled" {
		renewal, message = "disabled", "已通过续费管理确认自动续费关闭，当前订阅有效期保留。"
	} else if state == "no_subscription" {
		message = "订阅付款已成功，但本次未查到有效订阅，未将原订单标记为续费已关闭，请联系运营核实账号空间。"
	}
	// The admission lock also serializes this finalization against fresh Session
	// submissions. Stale workers cannot sync unless their task update succeeded.
	_, err := tx.Exec(ctx, `WITH changed AS (
 UPDATE subscription_runs s SET renewal_state=$2,cancel_started=s.cancel_started OR t.cancel_started,updated_at=now()
 FROM renewal_tasks t WHERE s.renewal_task_id=t.id AND t.id=$1 RETURNING s.order_id
), orders_changed AS (
 UPDATE orders SET status='succeeded',renewal=$2,updated_at=now() WHERE id IN (SELECT order_id FROM changed) RETURNING id
)
INSERT INTO order_events(order_id,stage,message) SELECT id,'renewal',$3 FROM orders_changed`, taskID, renewal, message)
	return err
}
