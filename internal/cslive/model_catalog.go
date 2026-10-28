// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	strings "strings"
)

type model_planDefinition struct {
	id, label, checkout, hosted string
	team                        bool
	subscriptionTypes           map[string]bool
}

// 套餐能力与结算映射共享一份定义，避免调用方与执行路径各自维护名单。
var model_planDefinitions = []model_planDefinition{
	{"go", "Go", "chatgptgoplan", "go", false, protocol_StringSet("go", "go_plan", "chatgptgo", "chatgptgoplan")},
	{"plus", "Plus", "chatgptplusplan", "basic", false, protocol_StringSet("basic", "plus", "basic_plan", "chatgptplusplan")},
	{"pro5x", "Pro 5X", "chatgptprolite", "standard", false, protocol_StringSet("standard", "prolite", "standard_plan", "pro5", "pro5x", "chatgptprolite")},
	{"pro20x", "Pro 20X", "chatgptpro", "premium", false, protocol_StringSet("premium", "pro", "premium_plan", "pro20", "pro20x", "chatgptpro")},
	{"promax", "Pro Max", "chatgptpromax", "promax", false, protocol_StringSet("promax", "promax_plan", "pro_max", "pro_max_plan", "chatgptpromax")},
	{"team", "Team", "chatgptteamplan", "team", true, protocol_StringSet("team", "team_plan", "chatgptteam", "chatgptteamplan", "default")},
	{"team5x", "Team 5X", "chatgptteamplan", "team", true, protocol_StringSet("team5x", "team_5x", "prolite", "chatgptprolite", "chatgptteamplan", "team")},
}

func model_findPlan(id string) model_planDefinition {
	for _, plan := range model_planDefinitions {
		if plan.id == id {
			return plan
		}
	}
	return model_planDefinition{}
}

type model_Region struct{ Currency, Timezone string }

var model_regionDefinitions = map[string]model_Region{"CL": {"CLP", "America/Santiago"}, "PH": {"PHP", "Asia/Manila"}, "US": {"USD", "America/New_York"}, "JP": {"JPY", "Asia/Tokyo"}, "EG": {"EGP", "Africa/Cairo"}, "NR": {"AUD", "Pacific/Nauru"}}

// CheckoutPlan 返回套餐对应的创建请求值，未知套餐返回空字符串。
func model_CheckoutPlan(id string) string {
	plan := model_findPlan(id)
	return plan.checkout
}

// HostedPlanType 返回套餐对应的 Hosted 验证类型，未知套餐返回空字符串。
func model_HostedPlanType(id string) string {
	plan := model_findPlan(id)
	return plan.hosted
}

// LookupRegion 返回地区值的副本，外部修改不会改变协议定义。
func model_LookupRegion(country string) (model_Region, bool) {
	region, ok := model_regionDefinitions[country]
	return region, ok
}

// MatchesSubscriptionType 只匹配上游响应值，不把响应别名用作 SDK 套餐 ID。
func model_MatchesSubscriptionType(plan, value string) bool {
	definition := model_findPlan(plan)
	return definition.subscriptionTypes[value]
}

// IsSubscriptionType 判断上游订阅类型是否属于已知目录，供脱敏事件校验。
func model_IsSubscriptionType(value string) bool {
	for _, plan := range model_planDefinitions {
		if plan.subscriptionTypes[value] {
			return true
		}
	}
	return false
}

const model_DefaultTeamSeats = 2
const model_MinTeamSeats = 2
const model_DefaultTeamInterval = "month"

func model_IsTeamPlan(plan string) bool {
	return model_findPlan(strings.ToLower(plan)).team
}

type model_TeamSeatConfig struct {
	Default, ProLite int
	Interval         string
}

func (c model_TeamSeatConfig) Total() int { return c.Default + c.ProLite }

func model_NormalizeTeamSeatConfig(plan string, seats, seats5x int, interval string) (model_TeamSeatConfig, error) {
	plan = strings.ToLower(plan)
	interval = strings.ToLower(strings.TrimSpace(interval))
	if interval == "" {
		interval = model_DefaultTeamInterval
	}
	if interval != "month" && interval != "year" {
		return model_TeamSeatConfig{}, fmt.Errorf("invalid team price interval")
	}
	if !model_IsTeamPlan(plan) {
		if seats != 0 || seats5x != 0 {
			return model_TeamSeatConfig{}, fmt.Errorf("seats only apply to team plans")
		}
		return model_TeamSeatConfig{}, nil
	}
	defaultSeats, proLite := seats, seats5x
	if plan == "team5x" && seats5x == 0 && seats != 0 {
		defaultSeats, proLite = 0, seats
	}
	if defaultSeats == 0 && proLite == 0 {
		if plan == "team5x" {
			proLite = model_DefaultTeamSeats
		} else {
			defaultSeats = model_DefaultTeamSeats
		}
	}
	if defaultSeats < 0 || proLite < 0 || defaultSeats+proLite < model_MinTeamSeats {
		return model_TeamSeatConfig{}, fmt.Errorf("team seats must be at least %d", model_MinTeamSeats)
	}
	return model_TeamSeatConfig{Default: defaultSeats, ProLite: proLite, Interval: interval}, nil
}

func (c model_TeamSeatConfig) Quantities() []any {
	items := []any{}
	if c.Default > 0 {
		items = append(items, protocol_O("seat_type", "default", "quantity", c.Default))
	}
	if c.ProLite > 0 {
		items = append(items, protocol_O("seat_type", "prolite", "quantity", c.ProLite))
	}
	return items
}

// TeamSeatConfig 读取已经校验的快照字段，不补席位或周期默认值。
func (c model_Checkpoint) TeamSeatConfig() model_TeamSeatConfig {
	defaultSeats, _ := protocol_Integer(c.Metadata["team_default_seats"])
	proLiteSeats, _ := protocol_Integer(c.Metadata["team_prolite_seats"])
	return model_TeamSeatConfig{Default: int(defaultSeats), ProLite: int(proLiteSeats), Interval: protocol_AsString(c.Metadata["team_price_interval"])}
}
