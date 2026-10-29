package platform

const (
	subscriptionFlowLegacy = "legacy"
	subscriptionFlowCSLive = "cslive"
)

// Existing installations keep the currently deployed flow until an admin saves
// another choice. The setting is read only when admitting a new order; workers
// and recovery continue to use that order's persisted protocol membership.
func configuredSubscriptionFlow(cfg M) string {
	if _, present := cfg["subscription_flow"]; !present {
		return subscriptionFlowCSLive
	}
	return str(cfg, "subscription_flow")
}

func validSubscriptionFlow(flow string) bool {
	return flow == subscriptionFlowLegacy || flow == subscriptionFlowCSLive
}
