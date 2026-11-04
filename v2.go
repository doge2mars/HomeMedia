package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// v0.2：LunaTV 式配置 + 多源聚合详情
// - 源管理：增/删/禁用/测速（ac=list + 搜一词）
// - 直播源管理：M3U 地址入库、按分组解析频道
// - 聚合详情：按标题归一化精确匹配，多源并列，播放时可换源

func normTitle(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "　", "")
	return s
}

// stripVersionSuffix：去掉标题末尾的版本后缀，让“繁花 国语版/沪语版”与“繁花”归为同一部
// 注意只去版本词，不去“侠/行”这类正片字，避免“白夜”误并“白夜侠”
func stripVersionSuffix(s string) string {
	s = normTitle(s)
	for _, suf := range []string{"国语版", "沪语版", "粤语版", "普通话版", "中文版", "国语", "粤语", "沪语"} {
		if strings.HasSuffix(s, suf) && len(s) > len(suf) {
			return strings.TrimSuffix(s, suf)
		}
	}
	return s
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"username": currentUser(r), "role": r.Header.Get("X-Role")})
}

func handleSourcesV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Header.Get("X-Role") != "owner" {
		writeJSON(w, 403, map[string]string{"error": "owner only"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleSearchResources(w, r)
	case http.MethodPost:
		var b struct {
			Key      string `json:"key"`
			Name     string `json:"name"`
			API      string `json:"api"`
			Disabled bool   `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" || b.API == "" {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		dis := 0
		if b.Disabled {
			dis = 1
		}
		db.Exec(`INSERT OR REPLACE INTO sources(key, name, api, disabled) VALUES(?,?,?,?)`, b.Key, b.Name, b.API, dis)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		k := r.URL.Query().Get("key")
		if k == "" {
			writeJSON(w, 400, map[string]string{"error": "missing key"})
			return
		}
		db.Exec(`DELETE FROM sources WHERE key=?`, k)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleSourceTest(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	var api, name string
	err := db.QueryRow(`SELECT api, name FROM sources WHERE key=?`, key).Scan(&api, &name)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "source not found"})
		return
	}
	start := time.Now()
	cats, catErr := cmsCategories(api)
	_, searchErr := cmsGet(api, "ac=videolist&wd="+url.QueryEscape("繁花"))
	elapsed := time.Since(start).Milliseconds()
	ok := catErr == nil || searchErr == nil
	writeJSON(w, 200, map[string]any{
		"key": key, "name": name, "ok": ok,
		"categories": len(cats), "ms": elapsed,
		"category_error": errStr(catErr), "search_error": errStr(searchErr),
	})
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func handleLiveSourcesV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Header.Get("X-Role") != "owner" {
		writeJSON(w, 403, map[string]string{"error": "owner only"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleLiveSources(w, r)
	case http.MethodPost:
		var b struct {
			Key  string `json:"key"`
			Name string `json:"name"`
			URL  string `json:"url"`
			UA   string `json:"ua"`
			EPG  string `json:"epg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" || b.URL == "" {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		db.Exec(`INSERT OR REPLACE INTO livesources(key, name, url, ua, epg, disabled) VALUES(?,?,?,?,?,0)`, b.Key, b.Name, b.URL, b.UA, b.EPG)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		k := r.URL.Query().Get("key")
		if k == "" {
			writeJSON(w, 400, map[string]string{"error": "missing key"})
			return
		}
		db.Exec(`DELETE FROM livesources WHERE key=?`, k)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleLiveChannels(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("source")
	if key == "" {
		writeJSON(w, 400, map[string]string{"error": "missing source"})
		return
	}
	var u string
	err := db.QueryRow(`SELECT url FROM livesources WHERE key=?`, key).Scan(&u)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "live source not found"})
		return
	}
	ch, err := parseM3U(u)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	groups := map[string][]Channel{}
	var order []string
	for _, c := range ch {
		g := c.Group
		if g == "" {
			g = "未分组"
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	writeJSON(w, 200, map[string]any{"groups": groups, "order": order, "total": len(ch)})
}

// 聚合详情：全源搜标题，精确匹配（去空格）后逐源调详情拿完整集数与简介
func handleDetailAggregate(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	if title == "" {
		writeJSON(w, 400, map[string]string{"error": "missing title"})
		return
	}
	want := stripVersionSuffix(title)
	// 并行聚合（并发 5，整体 20 秒超时）：慢源不再拖垮全部，超时按已有结果返回，不静默等
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	srcs := listSources()
	perSrc := make([]*aggSource, len(srcs))
	var awg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for i, s := range srcs {
		i, s := i, s
		awg.Add(1)
		go func() {
			defer awg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			perSrc[i] = aggregateOneSource(ctx, s, title, want)
		}()
	}
	awg.Wait()
	type srcDetail = aggSource
	var out []srcDetail
	for _, sd := range perSrc {
		if sd != nil && len(sd.Episodes) > 0 {
			out = append(out, *sd)
			if len(out) >= 6 {
				break
			}
		}
	}
	if len(out) == 0 {
		writeJSON(w, 200, map[string]any{"title": title, "sources": []srcDetail{}, "error": "暂无可播源：各源都没有精确匹配到这个标题，换个关键词搜一下"})
		return
	}
	writeJSON(w, 200, map[string]any{"title": title, "sources": out})
}
