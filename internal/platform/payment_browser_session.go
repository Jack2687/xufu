package platform

import (
	"context"
	"encoding/json"
	"net/http"
)

func browserPaymentURL(raw string) bool { return hostedPaymentURL(raw) || validCustomCheckoutURL(raw) }

// Only real first-party login cookies enter the private ephemeral browser.
// An access token alone cannot establish the official checkout webpage login.
func browserSessionCookies(in checkoutInput) string {
	if in.Cookies != "" {
		return joinSessionCookies((&http.Request{Header: http.Header{"Cookie": {in.Cookies}}}).Cookies())
	}
	if in.SessionToken != "" {
		return "__Secure-next-auth.session-token=" + in.SessionToken
	}
	return ""
}

func (a *App) loadBrowserSession(ctx context.Context, id string) (string, error) {
	var enc string
	if err := a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1 AND credentials_expires_at>now()", id).Scan(&enc); err != nil {
		return "", err
	}
	raw, err := a.decrypt(enc)
	var in checkoutInput
	if err != nil || json.Unmarshal([]byte(raw), &in) != nil {
		return "", checkoutFailure("invalid_session", false, false)
	}
	cookies := browserSessionCookies(in)
	if cookies == "" {
		return "", checkoutFailure("invalid_session", false, false)
	}
	return cookies, nil
}
