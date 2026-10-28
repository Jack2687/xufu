// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	time "time"
)

// SubscriptionStatus 仅包含订阅查询结果，不携带凭据或上游原始响应。
type model_SubscriptionStatus struct {
	PlanType              string    `json:"planType"`
	SubscriptionPlan      string    `json:"subscriptionPlan"`
	HasActiveSubscription *bool     `json:"hasActiveSubscription"`
	ActiveUntil           string    `json:"activeUntil"`
	Renewal               string    `json:"renewal"`
	FetchedAt             time.Time `json:"fetchedAt"`
}
