package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

func validBilling(v M) bool {
	if str(v, "country") != "US" || !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(str(v, "state")) || !regexp.MustCompile(`^\d{5}(-\d{4})?$`).MatchString(str(v, "postal_code")) {
		return false
	}
	if !strings.Contains("|AL|AK|AZ|AR|CA|CO|CT|DE|DC|FL|GA|HI|ID|IL|IN|IA|KS|KY|LA|ME|MD|MA|MI|MN|MS|MO|MT|NE|NV|NH|NJ|NM|NY|NC|ND|OH|OK|OR|PA|RI|SC|SD|TN|TX|UT|VT|VA|WA|WV|WI|WY|", "|"+str(v, "state")+"|") {
		return false
	}
	for _, k := range []string{"first_name", "last_name", "city", "line1"} {
		s := str(v, k)
		if strings.TrimSpace(s) == "" || len(s) > 200 {
			return false
		}
	}
	return true
}

// Select only records explicitly enabled for live use. Existing email fields
// are legacy metadata and never bind the pool to a subscription account.
func chooseBillingProfile(cfg M, pick func(int) (int, error)) (M, error) {
	available := []M{}
	templates, _ := cfg["address_templates"].([]any)
	for _, item := range templates {
		row, ok := item.(map[string]any)
		if !ok || row["enabled"] != true || row["test_only"] != false || billingProfileKey(row) == "" {
			continue
		}
		v := M{"country": "US", "source": "saved", "profile_id": billingProfileKey(row)}
		for _, k := range []string{"first_name", "last_name", "state", "city", "line1", "postal_code"} {
			v[k] = strings.TrimSpace(str(row, k))
		}
		if validBilling(v) {
			available = append(available, v)
		}
	}
	if len(available) == 0 {
		return nil, bad("暂无已启用且完整的正式账单资料，请联系运营配置后重试")
	}
	index, err := pick(len(available))
	if err != nil || index < 0 || index >= len(available) {
		return nil, bad("暂时无法选择账单资料，请稍后重试")
	}
	return available[index], nil
}

func randomBillingIndex(count int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

func (a *App) randomBillingTx(ctx context.Context, tx pgx.Tx) (M, error) {
	var cfg M
	// The selected record cannot be disabled/changed before admission commits.
	if err := tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='billing' FOR SHARE").Scan(&cfg); err != nil {
		return nil, err
	}
	return chooseBillingProfile(cfg, randomBillingIndex)
}

func (a *App) saveOrderBillingTx(ctx context.Context, tx pgx.Tx, id string, billing M) error {
	raw, err := json.Marshal(billing)
	if err != nil {
		return err
	}
	sealed, err := a.encrypt(string(raw))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO order_billing(order_id,details_enc) VALUES($1,$2)", id, sealed)
	return err
}

// Caller holds the order lock. Retrying a known, unsubmitted payment keeps the
// original encrypted snapshot even if the administrator later edits the pool.
func (a *App) orderBillingTx(ctx context.Context, tx pgx.Tx, id string) (M, error) {
	var sealed string
	err := tx.QueryRow(ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", id).Scan(&sealed)
	if err == nil {
		raw, e := a.decrypt(sealed)
		var billing M
		if e != nil || json.Unmarshal([]byte(raw), &billing) != nil || !validBilling(billing) {
			return nil, bad("订单账单资料无法读取，请联系运营核实")
		}
		return billing, nil
	}
	if err != pgx.ErrNoRows {
		return nil, err
	}
	billing, err := a.randomBillingTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = a.saveOrderBillingTx(ctx, tx, id, billing); err != nil {
		return nil, err
	}
	return billing, nil
}

func (a *App) billingProfiles(w http.ResponseWriter, r *http.Request) {
	cfg := a.config(r.Context(), "billing")
	items, ok := cfg["address_templates"].([]any)
	if !ok {
		items = []any{}
	}
	out(w, 200, M{"items": items})
}

func billingProfileKey(row M) string {
	if id := str(row, "id"); id != "" {
		return id
	}
	return strings.ToLower(strings.TrimSpace(str(row, "email")))
}

// This preview never starts checkout or payment and never serves customer APIs.
func (a *App) sampleBillingProfile(w http.ResponseWriter, r *http.Request) {
	available := []M{}
	items, _ := a.config(r.Context(), "billing")["address_templates"].([]any)
	for _, item := range items {
		if row, ok := item.(map[string]any); ok && row["test_only"] == true && row["enabled"] == true {
			available = append(available, row)
		}
	}
	if len(available) == 0 {
		fail(w, 400, "暂无已启用的测试资料")
		return
	}
	n, e := rand.Int(rand.Reader, big.NewInt(int64(len(available))))
	if e != nil {
		fail(w, 503, "暂时无法抽取测试资料")
		return
	}
	out(w, 200, M{"profile": available[n.Int64()], "test_only": true})
}

// Lock and update only the requested record. Concurrent changes to other
// profiles must survive, and no address details are copied into audit events.
func (a *App) changeBillingProfile(w http.ResponseWriter, r *http.Request) {
	profile := M{}
	if r.Method != http.MethodDelete {
		if !read(w, r, &profile) {
			return
		}
		for _, key := range []string{"name", "email", "first_name", "last_name", "state", "city", "line1", "postal_code"} {
			if value, ok := profile[key].(string); ok {
				profile[key] = strings.TrimSpace(value)
			}
		}
		if _, ok := profile["email"]; ok {
			profile["email"] = strings.ToLower(str(profile, "email"))
		}
		if r.Method == http.MethodPost {
			profile["id"] = "bp_" + uid()
		}
		// New admin records explicitly declare their availability and purpose.
		for _, key := range []string{"enabled", "test_only"} {
			if _, ok := profile[key].(bool); !ok {
				fail(w, 400, "请指定账单资料的启用状态及是否仅供测试")
				return
			}
		}
	}
	target := strings.TrimSpace(r.PathValue("email"))
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var raw []byte
		if e := tx.QueryRow(r.Context(), "SELECT value FROM settings WHERE key='billing' FOR UPDATE").Scan(&raw); e != nil {
			return nil, e
		}
		cfg := M{}
		if e := json.Unmarshal(raw, &cfg); e != nil {
			return nil, e
		}
		items, ok := cfg["address_templates"].([]any)
		if !ok {
			return nil, bad("账单资料配置格式无效")
		}
		index := -1
		for i, item := range items {
			if row, ok := item.(map[string]any); ok && (billingProfileKey(row) == target || (str(row, "email") != "" && strings.EqualFold(strings.TrimSpace(str(row, "email")), target))) {
				index = i
				break
			}
		}
		action := "billing.created"
		switch r.Method {
		case http.MethodPost:
			items = append(items, profile)
		case http.MethodPut, http.MethodDelete:
			if index < 0 {
				return nil, businessError{404, "账单资料不存在，请刷新列表"}
			}
			if r.Method == http.MethodDelete {
				items = append(items[:index], items[index+1:]...)
				action = "billing.deleted"
			} else {
				previous := items[index].(map[string]any)
				if id := str(previous, "id"); id != "" {
					profile["id"] = id
				} else {
					profile["id"] = "bp_" + uid()
				}
				items[index] = profile
				action = "billing.updated"
			}
		}
		cfg["address_templates"] = items
		if e := validateConfig("billing", cfg); e != nil {
			return nil, e
		}
		updated, e := json.Marshal(cfg)
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec(r.Context(), "UPDATE settings SET value=$1 WHERE key='billing'", updated); e != nil {
			return nil, e
		}
		if e = a.audit(r.Context(), tx, r, action, "billing", "更新账单资料列表"); e != nil {
			return nil, e
		}
		return M{"items": items}, nil
	})
}
