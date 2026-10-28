package platform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fixed read-only probes. Never send account authorization or expose raw provider bodies.
const openAIProbeURL = "https://api.openai.com/v1/models"

type proxyExitSample struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	Latency int64  `json:"latency_ms"`
	OK      bool   `json:"ok"`
}
type proxyOpenAIResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	HTTP    int    `json:"http_status"`
	Latency int64  `json:"latency_ms"`
	Stage   string `json:"stage,omitempty"`
	Reason  string `json:"reason,omitempty"`
}
type proxyQualityResult struct {
	Version   int               `json:"version,omitempty"`
	Kind      string            `json:"kind"`
	Status    string            `json:"status"`
	Message   string            `json:"message"`
	CheckedAt time.Time         `json:"checked_at"`
	Score     *int              `json:"score,omitempty"`
	Grade     string            `json:"grade,omitempty"`
	Parts     M                 `json:"parts,omitempty"`
	Samples   []proxyExitSample `json:"samples,omitempty"`
	Stability string            `json:"stability,omitempty"`
	OpenAI    proxyOpenAIResult `json:"openai"`
}

// Return only fixed diagnostic labels. Network errors may contain proxy
// credentials, resolved addresses or request URLs and must never be projected.
func proxyProbeFailure(err error, stage string) (string, string) {
	if errors.Is(err, context.Canceled) {
		return "cancelled", "检测已中止"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout", map[string]string{"proxy_connect": "连接代理或建立代理隧道超时", "tls": "目标 TLS 握手超时", "response": "等待目标响应超时", "body": "读取目标响应超时"}[stage]
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", "代理地址解析失败"
	}
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	if errors.As(err, &certificate) || errors.As(err, &authority) {
		return "certificate", "TLS 证书校验失败"
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	// CONNECT errors are returned without an HTTP response by net/http.
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "local proxy") {
		return "local_proxy", "本机系统代理连接失败，请检查本地代理是否运行及可用"
	}
	if stage == "proxy_connect" {
		if strings.Contains(message, "proxy authentication required") || message == "407" {
			return "proxy_auth", "代理认证失败（407）"
		}
		if strings.Contains(message, "forbidden") {
			return "proxy_denied", "代理拒绝建立目标隧道"
		}
		if strings.Contains(message, "bad gateway") || strings.Contains(message, "gateway timeout") {
			return "proxy_gateway", "代理网关连接目标失败"
		}
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(message, "connection reset") || strings.Contains(message, "forcibly closed") {
		return "connection_reset", map[string]string{"proxy_connect": "代理隧道建立前连接被重置，未收到目标响应", "tls": "目标 TLS 握手时连接被重置", "response": "等待目标响应时连接被重置", "body": "读取目标响应时连接被重置"}[stage]
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection_closed", map[string]string{"proxy_connect": "代理隧道建立前连接已关闭，未收到目标响应", "tls": "目标 TLS 握手时连接已关闭", "response": "目标未返回完整响应", "body": "目标响应读取中断"}[stage]
	}
	return "network", map[string]string{"proxy_connect": "连接代理或建立代理隧道失败", "tls": "目标 TLS 握手失败", "response": "目标请求失败", "body": "目标响应读取失败"}[stage]
}

func probeProxyExit(ctx context.Context, c *http.Client) proxyExitSample {
	start := time.Now()
	sample := proxyExitSample{}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://ipinfo.io/json", nil)
	// Separate connections detect rotating exits instead of reusing one tunnel.
	req.Close = true
	res, e := c.Do(req)
	sample.Latency = time.Since(start).Milliseconds()
	if e != nil {
		return sample
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 8193))
	if e != nil || len(b) > 8192 || res.StatusCode != 200 {
		return sample
	}
	var v struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
	}
	if json.Unmarshal(b, &v) != nil || net.ParseIP(v.IP) == nil {
		return sample
	}
	sample.IP = net.ParseIP(v.IP).String()
	sample.OK = true
	if len(v.Country) == 2 && v.Country[0] >= 'A' && v.Country[0] <= 'Z' && v.Country[1] >= 'A' && v.Country[1] <= 'Z' {
		sample.Country = v.Country
	}
	return sample
}

func probeOpenAI(ctx context.Context, c *http.Client) proxyOpenAIResult {
	result := proxyOpenAIResult{Status: "fail", Message: "连接失败或请求超时"}
	var phase atomic.Int32
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { phase.Store(1) },
		GotConn:           func(httptrace.GotConnInfo) { phase.Store(2) },
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	req, _ := http.NewRequestWithContext(ctx, "GET", openAIProbeURL, nil)
	req.Header.Set("User-Agent", webCheckoutUA)
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	res, e := c.Do(req)
	result.Latency = time.Since(start).Milliseconds()
	if e != nil {
		result.Stage = []string{"proxy_connect", "tls", "response"}[phase.Load()]
		result.Reason, result.Message = proxyProbeFailure(e, result.Stage)
		return result
	}
	defer res.Body.Close()
	result.HTTP = res.StatusCode
	b, e := io.ReadAll(io.LimitReader(res.Body, 8193))
	if e != nil {
		result.Stage = "body"
		result.Reason, result.Message = proxyProbeFailure(e, result.Stage)
		return result
	}
	body := strings.ToLower(string(b))
	challenge := strings.EqualFold(res.Header.Get("cf-mitigated"), "challenge")
	for _, mark := range []string{"window._cf_chl_opt", "just a moment", "enable javascript and cookies to continue", "__cf_chl_", "challenge-platform"} {
		challenge = challenge || strings.Contains(body, mark)
	}
	if strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/html") && (strings.Contains(body, "<html") || strings.Contains(body, "<!doctype html")) && (strings.Contains(body, "cloudflare") || strings.Contains(body, "challenge")) {
		challenge = true
	}
	if (res.StatusCode == 403 || res.StatusCode == 429) && challenge {
		result.Status, result.Message = "challenge", "遇到 Cloudflare 验证"
		return result
	}
	switch res.StatusCode {
	case 401:
		result.Status, result.Message = "pass", "目标可达（未携带密钥，返回 401）"
	case 429:
		result.Status, result.Message = "warn", "目标返回限流，请稍后再测"
	case 403:
		result.Message = "目标拒绝访问"
	default:
		result.Message = "目标返回非预期状态"
	}
	return result
}

func measureProxy(ctx context.Context, c *http.Client, kind string) proxyQualityResult {
	result := proxyQualityResult{Version: 2, Kind: kind, CheckedAt: time.Now().UTC()}
	if kind == "quality" {
		result.Samples = []proxyExitSample{probeProxyExit(ctx, c), probeProxyExit(ctx, c)}
	}
	result.OpenAI = probeOpenAI(ctx, c)
	result.Status, result.Message = result.OpenAI.Status, result.OpenAI.Message
	if kind != "quality" {
		return result
	}
	connectivity, stable, speed, openai := 0, 0, 0, 0
	latency, total := int64(0), 0
	for _, s := range result.Samples {
		if s.OK {
			connectivity += 15
			total++
			latency += s.Latency
		}
	}
	result.Stability = "unknown"
	if total == 2 {
		// Dynamic proxies intentionally rotate exits. Repeated successful
		// requests measure reliability; IP changes are informational only.
		stable = 20
		if result.Samples[0].IP == result.Samples[1].IP {
			result.Stability = "stable"
		} else {
			result.Stability = "changed"
		}
	}
	if total > 0 {
		switch mean := latency / int64(total); {
		case mean <= 500:
			speed = 20
		case mean <= 1500:
			speed = 15
		case mean <= 3000:
			speed = 10
		default:
			speed = 5
		}
	}
	if result.OpenAI.Status == "pass" {
		openai = 30
	} else if result.OpenAI.Status == "warn" {
		openai = 10
	}
	score := connectivity + stable + speed + openai
	result.Score = &score
	result.Parts = M{"connectivity": connectivity, "stability": stable, "latency": speed, "openai": openai}
	switch {
	case score >= 90:
		result.Grade = "A"
	case score >= 75:
		result.Grade = "B"
	case score >= 60:
		result.Grade = "C"
	case score >= 40:
		result.Grade = "D"
	default:
		result.Grade = "F"
	}
	result.Status = "pass"
	result.Message = "检测完成，评分反映本次连通质量"
	if total == 0 {
		result.Status = "fail"
		result.Message = "出口探测失败，请检查代理连接"
	} else if total < 2 || result.OpenAI.Status != "pass" {
		result.Status = "warn"
		result.Message = "检测完成，存在需关注的项目"
	}
	return result
}

func (a *App) startProxyCheck(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Kind string `json:"kind"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Kind != "quality" && v.Kind != "openai" {
		fail(w, 400, "请选择评分或 OpenAI 测试")
		return
	}
	if !a.limit(r.Context(), "proxytest:"+current(r).ID, 10) {
		fail(w, 429, "测试过于频繁")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
			return nil, e
		}
		var id, kind string
		if e := tx.QueryRow(r.Context(), "SELECT id FROM resources WHERE id=$1", r.PathValue("id")).Scan(&id); e != nil {
			return nil, businessError{404, "资源不存在"}
		}
		e := tx.QueryRow(r.Context(), "SELECT id,kind FROM proxy_checks WHERE resource_id=$1 AND state IN ('queued','running')", id).Scan(&id, &kind)
		if e == nil {
			if kind != v.Kind {
				return nil, businessError{409, "此代理已有测试在进行，请等待结果"}
			}
			return M{"id": id, "replayed": true}, nil
		}
		if e != pgx.ErrNoRows {
			return nil, e
		}
		var pending int
		if e = tx.QueryRow(r.Context(), "SELECT count(*) FROM proxy_checks WHERE state IN ('queued','running')").Scan(&pending); e != nil {
			return nil, e
		}
		if pending >= 100 {
			return nil, businessError{429, "测试队列已满，请稍后再试"}
		}
		id = uid()
		if _, e = tx.Exec(r.Context(), "INSERT INTO proxy_checks(id,resource_id,kind) VALUES($1,$2,$3)", id, r.PathValue("id"), v.Kind); e != nil {
			return nil, e
		}
		return M{"id": id, "state": "queued"}, a.audit(r.Context(), tx, r, "proxy.check", r.PathValue("id"), v.Kind)
	})
}
func (a *App) proxyCheck(w http.ResponseWriter, r *http.Request) {
	rows, e := a.query(r.Context(), "SELECT id,resource_id,kind,state,result,created_at,updated_at FROM proxy_checks WHERE id=$1", r.PathValue("id"))
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	if len(rows) == 0 {
		fail(w, 404, "检测记录不存在")
		return
	}
	out(w, 200, rows[0])
}

type proxyCheckJob struct{ ID, ResourceID, Kind, Lease, ProxyLease, Enc string }

func (a *App) claimProxyCheck(ctx context.Context) (*proxyCheckJob, error) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
		return nil, e
	}
	var active int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM proxy_checks WHERE lease_until>now()").Scan(&active); e != nil {
		return nil, e
	}
	if active >= 2 {
		return nil, nil
	}
	j := &proxyCheckJob{}
	e = tx.QueryRow(ctx, `SELECT c.id,c.resource_id,c.kind,r.endpoint_enc FROM proxy_checks c JOIN resources r ON r.id=c.resource_id
 WHERE c.state IN ('queued','running') AND (c.lease_until IS NULL OR c.lease_until<now())
 AND (SELECT count(*) FROM proxy_leases l WHERE l.resource_id=r.id AND l.expires_at>now())<r.capacity
 ORDER BY c.created_at LIMIT 1 FOR UPDATE OF c SKIP LOCKED`).Scan(&j.ID, &j.ResourceID, &j.Kind, &j.Enc)
	if e == pgx.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM proxy_checks WHERE id=$1)", j.ID); e != nil {
		return nil, e
	}
	j.Lease, j.ProxyLease = secret(), uid()
	if _, e = tx.Exec(ctx, "INSERT INTO proxy_leases(id,resource_id,expires_at) VALUES($1,$2,now()+interval '35 seconds')", j.ProxyLease, j.ResourceID); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE proxy_checks SET state='running',lease_token=$2,lease_until=now()+interval '35 seconds',proxy_lease_id=$3,updated_at=now() WHERE id=$1", j.ID, j.Lease, j.ProxyLease); e != nil {
		return nil, e
	}
	return j, tx.Commit(ctx)
}

func (a *App) executeProxyCheck(ctx context.Context, j *proxyCheckJob) {
	var valid bool
	if e := a.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM proxy_checks WHERE id=$1 AND lease_token=$2 AND lease_until>now())", j.ID, j.Lease).Scan(&valid); e != nil || !valid {
		return
	}
	result := proxyQualityResult{Kind: j.Kind, Status: "fail", Message: "代理配置不可用", CheckedAt: time.Now().UTC(), OpenAI: proxyOpenAIResult{Status: "fail", Message: "未执行目标测试"}}
	endpoint, e := a.decrypt(j.Enc)
	if e == nil {
		factory := newPaymentClient
		if a.proxyProbeClient != nil {
			factory = a.proxyProbeClient
		}
		client, closeClient, err := factory(endpoint, 8*time.Second)
		if err == nil {
			result = measureProxy(ctx, client, j.Kind)
			closeClient()
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, e := a.DB.Begin(save)
	if e != nil {
		return
	}
	defer tx.Rollback(save)
	raw, _ := json.Marshal(result)
	tag, e := tx.Exec(save, "UPDATE proxy_checks SET state='completed',result=$3,lease_token=NULL,lease_until=NULL,proxy_lease_id=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2", j.ID, j.Lease, raw)
	if e != nil || tag.RowsAffected() != 1 {
		return
	}
	field := "quality_result"
	if j.Kind == "openai" {
		field = "openai_result"
	}
	if _, e = tx.Exec(save, "UPDATE resources SET "+field+"=$2 WHERE id=$1 AND endpoint_enc=$3", j.ResourceID, raw, j.Enc); e != nil {
		return
	}
	if j.Kind == "quality" {
		openaidata, _ := json.Marshal(proxyQualityResult{Version: result.Version, Kind: "openai", Status: result.OpenAI.Status, Message: result.OpenAI.Message, CheckedAt: result.CheckedAt, OpenAI: result.OpenAI})
		last := proxyExitSample{}
		for _, sample := range result.Samples {
			if sample.OK {
				last = sample
			}
		}
		status := "failed"
		if last.OK {
			status = "healthy"
		}
		if _, e = tx.Exec(save, "UPDATE resources SET openai_result=$2,last_status=$3,last_checked=$4,last_ip=$5,last_country=$6,last_latency_ms=$7 WHERE id=$1 AND endpoint_enc=$8", j.ResourceID, openaidata, status, result.CheckedAt, last.IP, last.Country, last.Latency, j.Enc); e != nil {
			return
		}
	}
	if _, e = tx.Exec(save, "DELETE FROM proxy_leases WHERE id=$1", j.ProxyLease); e != nil {
		return
	}
	tx.Commit(save)
}
func (a *App) RunProxyChecks(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, e := a.claimProxyCheck(ctx)
			if e == nil && job != nil {
				go func() {
					c, cancel := context.WithTimeout(ctx, 25*time.Second)
					defer cancel()
					a.executeProxyCheck(c, job)
				}()
			}
		}
	}
}
