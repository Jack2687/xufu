package platform

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiDocument []byte

func (a *App) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(openapiDocument)
}
func (a *App) catalog(w http.ResponseWriter, r *http.Request) {
	m, e := membershipFor(r.Context(), a.DB, current(r).ID)
	if e != nil {
		fail(w, 500, "读取报价失败")
		return
	}
	rows, e := a.query(r.Context(), "SELECT id,name,description,fee,tag,features FROM products WHERE enabled ORDER BY sort")
	if e != nil {
		fail(w, 500, "读取服务失败")
		return
	}
	for _, p := range rows {
		p["member_fee"] = feeFor(m, p["fee"].(int64))
	}
	out(w, 200, M{"items": rows, "membership": m, "unit": "cent_R", "countries": []M{{"code": "PH", "name": "菲律宾", "currency": "PHP"}, {"code": "US", "name": "美国", "currency": "USD"}, {"code": "JP", "name": "日本", "currency": "JPY"}, {"code": "SG", "name": "新加坡", "currency": "SGD"}, {"code": "GB", "name": "英国", "currency": "GBP"}, {"code": "EG", "name": "埃及", "currency": "EGP"}, {"code": "TR", "name": "土耳其", "currency": "TRY"}}, "payment_connected": a.paymentEnabled(r.Context())})
}
func (a *App) apiWallet(w http.ResponseWriter, r *http.Request) {
	m, e := membershipFor(r.Context(), a.DB, current(r).ID)
	if e != nil {
		fail(w, 500, "读取余额失败")
		return
	}
	u := current(r)
	out(w, 200, M{"balance": u.Balance, "frozen": u.Frozen, "unit": "cent_R", "membership": m})
}
