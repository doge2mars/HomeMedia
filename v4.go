package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// v0.4：多源时代的三个主流做法
// 1) 聚合不再串行等全部：并行 + 流式（SSE）先到先播，客户端断开即取消，后台不积压
// 2) 测速聚合：每源取一集 m3u8 测首分片速度 + 解析分辨率，默认播最快源，失败自动换下一个
// 3) 源去重：一键测全部源 + 按归一化 api 去重 + 禁用失败源

// ---- 归一化 api：去尾斜杠、小写 host，用于去重 ----
// 注意：查询串不能去掉——有些源是代理包装地址（如 ?url=https://真源/api），
// 去掉查询串会把不同真源误判成同一个代理（已实测误伤 lovedan.net）
func normAPI(api string) string {
	api = strings.TrimSpace(api)
	api = strings.TrimRight(api, "/")
	if u, err := url.Parse(api); err == nil && u.Host != "" {
		u.Host = strings.ToLower(u.Host)
		u.Fragment = ""
		return strings.TrimRight(u.String(), "/")
	}
	return api
}

// ---- 单源聚合详情（供并行/流式共用） ----
type aggSource struct {
	Source     string   `json:"source"`
	SourceName string   `json:"source_name"`
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Poster     string   `json:"poster"`
	Year       string   `json:"year"`
	TypeName   string   `json:"type_name"`
	Synopsis   string   `json:"synopsis"`
	Episodes   []string `json:"episodes"`
	SpeedKBs   float64  `json:"speed_kbs,omitempty"`
	Resolution string   `json:"resolution,omitempty"`
	SpeedErr   string   `json:"speed_error,omitempty"`
}

func aggregateOneSource(ctx context.Context, s Source, title, want string) *aggSource {
	items, err := searchSourceCtx(ctx, s, title)
	if err != nil {
		return nil
	}
	for _, it := range items {
		if stripVersionSuffix(it.Title) != want {
			continue
		}
		sd := &aggSource{Source: s.Key, SourceName: s.Name, ID: it.ID, Title: it.Title, Poster: it.Poster, Year: it.Year, TypeName: it.TypeName, Episodes: it.Episodes}
		if d, err := cmsGetCtx(ctx, s.API, "ac=videolist&ids="+url.QueryEscape(it.ID)); err == nil {
			if lst := parseVodList(d); len(lst) > 0 {
				m := lst[0]
				if eps := parsePlayURLs(sField(m, "vod_play_url")); len(eps) > 0 {
					sd.Episodes = eps
				}
				if v := sField(m, "vod_content", "vod_blurb"); v != "" {
					sd.Synopsis = v
				}
				if v := sField(m, "vod_pic", "pic"); v != "" {
					sd.Poster = v
				}
			}
		}
		return sd
	}
	return nil
}

func searchSourceCtx(ctx context.Context, s Source, q string) ([]SearchItem, error) {
	d, err := cmsGetCtx(ctx, s.API, "ac=videolist&wd="+url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	var out []SearchItem
	for _, m := range parseVodList(d) {
		out = append(out, SearchItem{
			ID: sField(m, "vod_id", "id"), Title: sField(m, "vod_name", "title"), Poster: sField(m, "vod_pic", "pic"),
			Year: sField(m, "vod_year", "year"), TypeName: sField(m, "type_name"),
			Source: s.Key, SourceName: s.Name, Episodes: parsePlayURLs(sField(m, "vod_play_url")),
		})
	}
	return out, nil
}

func cmsGetCtx(ctx context.Context, api, query string) (map[string]any, error) {
	u := strings.TrimRight(api, "/") + "/" + "?" + query
	u = strings.Replace(u, "vod//?", "vod/?", 1)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	var d map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return d, nil
}

// ---- 流式聚合详情：GET /api/detail-aggregate-stream?title=... ----
// 事件：start{total} / source{source} / speed{source, speed_kbs, resolution} / complete{total} / error{source, error}
func handleDetailAggregateStream(w http.ResponseWriter, r *http.Request) {
	// 防崩：SSE 断开后后台 goroutine 再写已回收的 ResponseWriter 会 panic，
	// 2026-11-04 实测一次客户端断开直接拖垮整个服务进程；此处兜底，单个流死不拖全站
	defer func() { _ = recover() }()
	title := r.URL.Query().Get("title")
	if title == "" {
		writeJSON(w, 400, map[string]string{"error": "missing title"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "no flusher"})
		return
	}
	ctx := r.Context()
	srcs := listSources()
	emit := func(v any) (ok bool) {
		// 断开后不再写；Flush 已回收的 Writer 会 panic，用 recover 转成 false
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		if ctx.Err() != nil {
			return false
		}
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !emit(map[string]any{"type": "start", "totalSources": len(srcs), "title": title}) {
		return
	}
	want := stripVersionSuffix(title)
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	var mu sync.Mutex
	found := 0
	for _, s := range srcs {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			sd := aggregateOneSource(ctx, s, title, want)
			if sd == nil || len(sd.Episodes) == 0 {
				return
			}
			mu.Lock()
			found++
			ok := emit(map[string]any{"type": "source", "source": sd})
			mu.Unlock()
			if !ok {
				return
			}
			// 测速与详情并行后发：先让用户播，不等测速
			if sp, res, err := speedTestSource(ctx, sd.Episodes); err == nil {
				mu.Lock()
				emit(map[string]any{"type": "speed", "source": sd.Source, "id": sd.ID, "speed_kbs": sp, "resolution": res})
				mu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		emit(map[string]any{"type": "complete", "total": found})
	case <-ctx.Done():
		// 客户端断开（用户点了另一个视频）：直接停，后台 goroutine 随 ctx 取消，不积压
		return
	}
}

// 测速：取一集 m3u8（优先第 2 集，更具代表性，Selene-TV 同款思路），
// 解析 master 播放列表的最高分辨率，下载首个分片一部分算速度；8 秒超时，失败不淘汰该源
func speedTestSource(ctx context.Context, episodes []string) (float64, string, error) {
	if len(episodes) == 0 {
		return 0, "", fmt.Errorf("no episodes")
	}
	idx := 0
	if len(episodes) > 1 {
		idx = 1
	}
	raw := episodes[idx]
	ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, "", err
	}
	text := string(body)
	res := ""
	if strings.Contains(text, "#EXT-X-STREAM-INF") {
		// master 播放列表：取最大的 RESOLUTION
		re := regexp.MustCompile(`RESOLUTION=(\d+x\d+)`)
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if m[1] > res {
				res = m[1]
			}
		}
		// 取第一个变体地址继续测分片
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "#EXT-X-STREAM-INF") && i+1 < len(lines) {
				u := strings.TrimSpace(lines[i+1])
				if u != "" && !strings.HasPrefix(u, "#") {
					if !strings.HasPrefix(u, "http") {
						if b, err := url.Parse(raw); err == nil {
							if uu, err := b.Parse(u); err == nil {
								u = uu.String()
							}
						}
					}
					raw = u
					break
				}
			}
		}
		// 变体 m3u8 再取一次
		req2, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
		req2.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
		if resp2, err := httpClient.Do(req2); err == nil {
			defer resp2.Body.Close()
			b2, _ := io.ReadAll(io.LimitReader(resp2.Body, 2<<20))
			text = string(b2)
		}
	}
	// 首个分片地址
	segURL := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			segURL = t
			break
		}
	}
	if segURL == "" {
		return 0, res, fmt.Errorf("no segment")
	}
	if !strings.HasPrefix(segURL, "http") {
		if b, err := url.Parse(raw); err == nil {
			if uu, err := b.Parse(segURL); err == nil {
				segURL = uu.String()
			}
		}
	}
	start := time.Now()
	req3, _ := http.NewRequestWithContext(ctx2, "GET", segURL, nil)
	req3.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp3, err := httpClient.Do(req3)
	if err != nil {
		return 0, res, err
	}
	defer resp3.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp3.Body, 512<<10))
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	return float64(n) / 1024 / elapsed, res, nil
}

// ---- 一键测全部源 ----
func handleSourcesTestAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	srcs := listSources()
	type res struct {
		Key        string `json:"key"`
		Name       string `json:"name"`
		OK         bool   `json:"ok"`
		Ms         int64  `json:"ms"`
		Categories int    `json:"categories"`
		Error      string `json:"error,omitempty"`
	}
	out := make([]res, len(srcs))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i, s := range srcs {
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			cats, catErr := cmsCategories(s.API)
			_, searchErr := cmsGet(s.API, "ac=videolist&wd="+url.QueryEscape("繁花"))
			errStr := ""
			if catErr != nil && searchErr != nil {
				errStr = catErr.Error()
			}
			out[i] = res{Key: s.Key, Name: s.Name, OK: catErr == nil || searchErr == nil, Ms: time.Since(start).Milliseconds(), Categories: len(cats), Error: errStr}
		}()
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"results": out})
}

func handleSourcesDisableFailed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	n := 0
	for _, k := range b.Keys {
		if _, err := db.Exec(`UPDATE sources SET disabled=1 WHERE key=?`, k); err == nil {
			n++
		}
	}
	writeJSON(w, 200, map[string]any{"disabled": n})
}

// ---- 源去重：按归一化 api 分组，保留第一个启用的，其余删除 ----
func handleSourcesDedupe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	rows, _ := db.Query(`SELECT key, name, api, disabled FROM sources ORDER BY key`)
	defer rows.Close()
	seen := map[string]string{}
	var removed []map[string]string
	for rows.Next() {
		var k, n, a string
		var dis int
		rows.Scan(&k, &n, &a, &dis)
		norm := normAPI(a)
		if first, ok := seen[norm]; ok {
			db.Exec(`DELETE FROM sources WHERE key=?`, k)
			removed = append(removed, map[string]string{"key": k, "name": n, "api": a, "kept": first})
		} else {
			seen[norm] = k
		}
	}
	writeJSON(w, 200, map[string]any{"removed": removed, "count": len(removed)})
}

// 订阅导入时也去重：同一归一化 api 已存在时，更新已有源、不建新 key
func dedupeOnSubscribe(s subSource) (string, bool) {
	rows, _ := db.Query(`SELECT key, api FROM sources`)
	defer rows.Close()
	for rows.Next() {
		var k, a string
		rows.Scan(&k, &a)
		if normAPI(a) == normAPI(s.API) {
			db.Exec(`UPDATE sources SET name=? WHERE key=?`, s.Name, k)
			return k, true
		}
	}
	return "", false
}

var _ = sort.StringSlice{}
var _ = strconv.Itoa
