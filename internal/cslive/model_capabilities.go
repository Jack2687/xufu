// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	sort "sort"
)

// CapabilityCatalog 是 SDK 支持能力的独立快照，不包含网络发现或运营价格。
type model_CapabilityCatalog struct {
	Plans   []model_PlanCapability   `json:"plans"`
	Regions []model_RegionCapability `json:"regions"`
}

type model_PlanCapability struct {
	Label        string                `json:"label"`
	ID           string                `json:"id"`
	CheckoutPlan string                `json:"checkout_plan"`
	HostedType   string                `json:"hosted_type"`
	Intervals    []string              `json:"intervals"`
	Seats        *model_SeatCapability `json:"seats,omitempty"`
}

type model_SeatCapability struct {
	Minimum int      `json:"minimum"`
	Default int      `json:"default"`
	Types   []string `json:"types"`
}

type model_RegionCapability struct {
	Country  string `json:"country"`
	Currency string `json:"currency"`
	Timezone string `json:"timezone"`
}

// Capabilities 每次返回独立切片，调用方的展示变更不会修改协议定义。
func model_Capabilities() model_CapabilityCatalog {
	catalog := model_CapabilityCatalog{Plans: make([]model_PlanCapability, 0, len(model_planDefinitions)), Regions: make([]model_RegionCapability, 0, len(model_regionDefinitions))}
	for _, p := range model_planDefinitions {
		plan := model_PlanCapability{ID: p.id, Label: p.label, CheckoutPlan: p.checkout, HostedType: p.hosted, Intervals: []string{"month"}}
		if p.team {
			plan.Intervals = []string{"month", "year"}
			plan.Seats = &model_SeatCapability{Minimum: model_MinTeamSeats, Default: model_DefaultTeamSeats, Types: []string{"default", "prolite"}}
		}
		catalog.Plans = append(catalog.Plans, plan)
	}
	for country, region := range model_regionDefinitions {
		catalog.Regions = append(catalog.Regions, model_RegionCapability{country, region.Currency, region.Timezone})
	}
	sort.Slice(catalog.Regions, func(i, j int) bool { return catalog.Regions[i].Country < catalog.Regions[j].Country })
	return catalog
}
