package platform

const (
	subscriptionFlowLegacy         = "legacy"
	subscriptionFlowCSLive         = "cslive"
	subscriptionFlowLegacyProtocol = "legacy_protocol"
	paymentExecutorBrowser         = "browser"
	paymentExecutorLegacyHTTP      = "legacy_http"
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

// One selection owns the entire flow, including the standalone checkout tool.
// Persisted historical order fields remain independent for read-only recovery.
func configuredCheckoutFlow(cfg M) string {
	if configuredSubscriptionFlow(cfg) == subscriptionFlowLegacyProtocol {
		return subscriptionFlowLegacy
	}
	return configuredSubscriptionFlow(cfg)
}

func validSubscriptionFlow(flow string) bool {
	return flow == subscriptionFlowLegacy || flow == subscriptionFlowCSLive || flow == subscriptionFlowLegacyProtocol
}

func subscriptionPaymentExecutor(flow string) string {
	if flow == subscriptionFlowLegacyProtocol {
		return paymentExecutorLegacyHTTP
	}
	return paymentExecutorBrowser
}

func configuredCheckoutType(cfg M) string {
	if _, present := cfg["checkout_type"]; !present {
		return "hosted"
	}
	return str(cfg, "checkout_type")
}

func validCheckoutType(kind string) bool { return kind == "hosted" || kind == "oaics" }

var checkoutProducts = []struct{ ID, Name, Default string }{
	{"pro5x", "Pro 5x", "hosted"},
	{"pro", "Pro 20x", "hosted"},
	{"team", "Team 标准", "oaics"},
	{"team_premium", "Team 高级", "oaics"},
	{"plus", "Plus", "oaics"},
	{"go", "Go", "oaics"},
}

func defaultCheckoutTypes() M {
	result := M{}
	for _, product := range checkoutProducts {
		result[product.ID] = product.Default
	}
	return result
}

func configuredCheckoutTypes(cfg M) M {
	if value, present := cfg["checkout_types"]; present {
		result, _ := value.(M)
		return result
	}
	return defaultCheckoutTypes()
}

func configuredProductCheckoutType(cfg M, product string) string {
	return str(configuredCheckoutTypes(cfg), product)
}

// Auto uses the SDK's default entry point, then its actual upstream mode.
// Keep explicit types on historical orders so recovery never changes their flow.
func protocolCheckoutType(kind string) string {
	if kind == "auto" {
		return ""
	}
	return kind
}
