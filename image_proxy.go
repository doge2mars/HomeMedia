package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// /api/image-proxy?url=<图片地址> —— 借 image-proxy 的机制，并扩展到所有海报
// 只给豆瓣/Bangumi/manmankan 走这条路，源站海报它也是直连；
// HomeMedia 网页端把全部海报都走这里，电视端源站海报
// 因 APK 的 getImageUrlSync 只改写 doubanio/bgm.tv，改不了——见 README。
// 安全：只放行 http/https、只转发 image/*、挡内网 IP（SSRF），单图 ≤10MB，直连 5s 超时、代理重试各 5s。
func handleImageProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, 400, map[string]string{"error": "bad url"})
		return
	}
	// 缓存命中直接回，跳过 DNS 私有性检查：热点海报每次请求都查一次 DNS 是白白的延迟，
	// 只有缓存未命中、真要出网抓时才需要这道检查。
	if body, ct, ok := imageCacheLookup(raw); ok {
		serveImage(w, body, ct)
		return
	}
	if isPrivateHost(u.Hostname()) {
		writeJSON(w, 403, map[string]string{"error": "private host blocked"})
		return
	}
	body, ct, err := fetchAndCache(raw)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	serveImage(w, body, ct)
}

// imageCacheLookup：只读磁盘缓存，命中返回 (body, ct, true)；未命中返回 ok=false，不写盘。
func imageCacheLookup(raw string) ([]byte, string, bool) {
	dataDir := getenv("HOME_MEDIA_DATA", "/opt/home-media/data")
	cacheDir := filepath.Join(dataDir, "image-cache")
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	b, err := os.ReadFile(filepath.Join(cacheDir, key+".bin"))
	if err != nil {
		return nil, "", false
	}
	ct, err := os.ReadFile(filepath.Join(cacheDir, key+".meta"))
	if err != nil {
		return nil, "", false
	}
	return b, strings.TrimSpace(string(ct)), true
}

func serveImage(w http.ResponseWriter, body []byte, ct string) {
	w.Header().Set("Content-Type", ct)
	// #9 nosniff：防浏览器把错误类型的内容当脚本执行
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=604800, stale-while-revalidate=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(body)
}

// getImageProxies：服务端拉图代理列表。优先读设置里存的（meta image_proxies），
// 为空则读环境变量 HOME_MEDIA_IMAGE_PROXIES（逗号分隔）。直连永远先试，
// 代理只在直连失败/超时后按顺序兜底，不是一上来就全走代理。
func getImageProxies() []string {
	if v, ok := metaGet("image_proxies"); ok {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil {
			out := []string{}
			for _, p := range list {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	env := getenv("HOME_MEDIA_IMAGE_PROXIES", "")
	if env == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(env, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fetchImageOnce(raw string, proxyURL string, timeout time.Duration) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")
	tr := &http.Transport{}
	if proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil {
			return nil, "", err
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("upstream %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/") {
		return nil, "", fmt.Errorf("not an image")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, "", err
	}
	return body, ct, nil
}

// fetchAndCache：命中磁盘缓存直接回，否则先直连、失败再按设置里的代理顺序重试，
// 成功才落盘；供代理和目录同步预取共用
func fetchAndCache(raw string) ([]byte, string, error) {
	_, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if b, ct, ok := imageCacheLookup(raw); ok {
		return b, ct, nil
	}
	dataDir := getenv("HOME_MEDIA_DATA", "/opt/home-media/data")
	cacheDir := filepath.Join(dataDir, "image-cache")
	_ = os.MkdirAll(cacheDir, 0o755)
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	bodyPath := filepath.Join(cacheDir, key+".bin")
	metaPath := filepath.Join(cacheDir, key+".meta")
	// 死图封顶从 25 秒压到 10 秒：直连 5 秒没回就判负走代理，代理各 5 秒；
	// 以前直连 15 秒 + 代理 10 秒，一张慢图能堵住浏览器同 host 的 6 连接队列里的一整排。
	body, ct, err := fetchImageOnce(raw, "", 5*time.Second)
	if err != nil {
		for _, px := range getImageProxies() {
			if body, ct, err = fetchImageOnce(raw, px, 5*time.Second); err == nil {
				break
			}
		}
	}
	if err != nil {
		// 海报拉不到要有迹可查：记 URL + 上游状态/错误（海报地址不含敏感信息）
		log.Printf("image-proxy: fetch failed url=%s err=%v", raw, err)
		return nil, "", err
	}
	_ = os.WriteFile(bodyPath, body, 0o644)
	_ = os.WriteFile(metaPath, []byte(ct), 0o644)
	return body, ct, nil
}

// imageProbeResult：单个图片代理的测试结果。
type imageProbeResult struct {
	Name  string `json:"name"` // 直连 或代理地址
	OK    bool   `json:"ok"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// probeImageProxies：对直连 + 每个已配代理并发真实拉一张图（各 5 秒超时），返回每路结果。
// 只测连通性，不写缓存。
func probeImageProxies(probeURL string) []imageProbeResult {
	type cand struct{ name, proxy string }
	cands := []cand{{"直连", ""}}
	for _, p := range getImageProxies() {
		cands = append(cands, cand{p, p})
	}
	results := make([]imageProbeResult, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, name, proxy string) {
			defer wg.Done()
			start := time.Now()
			_, _, err := fetchImageOnce(probeURL, proxy, 5*time.Second)
			r := imageProbeResult{Name: name, Ms: time.Since(start).Milliseconds()}
			if err != nil {
				r.Error = err.Error()
			} else {
				r.OK = true
			}
			results[i] = r
		}(i, c.name, c.proxy)
	}
	wg.Wait()
	return results
}

// imageProbeFallbackURL：取不到实时海报时的兜底探针图（2026-11-06 实测可达的豆瓣海报）。
const imageProbeFallbackURL = "https://img9.doubanio.com/view/photo/s_ratio_poster/public/p2935652656.jpg"

// probeImageURL：测试用的海报地址——先用当前豆瓣代理拉热门榜单取第一张封面，
// 拿不到就用兜底图。
func probeImageURL() string {
	if items, err := fetchDoubanListTimeout("movie", "热门", "recommend", 3, 5*time.Second); err == nil {
		for _, it := range items {
			if p, _ := it["poster"].(string); p != "" {
				return p
			}
		}
	}
	return imageProbeFallbackURL
}

// handleImageProxiesTest：POST /api/image-proxies/test（仅站长）。
// 对直连 + 每个已配图片代理真实拉一张海报，回显每路成功/失败 + 耗时。
func handleImageProxiesTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	u := probeImageURL()
	writeJSON(w, 200, map[string]any{"url": u, "results": probeImageProxies(u)})
}

// prefetchCatalogPosters：目录同步完成 / 服务启动后后台预取海报（并发 8），让常逛的页面是热缓存。
// 按分类各取最新 150 张（ORDER BY updated_at DESC）：以前无序取前 300 张，暖到的常是旧数据，
// 用户实际逛的分类首页和首页行反而是冷的。去重后总量约千张，后台跑，不阻塞任何请求。
func prefetchCatalogPosters() {
	if db == nil {
		return
	}
	kinds, err := db.Query(`SELECT DISTINCT kind FROM catalog WHERE kind != ''`)
	if err != nil {
		return
	}
	var kindList []string
	for kinds.Next() {
		var k string
		if err := kinds.Scan(&k); err == nil && k != "" {
			kindList = append(kindList, k)
		}
	}
	kinds.Close()
	seen := map[string]bool{}
	var urls []string
	for _, k := range kindList {
		rows, err := db.Query(`SELECT poster FROM catalog WHERE kind = ? AND poster LIKE 'http%' ORDER BY updated_at DESC LIMIT 150`, k)
		if err != nil {
			continue
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil && p != "" && !seen[p] {
				seen[p] = true
				urls = append(urls, p)
			}
		}
		rows.Close()
	}
	sem := make(chan struct{}, 8)
	for _, u := range urls {
		sem <- struct{}{}
		go func(raw string) {
			defer func() { <-sem }()
			_, _, _ = fetchAndCache(raw)
		}(u)
	}
	for i := 0; i < 8; i++ {
		sem <- struct{}{}
	}
}

func isPrivateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false // 解析失败交给上游报错，不在这里误杀
	}
	for _, ip := range ips {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return true
		}
		// Tailscale CGNAT 段 100.64.0.0/10 也挡
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] < 128 {
			return true
		}
	}
	return false
}
