package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// /api/image-proxy?url=<图片地址> —— 借 image-proxy 的机制，并扩展到所有海报
// 只给豆瓣/Bangumi/manmankan 走这条路，源站海报它也是直连；
// HomeMedia 网页端把全部海报都走这里，电视端源站海报
// 因 APK 的 getImageUrlSync 只改写 doubanio/bgm.tv，改不了——见 README。
// 安全：只放行 http/https、只转发 image/*、挡内网 IP（SSRF），单图 ≤10MB，15s 超时。
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
	if isPrivateHost(u.Hostname()) {
		writeJSON(w, 403, map[string]string{"error": "private host blocked"})
		return
	}
	body, ct, err := fetchAndCache(raw)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=604800, stale-while-revalidate=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(body)
}

// getImageProxies：服务端拉图代理列表。优先读设置里存的（meta image_proxies），
// 为空则读环境变量 HOME_MEDIA_IMAGE_PROXIES（逗号分隔）。直连永远先试，
// 代理只在直连失败/超时后按顺序兜底，不是一上来就全走代理。
func getImageProxies() []string {
	if db != nil {
		var v string
		if err := db.QueryRow(`SELECT v FROM meta WHERE k='image_proxies'`).Scan(&v); err == nil && v != "" {
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
	dataDir := getenv("HOME_MEDIA_DATA", "/opt/home-media/data")
	cacheDir := filepath.Join(dataDir, "image-cache")
	_ = os.MkdirAll(cacheDir, 0o755)
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	bodyPath := filepath.Join(cacheDir, key+".bin")
	metaPath := filepath.Join(cacheDir, key+".meta")
	if b, err := os.ReadFile(bodyPath); err == nil {
		if ct, err := os.ReadFile(metaPath); err == nil {
			return b, strings.TrimSpace(string(ct)), nil
		}
	}
	body, ct, err := fetchImageOnce(raw, "", 15*time.Second)
	if err != nil {
		for _, px := range getImageProxies() {
			if body, ct, err = fetchImageOnce(raw, px, 10*time.Second); err == nil {
				break
			}
		}
	}
	if err != nil {
		return nil, "", err
	}
	_ = os.WriteFile(bodyPath, body, 0o644)
	_ = os.WriteFile(metaPath, []byte(ct), 0o644)
	return body, ct, nil
}

// prefetchCatalogPosters：目录同步完成后后台预取海报（并发 4），让货架第一次打开就是热缓存
func prefetchCatalogPosters() {
	rows, err := db.Query(`SELECT DISTINCT poster FROM catalog WHERE poster LIKE 'http%' LIMIT 300`)
	if err != nil {
		return
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil && p != "" {
			urls = append(urls, p)
		}
	}
	rows.Close()
	sem := make(chan struct{}, 4)
	for _, u := range urls {
		sem <- struct{}{}
		go func(raw string) {
			defer func() { <-sem }()
			_, _, _ = fetchAndCache(raw)
		}(u)
	}
	for i := 0; i < 4; i++ {
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
