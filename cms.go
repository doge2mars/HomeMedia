package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Poster     string   `json:"poster"`
	Year       string   `json:"year"`
	TypeName   string   `json:"type_name"`
	Source     string   `json:"source"`
	SourceName string   `json:"source_name"`
	Episodes   []string `json:"episodes,omitempty"`
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
	var best []string
	bestHasM3U8 := false
	for _, g := range groups {
		parts := strings.Split(g, "#")
		var eps []string
		hasM3U8 := false
		for _, p := range parts {
			seg := strings.Split(p, "$")
			var u string
			if len(seg) >= 2 {
				u = seg[len(seg)-1]
			} else if strings.Contains(p, "http") {
				u = p
			}
			if u != "" {
				eps = append(eps, u)
				if strings.Contains(strings.ToLower(u), ".m3u8") {
					hasM3U8 = true
				}
			}
		}
		if len(eps) == 0 {
			continue
		}
		if len(best) == 0 || (hasM3U8 && !bestHasM3U8) || (hasM3U8 == bestHasM3U8 && len(eps) > len(best)) {
			best = eps
			bestHasM3U8 = hasM3U8
		}
	}
	return best
}

func searchSource(s Source, q string) ([]SearchItem, error) {
	// #2 负缓存：该源对该词刚失败过（10 分钟内）→ 直接返回空，不再发请求；
	// 命中不记健康（没发请求就没有失败）。
	if negCacheHit(s.Key, q) {
		return []SearchItem{}, nil
	}
	start := time.Now()
	d, err := cmsGet(s.API, "ac=videolist&wd="+url.QueryEscape(q))
	if err != nil {
		// #1 健康记分 + #2 负缓存：失败才记，空结果（err==nil 但 list 为空）不是失败
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
		id := sField(m, "vod_id", "id")
		eps := parsePlayURLs(sField(m, "vod_play_url"))
		out = append(out, SearchItem{
			ID: id, Title: sField(m, "vod_name", "title"), Poster: sField(m, "vod_pic", "pic"),
			Year: sField(m, "vod_year", "year"), TypeName: typ,
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
	unlocked := isRestrictedUnlocked(r)
	cacheKey := "search:" + q
	if unlocked {
		cacheKey = "search:unlocked:" + q
	}
	// 服务端搜索缓存 10 分钟：同一词不再实时拉各源，换设备也命中；?fresh=1 强制刷新
	if r.URL.Query().Get("fresh") != "1" {
		if v, ok := cacheGet(cacheKey); ok {
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
		for _, it := range items {
			if !unlocked && isRestrictedContent(it.TypeName, "", kindOf(it.TypeName, "")) {
				continue
			}
			all = append(all, it)
		}
		if len(all) > 100 {
			all = all[:100]
			break
		}
	}
	data, _ := json.Marshal(map[string]any{"results": all, "count": len(all)})
	cacheSet(cacheKey, string(data), 10*time.Minute)
	w.Header().Set("X-Cache", "miss")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func handleSearchWS(w http.ResponseWriter, r *http.Request) {
	// 电视端服务端聚合搜索（SSE）：type=start/source_result/source_error/complete
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
	// 并发搜各源（并发 8，单源 10 秒 ctx 超时）：原逐源顺序会把慢源耗时全串起来——
	// 7 个 15 秒超时的源就能让整条 SSE 拖 3 分钟，详情页一直载入；并发后总耗时≈最慢源，
	// 且结果随到随推，客户端不用等全部。事件形状不变，客户端无需改动。
	type wsRes struct {
		key, name string
		items     []SearchItem
		err       error
	}
	ch := make(chan wsRes, len(srcs))
	sem := make(chan struct{}, 8)
	for _, s := range srcs {
		s := s
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			items, err := searchSourceCtx(ctx, s, q)
			ch <- wsRes{key: s.Key, name: s.Name, items: items, err: err}
		}()
	}
	for range srcs {
		r := <-ch
		if r.err != nil {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "source_error", "source": r.key, "sourceName": r.name, "error": r.err.Error()}))
			fl.Flush()
			continue
		}
		// 转成 电视端 SearchResult 形状
		res := []map[string]any{}
		for _, it := range r.items {
			res = append(res, map[string]any{"id": it.ID, "title": it.Title, "poster": it.Poster, "episodes": it.Episodes, "source": it.Source, "source_name": it.SourceName, "year": it.Year, "type_name": it.TypeName})
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "source_result", "source": r.key, "sourceName": r.name, "results": res}))
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
		"episodes":  parsePlayURLs(sField(m, "vod_play_url")),
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
	if v, ok := metaGet("sync_config"); ok {
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
	return metaSet("sync_config", string(b))
}

func getLastSyncUnix() int64 {
	if v, ok := metaGet("last_catalog_sync"); ok {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// ---- 目录同步：先 ?ac=list 拿分类，再按分类拉元数据（不存播放链接）；5 并发按源拉取、写库串行 ----
func syncCatalog() error { return syncCatalogWith(getSyncConfig()) }

type catalogItem struct {
	title, poster, year, region, genre, typ, rating, synopsis string
	hitsWeek                                                  int // #13 源站周点击量（vod_hits_week）
}

// parseHits：CMS 的 vod_hits_* 可能是数字或数字字符串，容错转 int。
func parseHits(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

func sanitizeYear(y string) string {
	y = strings.TrimSpace(y)
	if len(y) == 4 {
		if n, err := strconv.Atoi(y); err == nil && n >= 1900 && n <= 2099 {
			return y
		}
	}
	if m := doubanYearRe.FindStringSubmatch(y); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 1900 && n <= 2099 {
			return m[1]
		}
	}
	return ""
}

func sanitizeRating(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return ""
	}
	f, err := strconv.ParseFloat(r, 64)
	if err != nil || f < 0 || f > 10 {
		return ""
	}
	return r
}

// upsertCatalogSQL：目录入库。冲突时只刷新 hits_week（取新旧最大值，
// 防不带点击量字段的源把值清零），其余字段与 updated_at 保持原样。
const upsertCatalogSQL = `INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind, hits_week) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(title, year, type) DO UPDATE SET hits_week=MAX(hits_week, excluded.hits_week)`

// kindOf：把源站细分类归一化为货架粗分类（2026-11-04 查库实锤：type 列约 130 种
// 细标签，网页要的“连续剧”全库仅 45 条，精确匹配下筛日本约等于 0）。
// 顺序有讲究：短剧/动漫/综艺/体育先于剧/片；题材型标签（现代都市、古装仙侠…）
// 在源站里长短剧与剧集混用，统一归剧集，短剧桶只收带明确短剧标记的（宁缺毋滥）。
// sportsKeywords：体育类关键词（与 kindOf 的体育分支同款）。
// 2026-11-06 用户决定体育分类下线：同步跳过此类条目，目录接口不再返回。
var sportsKeywords = []string{"足球", "篮球", "斯诺克", "网球", "台球", "赛事", "西甲", "英超", "法甲", "意甲", "德甲", "NBA", "CBA", "WCBA", "LPL", "体育"}

// isSportsKind：源站细分类/题材是否体育内容。
func isSportsKind(typ, genre string) bool {
	s := typ + " " + genre
	for _, sub := range sportsKeywords {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// isAdultContent：源站细分类/题材是否成人内容（2026-11-06 清理，11-06 午扩展）。
// 与库里清理口径一致：type 含伦理/三级/理论/里番/擦边/写真/两性，或 type 恰为福利，或 genre 含成人/情色。
// 注意：genre 里的年份数字（如 2018）不是成人标记，不要按数字判。
func isAdultContent(typ, genre string) bool {
	if strings.Contains(typ, "伦理") || strings.Contains(typ, "三级") || strings.Contains(typ, "理论") || strings.Contains(typ, "里番") || strings.Contains(typ, "擦边") || strings.Contains(typ, "写真") || strings.Contains(typ, "两性") || typ == "福利" {
		return true
	}
	if strings.Contains(genre, "成人") || strings.Contains(genre, "情色") {
		return true
	}
	return false
}

var seriesTitleRe = regexp.MustCompile(`第[一二三四五六七八九十百0-9]+[季集]|全[0-9]+集`)

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
	case has("短剧", "爽剧", "爽文"):
		return "短剧"
	case has("动漫", "动画", "漫剧"):
		return "动漫"
	case has("综艺", "演唱会", "晚会"):
		return "综艺"
	case isSportsKind(typ, genre):
		return "体育"
	// 竖屏微短剧专属题材标签（源站常直接拿这些当 type_name，或 genre 里带 "短-古装仙侠" 等）：
	// 必须在“剧”之前拦截，否则 4300+ 部高点击量微短剧会把正经剧集彻底淹没。
	case has("现代都市", "古装仙侠", "言情总裁", "穿越年代", "脑洞悬疑", "闪婚离婚", "都市脑洞", "现代言情", "女恋总裁", "穿越现代", "女频恋爱", "成长逆袭", "年代穿越", "悬疑烧脑", "重生民国", "短-", "霸总", "女频", "男频"):
		return "短剧"
	// 电影片必须先于“剧”判断：喜剧片/剧情片/爱情片的 genre 里常带“剧情/喜剧”，
	// 且部分源把家庭电影分类取名为“家庭篇”（篇幅的篇），也归入电影。
	case strings.HasSuffix(typ, "片") || strings.Contains(typ, "电影") || typ == "家庭篇" || typ == "伦理" || typ == "理论" || typ == "影视解说":
		return "电影"
	// 剧集只看 type 里的“剧”（电视剧/连续剧/国产剧/港台剧/日韩剧/欧美剧…），
	// 不再看 genre——genre 里的“剧情/喜剧”是电影体裁，不是剧集。
	case typ == "剧集" || strings.Contains(typ, "剧"):
		return "剧集"
	default:
		return "其他"
	}
}

// refineTypeWithPlayMeta：同步拉取 ?ac=videolist 时结合 vod_remarks / vod_state 与 vod_play_url 集数做二次校验，
// 纠正源站自身把单集电影塞进剧集分类、或把多集连续剧塞进电影分类的问题。
func refineTypeWithPlayMeta(title, typ, genre, remarks, playURL string) string {
	k := kindOf(typ, genre)
	if k != "剧集" && k != "电影" {
		return typ
	}
	eps := parsePlayURLs(playURL)
	firstLabel := ""
	if playURL != "" {
		g0 := strings.Split(playURL, "$$$")[0]
		p0 := strings.Split(g0, "#")[0]
		if seg := strings.Split(p0, "$"); len(seg) >= 2 {
			firstLabel = seg[0]
		}
	}
	metaUpper := strings.ToUpper(remarks + " " + firstLabel)
	hasSeriesMarker := seriesTitleRe.MatchString(title) ||
		strings.Contains(metaUpper, "集") ||
		strings.Contains(metaUpper, "季") ||
		strings.Contains(metaUpper, "期") ||
		strings.Contains(metaUpper, "更") ||
		strings.Contains(metaUpper, "完结") ||
		strings.Contains(metaUpper, "连载")

	if k == "剧集" && len(eps) == 1 && !hasSeriesMarker {
		for _, m := range []string{"HD", "正片", "BD", "TC", "TS", "1080", "720", "4K", "超清", "蓝光", "中字", "国语", "粤语", "抢先"} {
			if strings.Contains(metaUpper, m) {
				return "剧情片"
			}
		}
	}
	if k == "电影" && typ != "预告片" && typ != "短片" && typ != "电影解说" && typ != "影视解说" {
		if seriesTitleRe.MatchString(title) || (len(eps) >= 3 && hasSeriesMarker) {
			return "连续剧"
		}
	}
	return typ
}

// backfillCatalogKinds：用当前 kindOf + isRestrictedContent 重算全库 kind 与 restricted，
// 修正历史错归类（如家庭篇电影、4300+部微短剧误入剧集，以及归入“电影”的福利片未标受限）。只更新不一致的行，幂等，启动时跑一次。
func backfillCatalogKinds() int {
	rows, err := db.Query(`SELECT title, year, type, genre, COALESCE(kind,''), COALESCE(restricted,0) FROM catalog`)
	if err != nil {
		return 0
	}
	type row struct {
		title, year, typ, genre, kind string
		restricted                    int
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.title, &r.year, &r.typ, &r.genre, &r.kind, &r.restricted); err == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range todo {
		nk := kindOf(r.typ, r.genre)
		if nk == "电影" && r.typ != "预告片" && r.typ != "短片" && r.typ != "电影解说" && r.typ != "影视解说" && seriesTitleRe.MatchString(r.title) {
			nk = "剧集"
		}
		nr := 0
		if isRestrictedContent(r.typ, r.genre, nk) {
			nr = 1
		}
		if nk != r.kind || nr != r.restricted {
			if _, err := tx.Exec(`UPDATE catalog SET kind=?, restricted=? WHERE title=? AND year=? AND type=?`, nk, nr, r.title, r.year, r.typ); err == nil {
				n++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0
	}
	return n
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
			consecutiveCatFails := 0
			for _, c := range cats {
				var catItems []catalogItem
				for pg := 1; pg <= pages; pg++ {
					d, err := cmsGet(s.API, "ac=videolist&t="+url.QueryEscape(c.ID)+"&pg="+strconv.Itoa(pg))
					if err != nil {
						if pg == 1 {
							consecutiveCatFails++
						}
						break
					}
					if pg == 1 {
						consecutiveCatFails = 0
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
						genre := sField(m, "vod_class")
						typ = refineTypeWithPlayMeta(title, typ, genre, sField(m, "vod_remarks", "vod_state"), sField(m, "vod_play_url"))
						catItems = append(catItems, catalogItem{
							title,
							sField(m, "vod_pic", "pic"),
							sanitizeYear(sField(m, "vod_year", "year")),
							sField(m, "vod_area"),
							genre,
							typ,
							sanitizeRating(sField(m, "vod_score")),
							sField(m, "vod_blurb", "vod_content"),
							parseHits(sField(m, "vod_hits_week", "vod_hits_day", "vod_hits")),
						})
					}
				}
				if consecutiveCatFails >= 3 {
					log.Printf("sync: source %s (%s) failed 3 categories in a row, aborting source early", s.Key, s.Name)
					break
				}
				if len(catItems) == 0 {
					continue
				}
				// 每类写完以单事务批量落库（写库串行加锁）：大幅降低 SQLite 锁竞争与 fsync 次数
				mu.Lock()
				tx, txErr := db.Begin()
				if txErr == nil {
					for _, it := range catItems {
						if isSportsKind(it.typ, it.genre) {
							continue // 体育已下线：不同步入库
						}
						if isAdultContent(it.typ, it.genre) {
							continue // 成人内容不入库（2026-11-06 清理）
						}
						k := kindOf(it.typ, it.genre)
						_, _ = tx.Exec(upsertCatalogSQL,
							it.title, it.poster, it.year, it.region, it.genre, it.typ, "", it.rating, it.synopsis, now, k, it.hitsWeek)
						if isRestrictedContent(it.typ, it.genre, k) {
							_, _ = tx.Exec(`UPDATE catalog SET restricted=1 WHERE title=? AND year=? AND type=?`, it.title, it.year, it.typ)
						}
					}
					_ = tx.Commit()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	backfillCatalogKinds()
	db.Exec(`INSERT OR REPLACE INTO meta(k, v) VALUES('last_catalog_sync', ?)`, strconv.FormatInt(now, 10))
	metaInvalidate("last_catalog_sync")
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
		// #EXT-X-KEY / #EXT-X-MAP 行里的 URI 也要改写，否则播放器按代理地址解析出
		// 服务端根路径下的 404（密钥/初始化段落在服务端不存在）。密钥与 map 都是字节流，走 segment 代理。
		if strings.HasPrefix(trim, "#EXT-X-KEY:") || strings.HasPrefix(trim, "#EXT-X-MAP:") {
			if si := strings.Index(trim, `URI="`); si >= 0 {
				rest := trim[si+5:]
				if ei := strings.Index(rest, `"`); ei >= 0 {
					rawURI := rest[:ei]
					var absURI string
					if strings.HasPrefix(rawURI, "http") {
						absURI = rawURI
					} else if b, err := url.Parse(base); err == nil {
						if u, err := b.Parse(rawURI); err == nil {
							absURI = u.String()
						}
					}
					if absURI != "" {
						proxy := "/api/proxy/segment?url=" + url.QueryEscape(absURI)
						if strings.HasSuffix(absURI, ".m3u8") {
							proxy = "/api/proxy/m3u8?url=" + url.QueryEscape(absURI)
						}
						lines[i] = strings.Replace(line, `URI="`+rawURI+`"`, `URI="`+proxy+`"`, 1)
					}
				}
			}
			continue
		}
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

// 分享页解析：部分源（如电影天堂）给的不是直链 m3u8，而是一个 /share/ 落地页（HTML），
// 真链藏在页面的 const/var/let url = "...m3u8..."（单双引号都可能） 里。直接把 HTML 透传给播放器必播不了，
// 这里解析出真链返回；解析不到返回 ""。
var shareM3U8Re = regexp.MustCompile(`(?:const|var|let)\s+url\s*=\s*['"]([^'"]*\.m3u8[^'"]*)['"]`)

func extractShareM3U8(html, pageURL string) string {
	m := shareM3U8Re.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	u, err := base.Parse(m[1])
	if err != nil {
		return ""
	}
	return u.String()
}

func validateProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("bad url")
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && isPrivateHost(host) {
		return nil, fmt.Errorf("private host blocked")
	}
	return u, nil
}

func handleProxyM3U8(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	if _, err := validateProxyURL(raw); err != nil {
		if strings.Contains(err.Error(), "private") {
			writeJSON(w, 403, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		}
		return
	}
	isLive := r.URL.Query().Get("live") == "1"
	// VOD 播放列表（含 #EXT-X-ENDLIST）缓存 5 分钟；直播不缓存，永远实时取
	if !isLive && r.URL.Query().Get("fresh") != "1" {
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
	// 上游非 2xx（404/403/5xx）不要当成播放列表返回，否则播放器拿到一份“空列表”也不会触发换源
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("upstream status %d", resp.StatusCode)})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	ct := resp.Header.Get("Content-Type")
	// 分享页解析：上游返回 HTML 落地页时，从中取出真 m3u8 再拉一次；
	// 取不到真链、或重拉仍是 HTML 时直接 502（HTML 透传给播放器只会让它空耗到退出）。
	if strings.Contains(ct, "text/html") {
		real := extractShareM3U8(string(body), raw)
		if real == "" {
			writeJSON(w, 502, map[string]string{"error": "share page without m3u8"})
			return
		}
		req2, _ := http.NewRequest("GET", real, nil)
		req2.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
		resp2, err := httpClient.Do(req2)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		defer resp2.Body.Close()
		if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
			writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("upstream status %d", resp2.StatusCode)})
			return
		}
		body, _ = io.ReadAll(io.LimitReader(resp2.Body, 10<<20))
		ct = resp2.Header.Get("Content-Type")
		if strings.Contains(ct, "text/html") {
			writeJSON(w, 502, map[string]string{"error": "share page without m3u8"})
			return
		}
		raw = real
	}
	if strings.Contains(ct, "mpegurl") || strings.Contains(raw, ".m3u8") {
		out := rewriteM3U8(filterAdsFromM3U8(string(body)), raw)
		if !isLive && strings.Contains(out, "#EXT-X-ENDLIST") {
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
	w.Header().Set("Access-Control-Allow-Origin", "*")
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

// ---- 分片代理（稳定性加固）----
// 1) 只有“上游 200 + 完整读完 + 不是错误页”的分片才落盘，否则回 502，让播放器换源/重试；
//    之前 403/404 错误页、读到一半被截断的分片都会被缓存，之后同一分片永远命中坏文件。
// 2) 落盘先写临时文件再 rename，别的请求不会读到写了一半的文件。
// 3) 同一分片的并发请求合并成一次上游下载（多设备同看/播放器预加载）。
// 4) 分片整体超时放宽到 60s（共用的 httpClient 是 15s，慢 CDN 的大分片会被截断）。
// 5) 命中时刷新 mtime，使淘汰真正按“最近使用”而不是“最早写入”；淘汰任务同一时刻只跑一个。

const maxSegmentBytes = 50 << 20

var segClient = &http.Client{Timeout: 60 * time.Second}

type segCall struct {
	done chan struct{}
	body []byte
	ct   string
	err  error
}

var segInflight sync.Map // cachePath -> *segCall

// looksLikeErrorPage：上游常用 200 + HTML/JSON 返回“防盗链/限流”页面，真分片（TS/fMP4/密文）不会以这些开头。
func looksLikeErrorPage(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	head := b
	if len(head) > 64 {
		head = head[:64]
	}
	s := strings.ToLower(strings.TrimLeft(string(head), " \t\r\n\xef\xbb\xbf"))
	return strings.HasPrefix(s, "<!doctype") || strings.HasPrefix(s, "<html") || strings.HasPrefix(s, "{\"")
}

func downloadSegment(raw, cachePath string) ([]byte, string, error) {
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := segClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "text/html") {
		return nil, "", fmt.Errorf("upstream returned html, not a segment")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read segment: %w", err)
	}
	if len(body) > maxSegmentBytes {
		return nil, "", fmt.Errorf("segment larger than %d bytes", maxSegmentBytes)
	}
	if resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength {
		return nil, "", fmt.Errorf("truncated segment: got %d of %d bytes", len(body), resp.ContentLength)
	}
	if looksLikeErrorPage(body) {
		return nil, "", fmt.Errorf("upstream returned an error page, not a segment")
	}
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if tmp, err := os.CreateTemp(dir, "seg-*.tmp"); err == nil {
			_, werr := tmp.Write(body)
			cerr := tmp.Close()
			if werr == nil && cerr == nil {
				_ = os.Chmod(tmp.Name(), 0o644)
				if os.Rename(tmp.Name(), cachePath) != nil {
					_ = os.Remove(tmp.Name())
				}
			} else {
				_ = os.Remove(tmp.Name())
			}
		}
	}
	return body, ct, nil
}

// fetchSegment：同一 cachePath 同时只有一个下载，其余请求等它的结果。
func fetchSegment(raw, cachePath string) ([]byte, string, error) {
	c := &segCall{done: make(chan struct{}), err: fmt.Errorf("segment fetch aborted")}
	if v, loaded := segInflight.LoadOrStore(cachePath, c); loaded {
		other := v.(*segCall)
		<-other.done
		return other.body, other.ct, other.err
	}
	defer func() {
		close(c.done)
		segInflight.Delete(cachePath)
	}()
	c.body, c.ct, c.err = downloadSegment(raw, cachePath)
	return c.body, c.ct, c.err
}

var pruneRunning int32

func pruneSegmentCacheAsync() {
	if !atomic.CompareAndSwapInt32(&pruneRunning, 0, 1) {
		return
	}
	go func() {
		defer atomic.StoreInt32(&pruneRunning, 0)
		pruneSegmentCache()
	}()
}

func handleProxySegment(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	if _, err := validateProxyURL(raw); err != nil {
		if strings.Contains(err.Error(), "private") {
			writeJSON(w, 403, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		}
		return
	}
	cachePath := segmentCachePath(raw)
	if b, err := os.ReadFile(cachePath); err == nil {
		if looksLikeErrorPage(b) {
			// 旧版本留下的坏缓存：删掉，当作未命中重新拉
			_ = os.Remove(cachePath)
		} else {
			now := time.Now()
			_ = os.Chtimes(cachePath, now, now)
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("X-Cache", "hit")
			_, _ = w.Write(b)
			return
		}
	}
	body, ct, err := fetchSegment(raw, cachePath)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	pruneSegmentCacheAsync()
	if ct == "" {
		ct = "video/mp2t"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Cache", "miss")
	_, _ = w.Write(body)
}
