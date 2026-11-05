package main

// 演进文档 §3.1 稳定性三件里的 #1、#2：
//   #1 源健康度自动降级（借 LibreTV）：搜索时记每个源的连续失败次数，连续失败 3 次
//      自动把该源 disabled（只停用、不删除）；每 6 小时定时复测，恢复的源自动重新启用。
//      手动停用的源不在复测名单里，绝不会被自动恢复。
//   #2 搜索失败负缓存：单源对某词刚失败过（403 / 超时 / 网络错，10 分钟内），
//      直接返回空、不再发请求；空结果不是失败，不进负缓存、不记健康。
//
// 失败计数放内存（重启清零可以接受：停用状态本身持久化在 sources.disabled +
// meta.health_disabled，重启后复测循环会接手）；负缓存也是内存。

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	// healthFailThreshold：连续失败几次自动停用（单次抽风不停用）。
	healthFailThreshold = 3
	// healthRetestInterval：复测周期。
	healthRetestInterval = 6 * time.Hour
	// healthRetestFirstDelay：启动后多久做第一次复测（部署/重启后被停用的源能尽快恢复）。
	healthRetestFirstDelay = time.Minute
	// healthProbeTimeout：复测探针超时——按搜索真实预算（WS 搜索单源 10 秒）来，
	// 一个 12 秒才响应的源能通过 15 秒探针、却会在每次搜索里超时，复测用 15 秒
	// 只会让它刚恢复又被连续失败停用、来回抖动。
	healthProbeTimeout = 10 * time.Second
)

// negCacheTTL：负缓存存活时间（测试可改短）。
var negCacheTTL = 10 * time.Minute

// ---- #2 负缓存：key 为 "源key::query"，只记失败，不记空结果 ----
var (
	negMu    sync.Mutex
	negCache = map[string]time.Time{}
)

func negCacheKey(key, q string) string { return key + "::" + q }

func negCacheHit(key, q string) bool {
	negMu.Lock()
	defer negMu.Unlock()
	exp, ok := negCache[negCacheKey(key, q)]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(negCache, negCacheKey(key, q))
		return false
	}
	return true
}

func negCacheSet(key, q string) {
	negMu.Lock()
	defer negMu.Unlock()
	negCache[negCacheKey(key, q)] = time.Now().Add(negCacheTTL)
}

// ---- #1 健康计数 ----
type srcHealth struct {
	ConsecutiveFails int   `json:"consecutive_fails"`
	LastFailUnix     int64 `json:"last_fail_unix"`
	LastOKUnix       int64 `json:"last_ok_unix"`
	LastLatencyMs    int64 `json:"last_latency_ms"`
}

var (
	healthMu  sync.Mutex
	healthMap = map[string]*srcHealth{}
)

func getHealth(key string) srcHealth {
	healthMu.Lock()
	defer healthMu.Unlock()
	if h, ok := healthMap[key]; ok {
		return *h
	}
	return srcHealth{}
}

// recordSearchOutcome：每次搜索（REST / WS / 详情聚合都经 searchSource/searchSourceCtx）
// 记一次结果。成功清零失败计数；失败累加，到 3 次整时自动停用一次（==3 才触发，
// 之后再失败不再重复写库）。
func recordSearchOutcome(key string, err error, latencyMs int64) {
	healthMu.Lock()
	h, ok := healthMap[key]
	if !ok {
		h = &srcHealth{}
		healthMap[key] = h
	}
	var disable bool
	if err == nil {
		h.ConsecutiveFails = 0
		h.LastOKUnix = time.Now().Unix()
		h.LastLatencyMs = latencyMs
	} else {
		h.ConsecutiveFails++
		h.LastFailUnix = time.Now().Unix()
		if h.ConsecutiveFails == healthFailThreshold {
			disable = true
		}
	}
	healthMu.Unlock()
	if disable {
		autoDisableSource(key)
	}
}

func autoDisableSource(key string) {
	if db == nil {
		return
	}
	if _, err := db.Exec(`UPDATE sources SET disabled=1 WHERE key=?`, key); err != nil {
		log.Printf("health: auto-disable %s failed: %v", key, err)
		return
	}
	keys := getHealthDisabled()
	for _, k := range keys {
		if k == key {
			log.Printf("health: source %s auto-disabled after %d consecutive search failures (retest every %v)", key, healthFailThreshold, healthRetestInterval)
			return
		}
	}
	setHealthDisabled(append(keys, key))
	log.Printf("health: source %s auto-disabled after %d consecutive search failures (retest every %v)", key, healthFailThreshold, healthRetestInterval)
}

// getHealthDisabled/setHealthDisabled：meta.health_disabled（JSON 数组），
// 只记“健康系统自动停用的源”，手动停用的源不在里面，复测绝不会碰它们。
func getHealthDisabled() []string {
	if db == nil {
		return nil
	}
	var v string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k='health_disabled'`).Scan(&v); err != nil {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(v), &keys); err != nil {
		return nil
	}
	return keys
}

func setHealthDisabled(keys []string) {
	if db == nil {
		return
	}
	b, _ := json.Marshal(keys)
	_, _ = db.Exec(`INSERT INTO meta(k, v) VALUES('health_disabled', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
}

func removeHealthDisabled(key string) {
	keys := getHealthDisabled()
	var kept []string
	for _, k := range keys {
		if k != key {
			kept = append(kept, k)
		}
	}
	setHealthDisabled(kept)
}

// testSourceOnce：手动“测全部源”共用的单源探针（分类 + 搜“繁花”，走 15 秒 httpClient
// 超时）。这是人工体检（越全越好）；自动复测不用它，见 probeSourceHealth。
func testSourceOnce(s Source) (ok bool, ms int64, cats int, errStr string) {
	start := time.Now()
	cs, catErr := cmsCategories(s.API)
	_, searchErr := cmsGet(s.API, "ac=videolist&wd="+url.QueryEscape("繁花"))
	if catErr != nil && searchErr != nil {
		errStr = catErr.Error()
	}
	return catErr == nil || searchErr == nil, time.Since(start).Milliseconds(), len(cs), errStr
}

// probeSourceHealth：复测探针——按搜索真实预算（10 秒）对“繁花”搜一次，
// 能在搜索预算内答上来的才算恢复。注意 searchSourceCtx 里的钩子会在探针失败时
// 再记一次失败、成功时清零计数，都是想要的副作用，不用绕开；但必须走
// searchSourceCtxDirect 绕开负缓存，否则“上次失败”的缓存会让探针误判源已恢复。
func probeSourceHealth(s Source) bool {
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	_, err := searchSourceCtxDirect(ctx, s, "繁花")
	return err == nil
}

type healthRetestResult struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Action string `json:"action"` // reenabled | still_disabled
}

// retestHealthSources：复测全部健康停用的源。恢复的重新启用并移出名单；
// 仍失败的继续停用；源已被手动删除的，从名单里清掉。
func retestHealthSources() []healthRetestResult {
	if db == nil {
		return nil
	}
	var out []healthRetestResult
	for _, k := range getHealthDisabled() {
		var s Source
		if err := db.QueryRow(`SELECT key, name, api FROM sources WHERE key=?`, k).Scan(&s.Key, &s.Name, &s.API); err != nil {
			removeHealthDisabled(k)
			continue
		}
		if probeSourceHealth(s) {
			if _, err := db.Exec(`UPDATE sources SET disabled=0 WHERE key=?`, k); err != nil {
				log.Printf("health: re-enable %s failed: %v", k, err)
				out = append(out, healthRetestResult{Key: k, Name: s.Name, OK: true, Action: "still_disabled"})
				continue
			}
			removeHealthDisabled(k)
			log.Printf("health: source %s recovered, re-enabled", k)
			out = append(out, healthRetestResult{Key: k, Name: s.Name, OK: true, Action: "reenabled"})
		} else {
			out = append(out, healthRetestResult{Key: k, Name: s.Name, OK: false, Action: "still_disabled"})
		}
	}
	return out
}

// healthRetestLoop：启动 1 分钟后先复测一次，之后每 6 小时。
func healthRetestLoop() {
	time.Sleep(healthRetestFirstDelay)
	retestHealthSources()
	for range time.Tick(healthRetestInterval) {
		retestHealthSources()
	}
}

// ---- 健康状态接口 ----

func handleSourcesHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	type row struct {
		Key              string `json:"key"`
		Name             string `json:"name"`
		Disabled         bool   `json:"disabled"`
		HealthDisabled   bool   `json:"health_disabled"`
		ConsecutiveFails int    `json:"consecutive_fails"`
		LastFailUnix     int64  `json:"last_fail_unix"`
		LastOKUnix       int64  `json:"last_ok_unix"`
		LastLatencyMs    int64  `json:"last_latency_ms"`
	}
	out := []row{}
	if db != nil {
		rows, _ := db.Query(`SELECT key, name, api, disabled FROM sources ORDER BY key`)
		if rows != nil {
			defer rows.Close()
			hd := map[string]bool{}
			for _, k := range getHealthDisabled() {
				hd[k] = true
			}
			for rows.Next() {
				var k, n, a string
				var dis int
				if err := rows.Scan(&k, &n, &a, &dis); err != nil {
					continue
				}
				h := getHealth(k)
				out = append(out, row{Key: k, Name: n, Disabled: dis == 1, HealthDisabled: hd[k],
					ConsecutiveFails: h.ConsecutiveFails, LastFailUnix: h.LastFailUnix,
					LastOKUnix: h.LastOKUnix, LastLatencyMs: h.LastLatencyMs})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"sources": out})
}

func handleSourcesHealthRetest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	writeJSON(w, 200, map[string]any{"results": retestHealthSources()})
}
