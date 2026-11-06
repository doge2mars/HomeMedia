package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 豆瓣榜单（首页发现行）：借 LunaTV 的做法，直接调豆瓣非官方接口拿榜单，
// 不用自己的播放数据（家里人太少，撑不起“热门”）。
//
// 上游（与 LunaTV douban.client.ts 同款）：
//   - 榜单：GET https://movie.douban.com/j/search_subjects?type=movie|tv&tag=热门&sort=recommend&page_limit=N&page_start=0
//     返回 {subjects: [{id, title, cover, rate, card_subtitle}]}；sort=recommend 按豆瓣综合热度，
//     sort=time 最新，sort=rank 按评分。
//   - 反爬：UA 伪装成 Chrome + Referer https://movie.douban.com/（LunaTV 同款头）。
// 缓存 24 小时（LunaTV 同款）；上游失败时返回 502，前端回退到目录库最新入库。
//
// 豆瓣代理（学 LunaTV 的可选项，服务端直连版；存在 meta 表 douban_proxy，站长在设置页改）：
//   direct  直连 movie.douban.com（默认）
//   tencent CMLiussss 腾讯云 CDN：movie.douban.cmliussss.net
//   ali     CMLiussss 阿里云 CDN：movie.douban.cmliussss.com
//   zwei    Zwei 的 CORS 代理：https://ciao-cors.is-an.org/ + URL 编码后的目标地址
//   custom  自定义：填自己的镜像域名（如 https://movie.douban.cmliussss.net）
// 未设置时读环境变量 HOME_MEDIA_DOUBAN_PROXY_TYPE / HOME_MEDIA_DOUBAN_PROXY_URL；
// 都没有时默认 direct。改代理后榜单缓存会清空，立即生效。

const doubanCacheTTL = 24 * time.Hour

var doubanYearRe = regexp.MustCompile(`(\d{4})`)

// doubanProxyTypes：换 base 域名的代理（与 LunaTV 的 cmliussss-cdn-* 同款）。
var doubanProxyTypes = map[string]string{
	"direct":  "https://movie.douban.com",
	"tencent": "https://movie.douban.cmliussss.net",
	"ali":     "https://movie.douban.cmliussss.com",
}

type doubanProxyCfg struct {
	Type   string `json:"type"`
	Custom string `json:"custom"`
}

func validDoubanProxyType(t string) bool {
	if _, ok := doubanProxyTypes[t]; ok {
		return true
	}
	return t == "zwei" || t == "custom"
}

// getDoubanProxy：meta 优先，环境变量兜底，默认 direct。
// 返回 (type, customBase)；custom 为空时按 direct 处理。
func getDoubanProxy() (string, string) {
	if v, ok := metaGet("douban_proxy"); ok {
		var c doubanProxyCfg
		if err := json.Unmarshal([]byte(v), &c); err == nil && validDoubanProxyType(strings.TrimSpace(c.Type)) {
			return strings.TrimSpace(c.Type), strings.TrimSpace(c.Custom)
		}
	}
	t := strings.TrimSpace(getenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "direct"))
	if t == "" || !validDoubanProxyType(t) {
		t = "direct"
	}
	return t, strings.TrimSpace(getenv("HOME_MEDIA_DOUBAN_PROXY_URL", ""))
}

// doubanListURL：按当前代理拼榜单地址。
func doubanListURL(doubanType, tag, sort string, limit int) string {
	typ, custom := getDoubanProxy()
	return doubanListURLWith(typ, custom, doubanType, tag, sort, limit)
}

// doubanListURLWith：按指定档位拼榜单地址（代理测试用：未保存的档位也能测）。
// zwei 是前缀式代理（目标地址整体 URL 编码后接在后面，
// 与 LunaTV fetchWithTimeout 同款）；其余是换 base 域名。
func doubanListURLWith(typ, custom, doubanType, tag, sort string, limit int) string {
	base, ok := doubanProxyTypes[typ]
	if !ok {
		if typ == "custom" && custom != "" {
			base = strings.TrimSuffix(custom, "/")
		} else {
			base = doubanProxyTypes["direct"]
		}
	}
	direct := fmt.Sprintf("%s/j/search_subjects?type=%s&tag=%s&sort=%s&page_limit=%d&page_start=0",
		base, url.QueryEscape(doubanType), url.QueryEscape(tag), url.QueryEscape(sort), limit)
	if typ == "zwei" {
		return "https://ciao-cors.is-an.org/" + url.QueryEscape(direct)
	}
	return direct
}

type doubanSubject struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Cover        string `json:"cover"`
	Rate         string `json:"rate"`
	CardSubtitle string `json:"card_subtitle"`
}

type doubanListResp struct {
	Subjects []doubanSubject `json:"subjects"`
}

type doubanCacheEntry struct {
	items []map[string]any
	at    time.Time
}

var (
	doubanCacheMu sync.Mutex
	doubanCache   = map[string]doubanCacheEntry{}
)

// fetchDoubanList：调豆瓣榜单接口，返回首页卡片直接能用的条目
// （title/poster/rating/year/type，与 /api/catalog 的条目字段对齐）。
func fetchDoubanList(doubanType, tag, sort string, limit int) ([]map[string]any, error) {
	return fetchDoubanListTimeout(doubanType, tag, sort, limit, 10*time.Second)
}

// fetchDoubanListTimeout：同上，可指定超时（代理测试用 5 秒）。
func fetchDoubanListTimeout(doubanType, tag, sort string, limit int, timeout time.Duration) ([]map[string]any, error) {
	return fetchDoubanURL(doubanListURL(doubanType, tag, sort, limit), doubanType, timeout)
}

// fetchDoubanListWithProxy：用指定档位（未保存的也行，供代理测试）拉榜单。
func fetchDoubanListWithProxy(typ, custom, doubanType, tag, sort string, limit int, timeout time.Duration) ([]map[string]any, error) {
	return fetchDoubanURL(doubanListURLWith(typ, custom, doubanType, tag, sort, limit), doubanType, timeout)
}

// fetchDoubanURL：按给定地址调榜单接口并解析成首页卡片条目。
func fetchDoubanURL(u, doubanType string, timeout time.Duration) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://movie.douban.com/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("douban upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var d doubanListResp
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	kind := "电影"
	if doubanType == "tv" {
		kind = "剧集"
	}
	items := make([]map[string]any, 0, len(d.Subjects))
	for _, s := range d.Subjects {
		if s.Title == "" {
			continue
		}
		year := ""
		if m := doubanYearRe.FindStringSubmatch(s.CardSubtitle); m != nil {
			year = m[1]
		}
		// 地区：card_subtitle 形如 "2026 / 中国大陆 / 剧情"，第二段是地区
		// （第五批：综艺“国内/国外” pill 靠它在前端过滤；没有地区信息的条目 region 为空）
		region := ""
		if parts := strings.Split(s.CardSubtitle, "/"); len(parts) >= 2 {
			region = strings.TrimSpace(parts[1])
		}
		items = append(items, map[string]any{
			"title":  s.Title,
			"poster": s.Cover,
			"rating": s.Rate,
			"year":   year,
			"region": region,
			"type":   kind,
		})
	}
	return items, nil
}

func handleDouban(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	if typ != "movie" && typ != "tv" {
		writeJSON(w, 400, map[string]string{"error": "type must be movie or tv"})
		return
	}
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		tag = "热门"
	}
	sort := r.URL.Query().Get("sort")
	if sort != "recommend" && sort != "time" && sort != "rank" {
		sort = "recommend"
	}
	limit := 12
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 50 {
			limit = n
		}
	}
	proxyType, proxyCustom := getDoubanProxy()
	key := typ + "|" + tag + "|" + sort + "|" + strconv.Itoa(limit) + "|" + proxyType + "|" + proxyCustom
	doubanCacheMu.Lock()
	if e, ok := doubanCache[key]; ok && time.Since(e.at) < doubanCacheTTL {
		doubanCacheMu.Unlock()
		writeJSON(w, 200, map[string]any{"items": e.items, "source": "douban"})
		return
	}
	doubanCacheMu.Unlock()
	items, err := fetchDoubanList(typ, tag, sort, limit)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	doubanCacheMu.Lock()
	doubanCache[key] = doubanCacheEntry{items: items, at: time.Now()}
	doubanCacheMu.Unlock()
	writeJSON(w, 200, map[string]any{"items": items, "source": "douban"})
}

// handleDoubanProxyGet/Save：豆瓣代理选择（设置页“豆瓣代理”组），与图片代理同款做法。
// 仅站长可改；改完清榜单缓存，立即生效。
func handleDoubanProxyGet(w http.ResponseWriter, r *http.Request) {
	typ, custom := getDoubanProxy()
	writeJSON(w, 200, map[string]any{"type": typ, "custom": custom})
}

func handleDoubanProxySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var body doubanProxyCfg
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	body.Type = strings.TrimSpace(body.Type)
	body.Custom = strings.TrimSpace(body.Custom)
	if !validDoubanProxyType(body.Type) {
		writeJSON(w, 400, map[string]string{"error": "bad proxy type"})
		return
	}
	if body.Type == "custom" {
		u, err := url.Parse(body.Custom)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, 400, map[string]string{"error": "bad custom proxy url"})
			return
		}
		body.Custom = strings.TrimSuffix(body.Custom, "/")
	}
	b, _ := json.Marshal(body)
	if err := metaSet("douban_proxy", string(b)); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// 换代理立即生效：清榜单缓存
	doubanCacheMu.Lock()
	doubanCache = map[string]doubanCacheEntry{}
	doubanCacheMu.Unlock()
	writeJSON(w, 200, map[string]any{"type": body.Type, "custom": body.Custom})
}

// handleDoubanProxyTest：POST /api/douban-proxy/test（仅站长）。
// 用当前选中的档位（body 可带 type/custom，未带则用已保存的）真实拉一次榜单
// （tag=热门，limit=1，5 秒超时，不走缓存），回显成功/失败 + 耗时。
// 上游失败也返回 200（ok=false），让设置页直接显示原因，而不是弹通用错误。
func handleDoubanProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var body doubanProxyCfg
	_ = json.NewDecoder(r.Body).Decode(&body)
	body.Type = strings.TrimSpace(body.Type)
	body.Custom = strings.TrimSpace(body.Custom)
	if body.Type == "" {
		body.Type, body.Custom = getDoubanProxy()
	}
	if !validDoubanProxyType(body.Type) {
		writeJSON(w, 400, map[string]string{"error": "bad proxy type"})
		return
	}
	if body.Type == "custom" {
		u, err := url.Parse(body.Custom)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, 400, map[string]string{"error": "bad custom proxy url"})
			return
		}
		body.Custom = strings.TrimSuffix(body.Custom, "/")
	}
	start := time.Now()
	items, err := fetchDoubanListWithProxy(body.Type, body.Custom, "movie", "热门", "recommend", 1, 5*time.Second)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "ms": ms, "type": body.Type, "custom": body.Custom, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ms": ms, "type": body.Type, "custom": body.Custom, "items": len(items)})
}
