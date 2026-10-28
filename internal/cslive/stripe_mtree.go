// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	sha256 "crypto/sha256"
	base64 "encoding/base64"
	hex "encoding/hex"
	dep_math_rand_v2 "math/rand/v2"
	strconv "strconv"
	strings "strings"
	time "time"
)

// mtreeURLSalt 取自官方脚本 https://m.stripe.network/out-4.5.45.js 的 URL_SALT 常量，
// 用于对标题做 sha256WithSalt。该值随脚本版本变化，更新脚本版本时需同步核对。
const stripe_mtreeURLSalt = "7766e861-8279-424d-87a1-07a6022fd8cd"

// mtreeOrigin 是 MTree 客户端的部署来源。MTree 脚本本身托管在 m.stripe.network，
// 因此它发出的 m/6 请求 Origin/Referer 都是该源，而不是 js.stripe.com；
// 服务端用 access-control-allow-origin: https://m.stripe.network 校验。
const stripe_mtreeOrigin = "https://m.stripe.network"

// mtreeTagTemplate 是 m/6 载荷里 tag 字段的字面量取值。
// 官方脚本未替换该构建模板变量，真实抓包中四个请求都发这个字符串。
const stripe_mtreeTagTemplate = "$npm_package_version"

// MTreeTagTemplate 暴露 m/6 载荷 tag 字段的字面量取值，供测试对照。
func stripe_MTreeTagTemplate() string { return stripe_mtreeTagTemplate }

// MTreeOrigin 暴露 MTree 客户端来源，供测试对照请求头。
func stripe_MTreeOrigin() string { return stripe_mtreeOrigin }

// mtreeTimings 记录各特征的计算耗时，按官方字段逐一测量，不使用固定常数。
type stripe_mtreeTimings struct {
	start time.Time
	last  time.Time
	now   func() time.Time
}

func stripe_newMtreeTimings(now func() time.Time) *stripe_mtreeTimings {
	if now == nil {
		now = time.Now
	}
	start := now()
	return &stripe_mtreeTimings{start: start, last: start, now: now}
}

func (m *stripe_mtreeTimings) lap() float64 {
	current := m.now()
	elapsed := float64(current.Sub(m.last).Microseconds()) / 1000
	m.last = current
	return stripe_normalizeMilliseconds(elapsed)
}

func (m *stripe_mtreeTimings) total() float64 {
	return stripe_normalizeMilliseconds(float64(m.now().Sub(m.start).Microseconds()) / 1000)
}

// normalizeMilliseconds 输出毫秒耗时，精度对齐官方实测取值（0、0.1、4.6、4.9）。
//
// 官方是 `+(now()-start).toPrecision(5)`。不能直接照搬：toPrecision 对亚毫秒值会输出
// 科学计数法（如 1.2345e-05），转成十进制会退化成 0，使真实耗时全部显示为 0。
// 这里按量级选择定点小数位，保证微秒级耗时仍可见，同时贴近 5 位有效数字：
//
//	>= 100ms  -> 1 位小数   (123.5)
//	>= 10ms   -> 2 位小数   (12.35)
//	>= 1ms    -> 3 位小数   (1.235)
//	<  1ms    -> 4 位小数   (0.0770)
func stripe_normalizeMilliseconds(milliseconds float64) float64 {
	if milliseconds < 0 {
		return 0
	}
	places := 4
	switch {
	case milliseconds >= 100:
		places = 1
	case milliseconds >= 10:
		places = 2
	case milliseconds >= 1:
		places = 3
	}
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(milliseconds, 'f', places, 64), 64)
	if err != nil {
		return 0
	}
	return rounded
}

// MTreeFeatureKeys 按官方脚本的短键顺序列出已实现的特征键。
// 供对照测试构造期望载荷，避免测试与实现各写一份键表。
func stripe_MTreeFeatureKeys() []string {
	return []string{"b", "c", "d", "e", "f", "g", "h", "i", "l"}
}

// MTreeFeatureValues 返回每个已实现特征键对应的期望值。
// 值全部取自传入画像的既有字段，与 deviceFeature 使用同一份定义。
func stripe_MTreeFeatureValues(profile protocol_BrowserProfile) map[string]string {
	return map[string]string{
		"b": "true",
		"c": strings.Join(stripe_profileLanguages(), ","),
		"d": profile.NavigatorPlatform,
		"e": stripe_stripePluginsWithMime,
		"f": strconv.Itoa(profile.AvailWidth) + "w_" + strconv.Itoa(profile.AvailHeight) + "h_" +
			strconv.Itoa(profile.ColorDepth) + "d_" + stripe_formatPixelRatio(profile.DevicePixelRatio) + "r",
		"g": strconv.Itoa(profile.HardwareConcurrency),
		"h": "false",
		"i": "sessionStorage-unavailable, localStorage-unavailable",
		"l": profile.UserAgent,
	}
}

// deviceFeature 组装顶层 a 特征表。只填写本笔画像确实持有的设备事实与可验证的固定语义，
// 不虚构主机硬件，也不猜测官方脚本中语义未确认的字段。
func (s *stripe_Stripe) deviceFeature(timings *stripe_mtreeTimings) protocol_Fields {
	values := stripe_MTreeFeatureValues(s.Profile)
	// l 为异步项，官方对异步特征额外上报 at 活跃耗时。
	async := map[string]bool{"l": true}
	var fields protocol_Fields
	for _, key := range stripe_MTreeFeatureKeys() {
		elapsed := timings.lap()
		feature := protocol_O("v", values[key], "t", elapsed)
		if async[key] {
			feature.Set("at", elapsed)
		}
		fields.Set(key, feature)
	}
	return fields
}

// profileLanguages 是画像语言列表；当前画像固定 en-US。
func stripe_profileLanguages() []string { return []string{"en-US", "en"} }

// formatPixelRatio 让 1.0 输出为 1，与浏览器把 devicePixelRatio 拼接进字符串的结果一致。
func stripe_formatPixelRatio(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// 说明：真实抓包中 m/6 载荷有"两种形态"，这是第一种（设备注册）：
// 顶层 `a` 承载特征对象（首次注册为 null），顶层 `b` 承载上下文对象。
// 第二种是行为遥测 `{muid,sid,url,source,data}`（如 mouse-timings），
// 纯协议客户端没有真实交互事件，不伪造该数据。
//
// deviceContext 组装顶层 b 上下文对象，字段取值与真实抓包一致。
// 首次注册时 muid/sid 还没有值，上下文的 d/e 留空；服务端会在响应里回填它们。
func (s *stripe_Stripe) deviceContext() protocol_Fields {
	context := protocol_O("a", "")
	// b.b 是当前页面 URL 的 hashed 值。协议客户端没有页面 URL 上下文，留空不伪造。
	context.Set("b", "")
	// b.c 在真实抓包中是页面脚本注入的固定串（形如 Slack webhook token），来源不明，不伪造。
	// b.d/b.e 是已建立的设备标识。真实抓包里首次注册也带这两个字段（取值来自已有的
	// __stripe_mid/__stripe_sid cookie）；协议客户端首次注册时没有值，按空串发送而不是省略。
	context.Set("d", s.CP.StripeIdentity["muid"])
	context.Set("e", s.CP.StripeIdentity["sid"])
	context.Set("f", false)
	context.Set("g", true)
	context.Set("h", true)
	context.Set("i", []string{"location"})
	context.Set("j", []string{})
	// b.n 在真实抓包中是重复出现的同一数值（本次会话 256.8），疑似页面级耗时聚合值，
	// 语义未确认，按 0 发送而不编造。
	context.Set("n", 0)
	// b.u 是当前页面的域名；纯协议流程没有页面，留空。
	context.Set("u", "")
	// b.w 是 `时间戳:sha256(随机值+时间戳+1)`，由页面脚本注入，协议客户端没有该值。
	return context
}

// stripePluginsWithMime 对应浏览器 plugins 数组暴露的名称与 mime 类型序列。
// 取值来自 Chromium 内置 PDF 查看器的固定组合，与本文档画像的插件名列表一致。
var stripe_stripePluginsWithMime = strings.Join([]string{
	"PDF Viewer,internal-pdf-viewer,application/pdf,pdf++text/pdf,pdf",
	"Chrome PDF Viewer,internal-pdf-viewer,application/pdf,pdf++text/pdf,pdf",
	"Chromium PDF Viewer,internal-pdf-viewer,application/pdf,pdf++text/pdf,pdf",
	"Microsoft Edge PDF Viewer,internal-pdf-viewer,application/pdf,pdf++text/pdf,pdf",
	"WebKit built-in PDF,internal-pdf-viewer,application/pdf,pdf++text/pdf,pdf",
}, ", ")

// randomNonce 对应官方 getRandomValue，生成固定长度的随机标识。
func stripe_randomNonce(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < length; i++ {
		b.WriteByte(alphabet[dep_math_rand_v2.IntN(len(alphabet))])
	}
	return b.String()
}

// sha256WithSalt 对应官方 sha256WithSalt：sha256(值 + URL_SALT)，输出小写 hex。
func stripe_sha256WithSalt(value string) string {
	sum := sha256.Sum256([]byte(value + stripe_mtreeURLSalt))
	return hex.EncodeToString(sum[:])
}

// mtreeEncode 对应官方的 safeBtoa(encodeURIComponent(JSON.stringify(payload)))，即
// JSON → encodeURIComponent → base64。浏览器请求体在 DevTools 的“请求载荷”里显示为 base64 文本。
func stripe_mtreeEncode(payload any) (string, error) {
	plain, err := protocol_CompactJSON(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(stripe_encodeURIComponent(plain))), nil
}

// encodeURIComponent 实现 ECMAScript 的 encodeURIComponent 语义：
// 不转义 A-Za-z0-9 与 - _ . ! ~ * ' ( )，其余字节按 UTF-8 逐字节写成 %XX。
//
// 不能用 url.QueryEscape 代替：那是表单/查询串编码，会把空格写成 "+"，
// 而官方用 decodeURIComponent 反解，"+" 不会还原成空格，特征值会被解成
// "PDF+Viewer,..." 这种与浏览器不一致的内容。实测浏览器载荷里空格是 %20、加号是 %2B。
func stripe_encodeURIComponent(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		const hexDigits = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// mtreeHeaders 是官方客户端发送 m/6 时使用的请求头集合，取值来自真实浏览器抓包。
//
// 关键点：MTree 脚本本身托管在 m.stripe.network，因此它发出的请求
// Origin/Referer 是 https://m.stripe.network 而不是 js.stripe.com。
// 服务端用 access-control-allow-origin: https://m.stripe.network 校验，写错会跨域失败。
// sec-fetch-site 对应 cross-site（结算页 -> m.stripe.network）。
//
// 正文是标准 base64，含 "+" 字符；必须避免被当成表单编码（"+" 会被解成空格），
// 真实浏览器显式声明 text/plain;charset=UTF-8，且服务端据此放行 Content-Type。
func (s *stripe_Stripe) mtreeHeaders() protocol_Fields {
	h := s.Headers()
	h.Set("Origin", stripe_mtreeOrigin)
	h.Set("Referer", stripe_mtreeOrigin+"/")
	h.Update(protocol_O(
		"Accept", "*/*",
		"Content-Type", "text/plain;charset=UTF-8",
		"sec-fetch-dest", "empty",
		"sec-fetch-mode", "cors",
		"sec-fetch-site", "cross-site",
	))
	return h
}
