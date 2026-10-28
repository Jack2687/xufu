package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Tier struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Threshold   int64  `json:"threshold"`
	Discount    int64  `json:"discount"`
	Description string `json:"description"`
}
type TierConfig struct {
	Items []Tier `json:"items"`
}
type Membership struct {
	Current     Tier       `json:"current"`
	Automatic   Tier       `json:"automatic"`
	Next        *Tier      `json:"next"`
	Spent       int64      `json:"spent"`
	Remaining   int64      `json:"remaining"`
	Progress    int        `json:"progress"`
	Source      string     `json:"source"`
	Until       *time.Time `json:"until"`
	FeeOverride *int64     `json:"fee_override"`
	Tiers       []Tier     `json:"tiers"`
}

func defaultTiers() TierConfig {
	return TierConfig{[]Tier{
		{"L1", "启程会员", 0, 100, "从第一笔成功订单开始积累"},
		{"L2", "银阶会员", 200000, 95, "服务费减免 5%"},
		{"L3", "金阶会员", 500000, 90, "服务费减免 10%"},
		{"L4", "铂阶会员", 1000000, 85, "服务费减免 15%"},
		{"L5", "钻阶会员", 2000000, 80, "服务费减免 20%"},
		{"L9", "合作伙伴", 10000000, 20, "服务费减免 80%"},
	}}
}
func loadTiers(ctx context.Context, q rowQuerier) (TierConfig, error) {
	var raw []byte
	if e := q.QueryRow(ctx, "SELECT value FROM settings WHERE key='tiers'").Scan(&raw); e != nil {
		return TierConfig{}, e
	}
	var c TierConfig
	if e := json.Unmarshal(raw, &c); e != nil {
		return c, e
	}
	if len(c.Items) == 0 {
		return c, fmt.Errorf("missing membership tiers")
	}
	sort.Slice(c.Items, func(i, j int) bool { return c.Items[i].Threshold < c.Items[j].Threshold })
	for i := range c.Items {
		if c.Items[i].Code == "" {
			c.Items[i].Code = fmt.Sprintf("L%d", i+1)
		}
	}
	return c, nil
}

// Only settled charges tied to this user's successful order count. Refunds reduce
// that order's growth to a minimum of zero; deposits and manual adjustments do not.
const membershipSpendSQL = `SELECT l.user_id,l.order_id,GREATEST(0,-sum(l.amount))::bigint AS spent
 FROM ledger l JOIN orders o ON o.id=l.order_id AND o.user_id=l.user_id
 WHERE o.status='succeeded' AND ((l.kind='charge' AND l.amount<0) OR (l.kind='refund' AND l.amount>0))
 GROUP BY l.user_id,l.order_id`

func membershipFrom(c TierConfig, spent int64, code string, until *time.Time, fee *int64) Membership {
	m := Membership{Spent: spent, Current: c.Items[0], Automatic: c.Items[0], Tiers: c.Items, Source: "automatic", FeeOverride: fee, Progress: 100}
	for _, t := range c.Items {
		if spent >= t.Threshold {
			m.Automatic = t
		}
	}
	m.Current = m.Automatic
	if code != "" && (until == nil || until.After(time.Now())) {
		for _, t := range c.Items {
			if t.Code == code {
				m.Current = t
				m.Source = "manual"
				m.Until = until
			}
		}
	}
	for _, t := range c.Items {
		if t.Threshold > m.Automatic.Threshold {
			next := t
			m.Next = &next
			m.Remaining = t.Threshold - spent
			m.Progress = int((spent - m.Automatic.Threshold) * 100 / (t.Threshold - m.Automatic.Threshold))
			break
		}
	}
	return m
}
func membershipFor(ctx context.Context, q rowQuerier, id string) (Membership, error) {
	c, e := loadTiers(ctx, q)
	if e != nil {
		return Membership{}, e
	}
	var spent int64
	var code string
	var until *time.Time
	var fee *int64
	e = q.QueryRow(ctx, `SELECT COALESCE((SELECT sum(s.spent) FROM (`+membershipSpendSQL+`) s WHERE s.user_id=u.id),0),u.membership_code,u.membership_until,u.fee_override FROM users u WHERE u.id=$1`, id).Scan(&spent, &code, &until, &fee)
	if e != nil {
		return Membership{}, e
	}
	return membershipFrom(c, spent, code, until, fee), nil
}
func feeFor(m Membership, standard int64) int64 {
	if m.FeeOverride != nil {
		return *m.FeeOverride
	}
	return (standard*m.Current.Discount + 50) / 100
}
func (a *App) membership(w http.ResponseWriter, r *http.Request) {
	m, e := membershipFor(r.Context(), a.DB, current(r).ID)
	if e != nil {
		fail(w, 500, "读取会员信息失败")
		return
	}
	products, e := a.query(r.Context(), "SELECT id,name,description,fee,enabled FROM products ORDER BY sort")
	if e != nil {
		fail(w, 500, "读取报价失败")
		return
	}
	for _, p := range products {
		standard := p["fee"].(int64)
		p["member_fee"] = feeFor(m, standard)
		p["saved"] = standard - feeFor(m, standard)
	}
	growth, e := a.query(r.Context(), `SELECT o.id,o.product_name,o.updated_at,s.spent FROM (`+membershipSpendSQL+`) s JOIN orders o ON o.id=s.order_id WHERE s.user_id=$1 ORDER BY o.updated_at DESC LIMIT 100`, current(r).ID)
	if e != nil {
		fail(w, 500, "读取成长记录失败")
		return
	}
	out(w, 200, M{"membership": m, "products": products, "growth": growth, "unit": "cent_R", "basis": "settled_service_spend"})
}
func (a *App) me(w http.ResponseWriter, r *http.Request) {
	m, e := membershipFor(r.Context(), a.DB, current(r).ID)
	if e != nil {
		fail(w, 500, "读取会员信息失败")
		return
	}
	b, _ := json.Marshal(current(r))
	v := M{}
	json.Unmarshal(b, &v)
	v["membership"] = m
	out(w, 200, v)
}
func (a *App) adminMembership(w http.ResponseWriter, r *http.Request) {
	c, e := loadTiers(r.Context(), a.DB)
	if e != nil {
		fail(w, 500, "等级配置读取失败")
		return
	}
	rows, e := a.query(r.Context(), `SELECT u.id,u.email,u.name,u.role,u.status,u.fee_override,u.membership_code,u.membership_until,COALESCE(s.spent,0)::bigint AS spent
 FROM users u LEFT JOIN (SELECT user_id,sum(spent)::bigint AS spent FROM (`+membershipSpendSQL+`) s GROUP BY user_id) s ON s.user_id=u.id ORDER BY u.created_at DESC`)
	if e != nil {
		fail(w, 500, "会员列表读取失败")
		return
	}
	distribution := map[string]int{}
	total := 0
	for _, u := range rows {
		var fee *int64
		var until *time.Time
		if x, ok := u["fee_override"].(int64); ok {
			fee = &x
		}
		if x, ok := u["membership_until"].(time.Time); ok {
			until = &x
		}
		m := membershipFrom(c, u["spent"].(int64), u["membership_code"].(string), until, fee)
		u["membership"] = m
		if u["role"] == "member" {
			distribution[m.Current.Code]++
			total++
		}
	}
	out(w, 200, M{"items": rows, "config": c, "distribution": distribution, "customers": total})
}
func (a *App) setMembership(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Code   string     `json:"code"`
		Until  *time.Time `json:"until"`
		Reason string     `json:"reason"`
	}
	if !read(w, r, &v) {
		return
	}
	v.Reason = strings.TrimSpace(v.Reason)
	if len(v.Reason) < 2 || len(v.Reason) > 500 {
		fail(w, 400, "请填写调整原因（2～500 字节）")
		return
	}
	if v.Until != nil && !v.Until.After(time.Now()) {
		fail(w, 400, "到期时间必须晚于现在")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(782518860)"); e != nil {
			return nil, e
		}
		c, e := loadTiers(r.Context(), tx)
		if e != nil {
			return nil, e
		}
		found := v.Code == ""
		for _, t := range c.Items {
			if t.Code == v.Code {
				found = true
			}
		}
		if !found {
			return nil, bad("等级不存在")
		}
		if v.Code == "" {
			v.Until = nil
		}
		tag, e := tx.Exec(r.Context(), "UPDATE users SET membership_code=$2,membership_until=$3,updated_at=now() WHERE id=$1", r.PathValue("id"), v.Code, v.Until)
		if e != nil {
			return nil, e
		}
		if tag.RowsAffected() == 0 {
			return nil, businessError{404, "账户不存在"}
		}
		detail, _ := json.Marshal(M{"code": v.Code, "until": v.Until, "reason": v.Reason})
		e = a.audit(r.Context(), tx, r, "membership.adjusted", r.PathValue("id"), string(detail))
		return M{"ok": true}, e
	})
}
