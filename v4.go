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
	// #2 负缓存：同 searchSource（WS 搜索与详情聚合都走这里）。
	if negCacheHit(s.Key, q) {
		return []SearchItem{}, nil
	}
	return searchSourceCtxDirect(ctx, s, q)
}

// searchSourceCtxDirect：不带负缓存的搜索本体（复测探针用：探针必须真的去问源站，
// 不能命中“上次失败”的负缓存而误判源已恢复）。
func searchSourceCtxDirect(ctx context.Context, s Source, q string) ([]SearchItem, error) {
	start := time.Now()
	d, err := cmsGetCtx(ctx, s.API, "ac=videolist&wd="+url.QueryEscape(q))
	if err != nil {
		recordSearchOutcome(s.Key, err, time.Since(start).Milliseconds())
		negCacheSet(s.Key, q)
		return nil, err
	}
	recordSearchOutcome(s.Key, nil, time.Since(start).Milliseconds())
	var out []SearchItem
	for _, m := range parseVodList(d) {
		typ, genre := sField(m, "type_name"), sField(m, "vod_class")
		if isSportsKind(typ, genre) || isAdultContent(typ, genre) {
			continue
		}
		out = append(out, SearchItem{
			ID: sField(m, "vod_id", "id"), Title: sField(m, "vod_name", "title"), Poster: sField(m, "vod_pic", "pic"),
			Year: sField(m, "vod_year", "year"), TypeName: typ,
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

type aggCacheEntry struct {
	sources    []aggSource
	totalCount int
	exp        time.Time
}

var (
	aggCacheMu        sync.Mutex
	aggCache          = map[string]aggCacheEntry{}
	aggCacheTTL       = 3 * time.Minute
	activeAggStreamMu sync.Mutex
	activeAggStreams  = map[string]context.CancelFunc{}
)

func resetAggCache() {
	aggCacheMu.Lock()
	aggCache = map[string]aggCacheEntry{}
	aggCacheMu.Unlock()
}

func aggCacheGet(want string) ([]aggSource, int, bool) {
	aggCacheMu.Lock()
	defer aggCacheMu.Unlock()
	e, ok := aggCache[want]
	if !ok {
		return nil, 0, false
	}
	if time.Now().After(e.exp) {
		delete(aggCache, want)
		return nil, 0, false
	}
	out := make([]aggSource, len(e.sources))
	copy(out, e.sources)
	return out, e.totalCount, true
}

func aggCacheSet(want string, sources []aggSource, totalCount int) {
	cp := make([]aggSource, len(sources))
	copy(cp, sources)
	aggCacheMu.Lock()
	aggCache[want] = aggCacheEntry{sources: cp, totalCount: totalCount, exp: time.Now().Add(aggCacheTTL)}
	aggCacheMu.Unlock()
}

// ---- 流式聚合详情：GET /api/detail-aggregate-stream?title=... ----
// 事件：start{total} / source{source} / speed{source, speed_kbs, resolution, latency_ms, score} / complete{total} / error{source, error}
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
	// 单用户流驱逐：同一账号切新详情或重连时，主动取消上一条未结束的旧聚合流，防并发流翻倍烧源站
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if u := currentUser(r); u != "" {
		activeAggStreamMu.Lock()
		if oldCancel, exists := activeAggStreams[u]; exists {
			oldCancel()
		}
		activeAggStreams[u] = cancel
		activeAggStreamMu.Unlock()
		defer func() {
			activeAggStreamMu.Lock()
			delete(activeAggStreams, u)
			activeAggStreamMu.Unlock()
		}()
	}

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

	want := stripVersionSuffix(title)
	// 3 分钟聚合结果短缓存：客户端自动重连或从详情点“直接播放”二进宫时直接秒推，不再把 29 个源重查一遍
	if r.URL.Query().Get("fresh") != "1" {
		if cachedSrcs, totalCnt, hit := aggCacheGet(want); hit {
			if !emit(map[string]any{"type": "start", "totalSources": totalCnt, "title": title, "cached": true}) {
				return
			}
			for _, sd := range cachedSrcs {
				sdCopy := sd
				if !emit(map[string]any{"type": "source", "source": &sdCopy}) {
					return
				}
				if sp, res, lat, err := speedTestSource(ctx, sdCopy.Episodes); err == nil {
					if !emit(map[string]any{"type": "speed", "source": sdCopy.Source, "id": sdCopy.ID, "speed_kbs": sp, "resolution": res, "latency_ms": lat, "score": scoreProbe(res, sp, lat)}) {
						return
					}
				}
			}
			emit(map[string]any{"type": "complete", "total": len(cachedSrcs)})
			return
		}
	}

	srcs := listSources()
	if !emit(map[string]any{"type": "start", "totalSources": len(srcs), "title": title}) {
		return
	}
	// 搜索与测速信号量拆离：searchSem（并发 6）搜完元数据立即释放，绝不被分片测速霸占槽位；
	// speedSem（并发 4）独立限制并发分片下载；单写 channel evCh 让主协程独占写 SSE，消除持锁 Flush 阻塞。
	searchSem := make(chan struct{}, 6)
	speedSem := make(chan struct{}, 4)
	evCh := make(chan map[string]any, 64)
	sendEv := func(ev map[string]any) bool {
		select {
		case evCh <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var wg sync.WaitGroup
	var collectMu sync.Mutex
	var collected []aggSource
	for _, s := range srcs {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			select {
			case searchSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			sd := aggregateOneSource(ctx, s, title, want)
			<-searchSem // 搜完立刻释放搜索槽位，不等测速
			if sd == nil || len(sd.Episodes) == 0 {
				return
			}
			collectMu.Lock()
			collected = append(collected, *sd)
			collectMu.Unlock()
			if !sendEv(map[string]any{"type": "source", "source": sd}) {
				return
			}
			// 测速进入独立信号量（若已命中 probeCache 则不占槽秒回）
			if len(sd.Episodes) > 0 {
				if m, ok := probeCacheGet(sd.Episodes[0]); ok {
					if m.OK {
						sendEv(map[string]any{"type": "speed", "source": sd.Source, "id": sd.ID, "speed_kbs": m.SpeedKBs, "resolution": m.Resolution, "latency_ms": m.LatencyMs, "score": m.Score})
					}
					return
				}
			}
			select {
			case speedSem <- struct{}{}:
				defer func() { <-speedSem }()
			case <-ctx.Done():
				return
			}
			if sp, res, lat, err := speedTestSource(ctx, sd.Episodes); err == nil {
				sendEv(map[string]any{"type": "speed", "source": sd.Source, "id": sd.ID, "speed_kbs": sp, "resolution": res, "latency_ms": lat, "score": scoreProbe(res, sp, lat)})
			}
		}()
	}
	go func() {
		wg.Wait()
		close(evCh)
	}()
	found := 0
	for ev := range evCh {
		if ev["type"] == "source" {
			found++
		}
		if !emit(ev) {
			cancel()
			return
		}
	}
	if ctx.Err() == nil {
		if len(collected) > 0 {
			aggCacheSet(want, collected, len(srcs))
		}
		emit(map[string]any{"type": "complete", "total": found})
	}
}

// 测速：复用 probePlayURL + probeCache（取第 1 集，与优选播放候选 URL 完全对齐），
// 自动具备 HTTP 200 校验、HTML 分享页真链提取、防盗链错误页拦截与完整首分片落盘缓存。
func speedTestSource(ctx context.Context, episodes []string) (float64, string, int64, error) {
	if len(episodes) == 0 {
		return 0, "", 0, fmt.Errorf("no episodes")
	}
	raw := episodes[0]
	if m, ok := probeCacheGet(raw); ok {
		if !m.OK {
			return 0, "", m.LatencyMs, fmt.Errorf("%s", m.Error)
		}
		return m.SpeedKBs, m.Resolution, m.LatencyMs, nil
	}
	sp, res, lat, err := probePlayURL(ctx, raw)
	m := probeMetrics{SpeedKBs: sp, Resolution: res, LatencyMs: lat}
	if err != nil {
		m.Error = err.Error()
		probeCacheSet(raw, m)
		return 0, res, lat, err
	}
	m.OK = true
	m.Score = scoreProbe(res, sp, lat)
	probeCacheSet(raw, m)
	return sp, res, lat, nil
}

// ---- 一键测全部源（探针逻辑见 health.go testSourceOnce） ----
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
			ok, ms, cats, errStr := testSourceOnce(s)
			out[i] = res{Key: s.Key, Name: s.Name, OK: ok, Ms: ms, Categories: cats, Error: errStr}
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
			addHealthDisabled(k)
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
var _ = io.Discard
var _ = regexp.MustCompile
