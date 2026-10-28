// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	base64 "encoding/base64"
	fmt "fmt"
	math "math"
	big "math/big"
	dep_math_rand_v2 "math/rand/v2"
	strings "strings"
	time "time"
)

const sentinel_FrameVersion = "20260810913b"
const sentinel_SDKStubURL = "https://chatgpt.com/backend-api/sentinel/sdk.js"
const sentinel_SDKURL = "https://chatgpt.com/sentinel/" + sentinel_FrameVersion + "/sdk.js"
const sentinel_maxNonce = 500000

var sentinel_timezoneLabels = map[string][2]string{
	"Asia/Manila":         {"Philippine Standard Time", "Philippine Standard Time"},
	"America/New_York":    {"Eastern Standard Time", "Eastern Daylight Time"},
	"Asia/Singapore":      {"Singapore Standard Time", "Singapore Standard Time"},
	"Asia/Tokyo":          {"Japan Standard Time", "Japan Standard Time"},
	"Asia/Seoul":          {"Korean Standard Time", "Korean Standard Time"},
	"Europe/London":       {"Greenwich Mean Time", "British Summer Time"},
	"Europe/Berlin":       {"Central European Standard Time", "Central European Summer Time"},
	"Europe/Paris":        {"Central European Standard Time", "Central European Summer Time"},
	"Europe/Amsterdam":    {"Central European Standard Time", "Central European Summer Time"},
	"Europe/Dublin":       {"Greenwich Mean Time", "Irish Standard Time"},
	"Asia/Kolkata":        {"India Standard Time", "India Standard Time"},
	"Australia/Sydney":    {"Australian Eastern Standard Time", "Australian Eastern Daylight Time"},
	"America/Toronto":     {"Eastern Standard Time", "Eastern Daylight Time"},
	"Asia/Hong_Kong":      {"Hong Kong Standard Time", "Hong Kong Standard Time"},
	"Asia/Taipei":         {"Taipei Standard Time", "Taipei Standard Time"},
	"Asia/Kuala_Lumpur":   {"Malaysia Time", "Malaysia Time"},
	"Asia/Bangkok":        {"Indochina Time", "Indochina Time"},
	"Asia/Ho_Chi_Minh":    {"Indochina Time", "Indochina Time"},
	"Asia/Jakarta":        {"Western Indonesia Time", "Western Indonesia Time"},
	"America/Sao_Paulo":   {"Brasilia Standard Time", "Brasilia Summer Time"},
	"America/Mexico_City": {"Central Standard Time", "Central Daylight Time"},
	"UTC":                 {"Coordinated Universal Time", "Coordinated Universal Time"},
}

func sentinel_chromeDate(zone string, t time.Time) string {
	label := ""
	if labels, ok := sentinel_timezoneLabels[zone]; ok {
		label = labels[0]
		daylight := t.IsDST()
		if zone == "Europe/Dublin" {
			// IANA 的负 DST 与 macOS 的夏季 DST 标记不同；协议使用 Chrome 的冬夏标签。
			_, offset := t.Zone()
			daylight = offset > 0
		}
		if daylight && labels[1] != "" {
			label = labels[1]
		}
	} else {
		label, _ = t.Zone()
		if len(label) <= 3 {
			parts := strings.Split(zone, "/")
			label = strings.ReplaceAll(parts[len(parts)-1], "_", " ")
			if label == "" {
				label = "Coordinated Universal Time"
			}
		}
	}
	_, offset := t.Zone()
	minutes := int(math.Floor(float64(offset) / 60))
	sign := "+"
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	return fmt.Sprintf("%s GMT%s%02d%02d (%s)", t.Format("Mon Jan 02 2006 15:04:05"), sign, minutes/60, minutes%60, label)
}

// Generator 的随机值和 timeOrigin 在当前挑战中隔离，不缓存跨 flow 令牌。
type sentinel_Generator struct {
	deviceID, locale, timezone string
	profile                    protocol_BrowserProfile
	clock                      fingerprint_Clock
	reactKey                   string
	timeOrigin                 float64
	randomFloat                func() float64
	randomInt                  func(int, int) int
	perSecond                  func() float64
}

func sentinel_NewGenerator(deviceID string, profile protocol_BrowserProfile, locale, timezone string, clock fingerprint_Clock) *sentinel_Generator {
	if locale == "" {
		locale = "en-US"
	}
	if timezone == "" {
		timezone = "UTC"
	}
	alphabet := "abcdefghijklmnopqrstuvwxyz0123456789"
	key := make([]byte, 13)
	for i := range key {
		key[i] = alphabet[dep_math_rand_v2.IntN(len(alphabet))]
	}
	start := time.Now()
	return &sentinel_Generator{deviceID: strings.TrimSpace(deviceID), profile: profile, locale: locale,
		timezone: timezone, clock: clock, reactKey: "__reactContainer$" + string(key),
		timeOrigin:  clock.NowMS() - (1200 + dep_math_rand_v2.Float64()*3600),
		randomFloat: dep_math_rand_v2.Float64, randomInt: func(a, b int) int { return a + dep_math_rand_v2.IntN(b-a+1) },
		perSecond: func() float64 { return time.Since(start).Seconds() }}
}

func (g *sentinel_Generator) config(nonce int, elapsedMS float64) (*sentinel_array, error) {
	local, err := g.clock.LocalTime(g.timezone)
	if err != nil {
		return nil, err
	}
	primary := strings.ReplaceAll(g.locale, "_", "-")
	base := strings.SplitN(primary, "-", 2)[0]
	languages := primary
	if !strings.EqualFold(primary, base) {
		languages += "," + base
	}
	// 只有已明确的屏幕汇总和 UA 来自画像；未确认含义的数值槽位沿用协议基线。
	return sentinel_arr(int64(g.profile.ScreenWidth+g.profile.ScreenHeight), sentinel_chromeDate(g.timezone, local), int64(4294967296), int64(nonce),
		g.profile.UserAgent, sentinel_SDKURL, nil, primary, languages, int64(g.randomInt(2, 8)),
		"scheduling-[object Scheduling]", g.reactKey, "onscrollend", elapsedMS, g.deviceID,
		"", int64(8), sentinel_roundDecimal(g.timeOrigin, 1), int64(0), int64(0), int64(0),
		int64(0), int64(0), int64(0), int64(0)), nil
}

func (g *sentinel_Generator) RequirementsToken() (string, error) {
	config, err := g.config(1, g.randomFloat()*4900+100)
	if err != nil {
		return "", err
	}
	encoded, err := sentinel_encodeSentinelJSON(config)
	if err != nil {
		return "", err
	}
	return "gAAAAAC" + encoded + "~S", nil
}

func (g *sentinel_Generator) PowToken(seed, difficulty string, deadline time.Time) (string, error) {
	start := g.perSecond()
	if difficulty == "" {
		difficulty = "0"
	}
	prefix := min(len(difficulty), 8)
	for nonce := 0; nonce < sentinel_maxNonce; nonce++ {
		if nonce%64 == 0 {
			if err := sentinel_checkDeadline(deadline); err != nil {
				return "", err
			}
		}
		config, err := g.config(nonce, (g.perSecond()-start)*1000)
		if err != nil {
			return "", err
		}
		encoded, err := sentinel_encodeSentinelJSON(config)
		if err != nil {
			return "", err
		}
		digest := sentinel_hashFNV1a(seed + encoded)
		if strings.HasPrefix(digest, difficulty) || digest[:prefix] <= difficulty {
			if err := sentinel_checkDeadline(deadline); err != nil {
				return "", err
			}
			return "gAAAAAB" + encoded + "~S", nil
		}
	}
	return "", fmt.Errorf("pow_exhausted")
}

func sentinel_hashFNV1a(s string) string {
	h := uint32(2166136261)
	for _, b := range []byte(s) {
		h ^= uint32(b)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

func sentinel_encodeSentinelJSON(value any) (string, error) {
	raw, err := sentinel_dumpJSON(value, false, false)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(raw)), nil
}

// 用 float64 的精确有理数做十进制舍入，避免先乘十引入二次舍入。
func sentinel_roundDecimal(value float64, places int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	rat := new(big.Rat).SetFloat64(value)
	rat.Mul(rat, new(big.Rat).SetInt(scale))
	negative := rat.Sign() < 0
	num := new(big.Int).Abs(rat.Num())
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(num, rat.Denom(), remainder)
	cmp := new(big.Int).Lsh(remainder, 1).Cmp(rat.Denom())
	if cmp > 0 || (cmp == 0 && quotient.Bit(0) == 1) {
		quotient.Add(quotient, big.NewInt(1))
	}
	if negative {
		quotient.Neg(quotient)
	}
	result, _ := new(big.Rat).SetFrac(quotient, scale).Float64()
	if result == 0 && math.Signbit(value) {
		return math.Copysign(0, -1)
	}
	return result
}
