package platform

import (
	"context"
	"net/url"
	"strings"
)

type subscriptionSnapshot struct {
	Active, Renew, RenewalKnown bool
	Plan                        string
}

func readSubscription(ctx context.Context, s *providerSession) (subscriptionSnapshot, error) {
	v, err := s.read(ctx, subscriptionEndpoint+"?account_id="+url.QueryEscape(s.in.AccountID))
	if providerCode(err) == "subscription_not_found" {
		return subscriptionSnapshot{}, nil
	}
	if err != nil {
		return subscriptionSnapshot{}, err
	}
	if id := str(v, "account_id"); id != "" && id != s.in.AccountID {
		return subscriptionSnapshot{}, checkoutFailure("account_mismatch", false, false)
	}
	active, ok := v["is_active"].(bool)
	if !ok {
		active, ok = v["has_active_subscription"].(bool)
	}
	if !ok {
		return subscriptionSnapshot{}, checkoutFailure("invalid_response", true, false)
	}
	renew, known := v["will_renew"].(bool)
	plan := str(v, "plan_type")
	if plan == "" {
		plan = str(v, "subscription_plan")
	}
	if plan == "" {
		plan = str(v, "plan")
	}
	return subscriptionSnapshot{Active: active, Renew: renew, RenewalKnown: known, Plan: plan}, nil
}

func planMatches(product, plan string) bool {
	allowed := map[string][]string{
		"pro5x":        {"standard", "standard_plan", "prolite", "chatgptprolite", "pro5x", "pro5"},
		"pro":          {"premium", "premium_plan", "pro", "chatgptpro", "pro20x", "pro20"},
		"team":         {"team", "team_plan", "business", "chatgptteamplan", "chatgptteam"},
		"team_premium": {"team", "team5x", "team_5x", "business", "business_prolite", "chatgptteamplan"},
	}
	for _, v := range allowed[product] {
		if strings.EqualFold(v, plan) {
			return true
		}
	}
	return false
}

func verifySubscriptionPlan(ctx context.Context, s *providerSession, snapshot subscriptionSnapshot, product string) error {
	plan := snapshot.Plan
	if plan == "" {
		v, e := s.read(ctx, accountEndpoint)
		if e != nil {
			return e
		}
		accounts, _ := v["accounts"].(map[string]any)
		for key, value := range accounts {
			entry, _ := value.(map[string]any)
			account, _ := entry["account"].(map[string]any)
			id := str(account, "id")
			if id == "" {
				id = key
			}
			if id == s.in.AccountID {
				ent, _ := entry["entitlement"].(map[string]any)
				plan = str(ent, "subscription_plan")
				if plan == "" {
					plan = str(account, "plan_type")
				}
				break
			}
		}
	}
	if plan == "" {
		return checkoutFailure("account_status_unknown", true, false)
	}
	if !planMatches(product, plan) {
		return checkoutFailure("plan_mismatch", false, false)
	}
	return nil
}

func sessionRenewalStatus(ctx context.Context, s *providerSession) (bool, bool, error) {
	v, e := readSubscription(ctx, s)
	if e != nil {
		return false, false, e
	}
	if v.Active && !v.RenewalKnown {
		return false, false, checkoutFailure("invalid_response", true, false)
	}
	return v.Active, v.Renew, nil
}
