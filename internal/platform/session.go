package platform

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type SessionInfo struct {
	Email          string    `json:"email"`
	AccountID      string    `json:"account_id"`
	Plan           string    `json:"plan"`
	ExpiresAt      time.Time `json:"expires_at"`
	Status         string    `json:"status"`
	OnlineVerified bool      `json:"online_verified"`
}

// This inspects supplied metadata and expiry only. It neither authenticates a
// token signature nor calls an upstream service. Raw session data is never saved.
func inspectSession(raw json.RawMessage) (SessionInfo, error) {
	var info SessionInfo
	if len(raw) == 0 || len(raw) > 262144 {
		return info, bad("请粘贴完整 Session JSON（最多 256 KB）")
	}
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		raw = []byte(quoted)
	}
	var s struct {
		AccessToken string          `json:"accessToken"`
		Expires     string          `json:"expires"`
		Error       json.RawMessage `json:"error"`
		User        struct {
			Email string `json:"email"`
		} `json:"user"`
		Account struct {
			ID   string `json:"id"`
			Plan string `json:"planType"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return info, bad("Session JSON 格式不正确")
	}
	info.Email = strings.ToLower(strings.TrimSpace(s.User.Email))
	info.AccountID = s.Account.ID
	info.Plan = s.Account.Plan
	if !validEmail(info.Email) || len(info.AccountID) == 0 || len(info.AccountID) > 200 || len(info.Plan) > 80 || len(s.AccessToken) < 20 || len(s.AccessToken) > 32768 {
		return info, bad("Session 缺少有效的账号邮箱、空间标识或 accessToken")
	}
	parts := strings.Split(s.AccessToken, ".")
	if len(parts) != 3 {
		return info, bad("Session 的 accessToken 格式不正确")
	}
	payload, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return info, bad("Session 的 accessToken 格式不正确")
	}
	claims := M{}
	if json.Unmarshal(payload, &claims) != nil {
		return info, bad("Session 的 accessToken 格式不正确")
	}
	exp, ok := claims["exp"].(float64)
	if !ok || exp <= 0 || exp > 1e11 || exp != float64(int64(exp)) {
		return info, bad("Session 缺少有效的 Token 到期时间")
	}
	expiry := time.Unix(int64(exp), 0).UTC()
	if s.Expires != "" {
		date, err := time.Parse(time.RFC3339Nano, s.Expires)
		if err != nil {
			return info, bad("Session 到期时间格式不正确")
		}
		if date.Before(expiry) {
			expiry = date
		}
	}
	if !expiry.After(time.Now()) {
		return info, bad("Session 已过期，请重新获取")
	}
	if v, ok := claims["nbf"].(float64); ok && v > float64(time.Now().Unix()+60) {
		return info, bad("Session 尚未生效，请检查获取时间")
	}
	if len(s.Error) > 0 && string(s.Error) != "null" && string(s.Error) != `""` {
		return info, bad("Session 包含登录错误，请重新获取")
	}
	if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
		if email := str(profile, "email"); email != "" && !strings.EqualFold(email, info.Email) {
			return info, bad("Session 账号信息不一致，请重新获取")
		}
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id := str(auth, "chatgpt_account_id"); id != "" && id != info.AccountID {
			return info, bad("Session 空间信息不一致，请重新获取")
		}
	}
	info.ExpiresAt = expiry
	info.Status = "format_valid"
	return info, nil
}
func (a *App) checkSession(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Session json.RawMessage `json:"session"`
	}
	if !read(w, r, &v) {
		return
	}
	if !a.limit(r.Context(), "session-inspect:"+current(r).ID, 20) {
		fail(w, 429, "检查过于频繁，请稍后再试")
		return
	}
	info, e := inspectSession(v.Session)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	out(w, 200, info)
}
