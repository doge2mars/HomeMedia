package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// ---- 苹果 CMS 通用字段容错：vod_name/title、vod_pic/pic、vod_year/year 等 ----
func sField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				return t
			case float64:
				return strconv.FormatInt(int64(t), 10)
			}
		}
	}
	return ""
}
func iField(m map[string]any, keys ...string) int64 {
	s := sField(m, keys...)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

type Source struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	API  string `json:"api"`
}

func listSources() []Source {
	rows, _ := db.Query(`SELECT key, name, api FROM sources WHERE disabled=0 ORDER BY key`)
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		rows.Scan(&s.Key, &s.Name, &s.API)
		out = append(out, s)
	}
	return out
}

func cmsGet(api, query string) (map[string]any, error) {
	u := strings.TrimRight(api, "/") + "/" + "?" + query
	// api 本身已含 /api.php/provide/vod/ 时上面会多一个 /，CMS 仍可接受；规范化一下
	u = strings.Replace(u, "vod//?", "vod/?", 1)
	req, _ := http.NewRequest("GET", u, nil)
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

type SearchItem struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Poster   string   `json:"poster"`
	Year     string   `json:"year"`
	TypeName string   `json:"type_name"`
	Source   string   `json:"source"`
	SourceName string `json:"source_name"`
	Episodes []string `json:"episodes,omitempty"`
}

func parseVodList(d map[string]any) []map[string]any {
	arr, _ := d["list"].([]any)
	out := []map[string]any{}
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// 播放串解析：CMS vod_play_url 形如 "线路$第1集$url#第2集$url$$$线路2$..."
// 返回：按播放组切分的集数 URL 列表（取第一个含 m3u8 的组，兼容直接 URL）
func parsePlayURLs(playURL string) []string {
	if playURL == "" {
		return nil
	}
	groups := strings.Split(playURL, "$$$")
	for _, g := range groups {
		parts := strings.Split(g, "#")
		var eps []string
		for _, p := range parts {
			seg := strings.Split(p, "$")
			if len(seg) >= 2 {
				eps = append(eps, seg[len(seg)-1])
			} else if strings.Contains(p, "http") {
				eps = append(eps, p)
			}
		}
		if len(eps) > 0 {
			return eps
		}
	}
	return nil
}

func searchSource(s Source, q string) ([]SearchItem, error) {
	d, err := cmsGet(s.API, "ac=videolist&wd="+url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	var out []SearchItem
	for _, m := range parseVodList(d) {
		id := sField(m, "vod_id", "id")
		eps := parsePlayURLs(sField(m, "vod_play_url"))
		out = append(out, SearchItem{
			ID: id, Title: sField(m, "vod_name", "title"), Poster: sField(m, "vod_pic", "pic"),
			Year: sField(m, "vod_year", "year"), TypeName: sField(m, "type_name"),
			Source: s.Key, SourceName: s.Name, Episodes: eps,
		})
	}
	return out, nil
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		q = r.URL.Query().Get("wd")
	}
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "missing q"})
		return
	}
	// 记录搜索历史（网页端 + 电视端共用）
	db.Exec(`INSERT OR REPLACE INTO searchhistory(username, keyword, updated_at) VALUES(?,?,?)`, currentUser(r), q, time.Now().UnixMilli())
	// 服务端搜索缓存 10 分钟：同一词不再实时拉各源，换设备也命中；?fresh=1 强制刷新
	if r.URL.Query().Get("fresh") != "1" {
		if v, ok := cacheGet("search:" + q); ok {
			w.Header().Set("X-Cache", "hit")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(v))
			return
		}
	}
	// 并行搜各源（并发 5），不再一个源卡住拖垮全部；按源顺序拼回结果
	srcs := listSources()
	perSrc := make([][]SearchItem, len(srcs))
	var swg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for i, s := range srcs {
		i, s := i, s
		swg.Add(1)
		go func() {
			defer swg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if items, err := searchSource(s, q); err == nil {
				perSrc[i] = items
			}
		}()
	}
	swg.Wait()
	var all []SearchItem
	for _, items := range perSrc {
		all = append(all, items...)
		if len(all) > 100 {
			all = all[:100]
			break
		}
	}
	data, _ := json.Marshal(map[string]any{"results": all, "count": len(all)})
	cacheSet("search:"+q, string(data), 10*time.Minute)
	w.Header().Set("X-Cache", "miss")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func handleSearchWS(w http.ResponseWriter, r *http.Request) {
	// Selene-TV 服务端聚合搜索（SSE）：type=start/source_result/source_error/complete
	defer func() { _ = recover() }() // 与 detail-aggregate-stream 同款防崩：流写死不拖全站
	q := r.URL.Query().Get("q")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "no flusher"})
		return
	}
	srcs := listSources()
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "start", "totalSources": len(srcs)}))
	fl.Flush()
	for _, s := range srcs {
		items, err := searchSource(s, q)
		if err != nil {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "source_error", "source": s.Key, "sourceName": s.Name, "error": err.Error()}))
			fl.Flush()
			continue
		}
		// 转成 Selene SearchResult 形状
		res := []map[string]any{}
		for _, it := range items {
			res = append(res, map[string]any{"id": it.ID, "title": it.Title, "poster": it.Poster, "episodes": it.Episodes, "source": it.Source, "source_name": it.SourceName, "year": it.Year, "type_name": it.TypeName})
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "source_result", "source": s.Key, "sourceName": s.Name, "results": res}))
		fl.Flush()
	}
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "complete"}))
	fl.Flush()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func handleDetail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sourceKey, id := q.Get("source"), q.Get("id")
	if sourceKey == "" || id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing source/id"})
		return
	}
	var api, sname string
	err := db.QueryRow(`SELECT api, name FROM sources WHERE key=?`, sourceKey).Scan(&api, &sname)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "source not found"})
		return
	}
	d, err := cmsGet(api, "ac=videolist&ids="+url.QueryEscape(id))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	items := parseVodList(d)
	if len(items) == 0 {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	m := items[0]
	writeJSON(w, 200, map[string]any{
		"id": id, "title": sField(m, "vod_name", "title"), "poster": sField(m, "vod_pic", "pic"),
		"year": sField(m, "vod_year", "year"), "type_name": sField(m, "type_name"),
		"content": sField(m, "vod_content", "vod_blurb"), "source": sourceKey, "source_name": sname,
		"episodes": parsePlayURLs(sField(m, "vod_play_url")),
		"play_from": sField(m, "vod_play_from"),
	})
}

// ---- 目录同步配置：存 meta 表，选源为空 = 全部启用源；未选源仍可实时搜索/播放，只是不进货架 ----
type syncConfig struct {
	Pages         int      `json:"pages"`
	Sources       []string `json:"sources"`
	Auto          bool     `json:"auto"`
	IntervalHours int      `json:"interval_hours"`
	// DailyTime：每天固定时间点拉取（北京时间 HH:MM，如 "03:00"）；填了就按天跑、间隔失效，空着按间隔跑。
	DailyTime string `json:"daily_time"`
}

func validDailyTime(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func getSyncConfig() syncConfig {
	cfg := syncConfig{Pages: 10, Auto: false, IntervalHours: 24}
	var v string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k='sync_config'`).Scan(&v); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &cfg)
	}
	if cfg.Pages < 1 {
		cfg.Pages = 1
	}
	if cfg.Pages > 20 {
		cfg.Pages = 20
	}
	if cfg.IntervalHours < 1 {
		cfg.IntervalHours = 24
	}
	if !validDailyTime(cfg.DailyTime) {
		cfg.DailyTime = ""
	}
	return cfg
}

func setSyncConfig(cfg syncConfig) error {
	if cfg.Pages < 1 {
		cfg.Pages = 1
	}
	if cfg.Pages > 20 {
		cfg.Pages = 20
	}
	if cfg.IntervalHours < 1 {
		cfg.IntervalHours = 24
	}
	if !validDailyTime(cfg.DailyTime) {
		cfg.DailyTime = ""
	}
	b, _ := json.Marshal(cfg)
	_, err := db.Exec(`INSERT OR REPLACE INTO meta(k, v) VALUES('sync_config', ?)`, string(b))
	return err
}

func getLastSyncUnix() int64 {
	var v string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k='last_catalog_sync'`).Scan(&v); err == nil {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// ---- 目录同步：先 ?ac=list 拿分类，再按分类拉元数据（不存播放链接）；5 并发按源拉取、写库串行 ----
func syncCatalog() error { return syncCatalogWith(getSyncConfig()) }

type catalogItem struct {
	title, poster, year, region, genre, typ, rating, synopsis string
}

// kindOf：把源站细分类归一化为货架粗分类（2026-11-04 查库实锤：type 列约 130 种
// 细标签，网页要的“连续剧”全库仅 45 条，精确匹配下筛日本约等于 0）。
// 顺序有讲究：短剧/动漫/综艺/体育先于剧/片；题材型标签（现代都市、古装仙侠…）
// 在源站里长短剧与剧集混用，统一归剧集，短剧桶只收带明确短剧标记的（宁缺毋滥）。
func kindOf(typ, genre string) string {
	s := typ + " " + genre
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case has("短剧"):
		return "短剧"
	case has("动漫", "动画", "漫剧"):
		return "动漫"
	case has("综艺", "演唱会"):
		return "综艺"
	case has("足球", "篮球", "斯诺克", "网球", "台球", "赛事", "西甲", "英超", "法甲", "意甲", "德甲", "NBA", "CBA", "WCBA", "LPL", "体育"):
		return "体育"
	case has("剧集"):
		return "剧集"
	case has("剧"):
		return "剧集"
	case has("现代都市", "古装仙侠", "言情总裁", "穿越年代", "脑洞悬疑", "家庭篇", "闪婚离婚", "都市脑洞", "现代言情", "女恋总裁", "穿越现代", "女频恋爱", "反转爽文", "成长逆袭", "年代穿越", "悬疑烧脑"):
		return "剧集"
	case has("片", "电影", "伦理", "理论"):
		return "电影"
	default:
		return "其他"
	}
}

func syncCatalogWith(cfg syncConfig) error {
	now := time.Now().Unix()
	pages := cfg.Pages
	if pages < 1 {
		pages = 1
	}
	if pages > 20 {
		pages = 20
	}
	all := listSources()
	var srcs []Source
	if len(cfg.Sources) == 0 {
		srcs = all
	} else {
		want := map[string]bool{}
		for _, k := range cfg.Sources {
			want[k] = true
		}
		for _, s := range all {
			if want[s.Key] {
				srcs = append(srcs, s)
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, s := range srcs {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cats, err := cmsCategories(s.API)
			if err != nil {
				return
			}
			for _, c := range cats {
				var catItems []catalogItem
				for pg := 1; pg <= pages; pg++ {
					d, err := cmsGet(s.API, "ac=videolist&t="+url.QueryEscape(c.ID)+"&pg="+strconv.Itoa(pg))
					if err != nil {
						break
					}
					for _, m := range parseVodList(d) {
						title := sField(m, "vod_name", "title")
						if title == "" {
							continue
						}
						typ := sField(m, "type_name")
						if typ == "" {
							typ = c.Name
						}
						catItems = append(catItems, catalogItem{title, sField(m, "vod_pic", "pic"), sField(m, "vod_year", "year"), sField(m, "vod_area"), sField(m, "vod_class"), typ, sField(m, "vod_score"), sField(m, "vod_blurb", "vod_content")})
					}
				}
				// 每类写完立刻落库（写库串行加锁）：长同步中途也能看到库在涨，中断也不白跑
				mu.Lock()
				for _, it := range catItems {
					_, _ = db.Exec(`INSERT OR IGNORE INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
						it.title, it.poster, it.year, it.region, it.genre, it.typ, "", it.rating, it.synopsis, now, kindOf(it.typ, it.genre))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	db.Exec(`INSERT OR REPLACE INTO meta(k, v) VALUES('last_catalog_sync', ?)`, strconv.FormatInt(now, 10))
	// 同步完立即后台预取海报，货架第一次打开就不用等源站 CDN
	go prefetchCatalogPosters()
	return nil
}

type Category struct {
	ID   string
	Name string
}

func cmsCategories(api string) ([]Category, error) {
	d, err := cmsGet(api, "ac=list")
	if err != nil {
		return nil, err
	}
	arr, _ := d["class"].([]any)
	var out []Category
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			id := sField(m, "type_id", "typeid", "id")
			name := sField(m, "type_name", "typename", "name")
			if id != "" && name != "" {
				out = append(out, Category{ID: id, Name: name})
			}
		}
	}
	return out, nil
}

// ---- M3U 直播解析 ----
type Channel struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Logo  string `json:"logo"`
}

func parseM3U(rawURL string) ([]Channel, error) {
	resp, err := httpClient.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var out []Channel
	var cur Channel
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXTINF:") {
			cur = Channel{}
			if m := regexp.MustCompile(`group-title="([^"]*)"`).FindStringSubmatch(line); len(m) == 2 {
				cur.Group = m[1]
			}
			if m := regexp.MustCompile(`tvg-logo="([^"]*)"`).FindStringSubmatch(line); len(m) == 2 {
				cur.Logo = m[1]
			}
			if idx := strings.LastIndex(line, ","); idx >= 0 {
				cur.Name = strings.TrimSpace(line[idx+1:])
			}
		} else if strings.HasPrefix(line, "http") {
			cur.URL = line
			if cur.Name != "" {
				out = append(out, cur)
			}
			cur = Channel{}
		}
	}
	return out, nil
}

// ---- 服务端 m3u8 代理 + 去广告 ----
// GET /api/proxy/m3u8?url=<m3u8地址> ：取回、重写相对地址为经本代理的绝对地址、剔广告分片
// GET /api/proxy/segment?url=<分片地址> ：透传分片（供重写后的地址回指）
var adKeywords = []string{"sponsor", "/ad/", "/ads/", "advert", "advertisement", "/adjump", "redtraffic"}

func filterAdsFromM3U8(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.Contains(line, "#EXT-X-DISCONTINUITY") {
			continue
		}
		if strings.Contains(line, "#AD") {
			continue
		}
		if strings.Contains(line, "#EXTINF:") && i+1 < len(lines) {
			next := strings.ToLower(lines[i+1])
			hit := false
			for _, kw := range adKeywords {
				if strings.Contains(next, kw) {
					hit = true
					break
				}
			}
			if hit {
				i++
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func rewriteM3U8(content, base string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		var abs string
		if strings.HasPrefix(trim, "http") {
			abs = trim
		} else {
			if b, err := url.Parse(base); err == nil {
				if u, err := b.Parse(trim); err == nil {
					abs = u.String()
				}
			}
		}
		if abs == "" {
			continue
		}
		if strings.HasSuffix(abs, ".m3u8") {
			lines[i] = "/api/proxy/m3u8?url=" + url.QueryEscape(abs)
		} else {
			lines[i] = "/api/proxy/segment?url=" + url.QueryEscape(abs)
		}
	}
	return strings.Join(lines, "\n")
}

func handleProxyM3U8(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	// VOD 播放列表（含 #EXT-X-ENDLIST）缓存 5 分钟；直播不缓存，永远实时取
	if r.URL.Query().Get("fresh") != "1" {
		if v, ok := cacheGet("m3u8:" + raw); ok {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("X-Cache", "hit")
			_, _ = w.Write([]byte(v))
			return
		}
	}
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "mpegurl") || strings.Contains(raw, ".m3u8") {
		out := rewriteM3U8(filterAdsFromM3U8(string(body)), raw)
		if strings.Contains(out, "#EXT-X-ENDLIST") {
			cacheSet("m3u8:"+raw, out, 5*time.Minute)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Cache", "miss")
		_, _ = w.Write([]byte(out))
		return
	}
	// 非 m3u8 直接透传
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(body)
}

// 分片磁盘缓存：给低配 Chromebook 准备的——第一次从源站拿、落盘，
// 之后同一分片（倒带、重播、多设备同看）直接走局域网，不用再等源站 CDN。
// 总量超 2GB 时删最旧的，直到低于 1.5GB。
func segmentCachePath(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return filepath.Join(getenv("HOME_MEDIA_DATA", "/opt/home-media/data"), "segment-cache", hex.EncodeToString(sum[:])+".ts")
}

func pruneSegmentCache() {
	dir := filepath.Join(getenv("HOME_MEDIA_DATA", "/opt/home-media/data"), "segment-cache")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var total int64
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fi
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
			files = append(files, fi{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		}
	}
	if total < 2<<30 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total < 1536<<20 {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}

func handleProxySegment(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	cachePath := segmentCachePath(raw)
	if b, err := os.ReadFile(cachePath); err == nil {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Cache", "hit")
		_, _ = w.Write(b)
		return
	}
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	_ = os.WriteFile(cachePath, body, 0o644)
	go pruneSegmentCache()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Cache", "miss")
	_, _ = w.Write(body)
}
