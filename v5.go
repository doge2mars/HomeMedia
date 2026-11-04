package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---- 即将上映：慢看（manmankan，LunaTV 同款）+ TMDB 可选 ----

type upcomingItem struct {
	Title       string `json:"title"`
	Cover       string `json:"cover"`
	Type        string `json:"type"` // 电影片 / 连续剧
	ReleaseDate string `json:"release_date"`
	Genre       string `json:"genre"`
	Region      string `json:"region"`
	Director    string `json:"director"`
	Actors      string `json:"actors"`
	Source      string `json:"source"` // manmankan / tmdb
}

type upcomingConfig struct {
	Source  string `json:"source"` // manmankan | tmdb | off
	TMDBKey string `json:"tmdb_key"`
}

func getUpcomingConfig() upcomingConfig {
	cfg := upcomingConfig{Source: "manmankan"}
	var v string
	if err := db.QueryRow(`SELECT v FROM meta WHERE k='upcoming_config'`).Scan(&v); err == nil {
		json.Unmarshal([]byte(v), &cfg)
	}
	if cfg.Source == "" {
		cfg.Source = "manmankan"
	}
	return cfg
}

func setUpcomingConfig(cfg upcomingConfig) {
	b, _ := json.Marshal(cfg)
	db.Exec(`INSERT INTO meta(k, v) VALUES('upcoming_config', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b))
}

var upcomingRefreshing int32

func shanghaiToday() (today, start, end string) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Now().In(loc)
	return now.Format("2006-01-02"), now.AddDate(0, 0, -7).Format("2006-01-02"), now.AddDate(0, 0, 90).Format("2006-01-02")
}

var (
	reBlockTitle  = regexp.MustCompile(`<div class="dd-d1"><a[^>]*>([^<]+)</a>`)
	reDirector    = regexp.MustCompile(`<div>导演：([^<]*)</div>`)
	reRegionLink  = regexp.MustCompile(`<div>地区：<a[^>]*>([^<]*)</a></div>`)
	reRegionPlain = regexp.MustCompile(`<div>地区：([^<]*)</div>`)
	reGenreDiv    = regexp.MustCompile(`<div>类型：(.*?)</div>`)
	reGenreLink   = regexp.MustCompile(`<a[^>]*>([^<]+)</a>`)
	reRelDate     = regexp.MustCompile(`上映时间：(\d{4})/(\d{2})/(\d{2})`)
	reActorsDiv   = regexp.MustCompile(`<div class="dd-d2">主演：(.*?)</div>`)
	reImgSrc      = regexp.MustCompile(`<img src="([^"]+)"`)
	// 慢看是懒加载：真海报在 data-original，src 只是 loadimg.gif 占位图。
	// LunaTV 解析器也是优先 data-original、 fallback src；只取 src 就会全是占位图。
	reImgOriginal = regexp.MustCompile(`data-original="([^"]+)"`)
	reTag         = regexp.MustCompile(`<[^>]+>`)
)

func stripTags(s string) string { return strings.TrimSpace(reTag.ReplaceAllString(s, "")) }

// 抓慢看时间表页：电影 https://g.manmankan.com/dy2013/dianying/shijianbiao/
// 电视剧 https://g.manmankan.com/dy2013/dianshiju/shijianbiao/，结构与 LunaTV 解析器一致。
func scrapeManmankan(kind, url, typeName string) []upcomingItem {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36")
	req.Header.Set("Referer", "https://g.manmankan.com/dy2013/")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil
	}
	today, start, end := shanghaiToday()
	_ = today
	blocks := regexp.MustCompile(`<dl class="(?:twlist-block|dis_none)">`).Split(string(body), -1)
	out := []upcomingItem{}
	for _, b := range blocks[1:] {
		m := reBlockTitle.FindStringSubmatch(b)
		if m == nil {
			continue
		}
		d := reRelDate.FindStringSubmatch(b)
		if d == nil {
			continue
		}
		date := d[1] + "-" + d[2] + "-" + d[3]
		if date < start || date > end {
			continue
		}
		it := upcomingItem{Title: strings.TrimSpace(m[1]), Type: typeName, ReleaseDate: date, Source: "manmankan"}
		if dm := reDirector.FindStringSubmatch(b); dm != nil {
			it.Director = strings.TrimSpace(dm[1])
		}
		if rm := reRegionLink.FindStringSubmatch(b); rm != nil {
			it.Region = strings.TrimSpace(rm[1])
		} else if rm := reRegionPlain.FindStringSubmatch(b); rm != nil {
			it.Region = strings.TrimSpace(rm[1])
		}
		if gm := reGenreDiv.FindStringSubmatch(b); gm != nil {
			parts := []string{}
			for _, g := range reGenreLink.FindAllStringSubmatch(gm[1], -1) {
				parts = append(parts, strings.TrimSpace(g[1]))
			}
			if len(parts) == 0 {
				it.Genre = stripTags(gm[1])
			} else {
				it.Genre = strings.Join(parts, "/")
			}
		}
		if am := reActorsDiv.FindStringSubmatch(b); am != nil {
			it.Actors = stripTags(am[1])
		}
		cover := ""
		if im := reImgOriginal.FindStringSubmatch(b); im != nil {
			cover = strings.TrimSpace(im[1])
		} else if im := reImgSrc.FindStringSubmatch(b); im != nil {
			cover = strings.TrimSpace(im[1])
		}
		if strings.HasPrefix(cover, "//") {
			cover = "https:" + cover
		}
		it.Cover = cover
		out = append(out, it)
	}
	return out
}

func scrapeTMDBUpcoming(key string) []upcomingItem {
	today, _, end := shanghaiToday()
	out := []upcomingItem{}
	for pg := 1; pg <= 3; pg++ {
		u := fmt.Sprintf("https://api.themoviedb.org/3/movie/upcoming?api_key=%s&language=zh-CN&region=CN&page=%d", key, pg)
		resp, err := httpClient.Get(u)
		if err != nil {
			break
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			break
		}
		var data struct {
			Results []struct {
				Title       string `json:"title"`
				PosterPath  string `json:"poster_path"`
				ReleaseDate string `json:"release_date"`
				Overview    string `json:"overview"`
			} `json:"results"`
		}
		if json.Unmarshal(body, &data) != nil {
			break
		}
		for _, r := range data.Results {
			if r.ReleaseDate == "" || r.ReleaseDate > end || r.ReleaseDate < today {
				continue // TMDB 只取未来上映，不含已上映
			}
			cover := ""
			if r.PosterPath != "" {
				cover = "https://image.tmdb.org/t/p/w500" + r.PosterPath
			}
			out = append(out, upcomingItem{Title: r.Title, Cover: cover, Type: "电影片", ReleaseDate: r.ReleaseDate, Source: "tmdb"})
		}
	}
	return out
}

func refreshUpcoming() error {
	cfg := getUpcomingConfig()
	if cfg.Source == "off" {
		db.Exec(`DELETE FROM upcoming`)
		return nil
	}
	var items []upcomingItem
	if cfg.Source == "tmdb" {
		if cfg.TMDBKey == "" {
			return fmt.Errorf("TMDB 未填 key，请在设置里填 TMDB API key")
		}
		items = scrapeTMDBUpcoming(cfg.TMDBKey)
	} else {
		items = append(items, scrapeManmankan("movie", "https://g.manmankan.com/dy2013/dianying/shijianbiao/", "电影片")...)
		items = append(items, scrapeManmankan("tv", "https://g.manmankan.com/dy2013/dianshiju/shijianbiao/", "连续剧")...)
	}
	// 按标题去重，保留最早上映日期
	byTitle := map[string]upcomingItem{}
	for _, it := range items {
		if old, ok := byTitle[it.Title]; !ok || it.ReleaseDate < old.ReleaseDate {
			byTitle[it.Title] = it
		}
	}
	items = items[:0]
	for _, it := range byTitle {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ReleaseDate != items[j].ReleaseDate {
			return items[i].ReleaseDate < items[j].ReleaseDate
		}
		return items[i].Title < items[j].Title
	})
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM upcoming`); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`INSERT INTO upcoming(title, cover, type, release_date, genre, region, director, actors, source, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			it.Title, it.Cover, it.Type, it.ReleaseDate, it.Genre, it.Region, it.Director, it.Actors, it.Source, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(k, v) VALUES('last_upcoming_sync', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return err
	}
	return tx.Commit()
}

func triggerUpcomingRefresh() bool {
	if !atomic.CompareAndSwapInt32(&upcomingRefreshing, 0, 1) {
		return false
	}
	go func() {
		defer atomic.StoreInt32(&upcomingRefreshing, 0)
		refreshUpcoming()
	}()
	return true
}

func handleUpcoming(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		// 仅 owner 手动刷新
		user := r.Header.Get("X-User")
		role := ""
		db.QueryRow(`SELECT role FROM users WHERE username=?`, user).Scan(&role)
		if role != "owner" {
			writeJSON(w, 403, map[string]any{"error": "owner only"})
			return
		}
		started := triggerUpcomingRefresh()
		writeJSON(w, 202, map[string]any{"started": started, "running": atomic.LoadInt32(&upcomingRefreshing) == 1})
		return
	}
	cfg := getUpcomingConfig()
	rows, err := db.Query(`SELECT title, cover, type, release_date, genre, region, director, actors, source FROM upcoming ORDER BY release_date, title`)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	items := []upcomingItem{}
	for rows.Next() {
		var it upcomingItem
		rows.Scan(&it.Title, &it.Cover, &it.Type, &it.ReleaseDate, &it.Genre, &it.Region, &it.Director, &it.Actors, &it.Source)
		items = append(items, it)
	}
	var lastSync int64
	db.QueryRow(`SELECT CAST(v AS INTEGER) FROM meta WHERE k='last_upcoming_sync'`).Scan(&lastSync)
	// 库空或超过 24 小时未同步，后台自动刷一次
	if cfg.Source != "off" && (len(items) == 0 || time.Now().Unix()-lastSync > 24*3600) {
		triggerUpcomingRefresh()
	}
	writeJSON(w, 200, map[string]any{"items": items, "source": cfg.Source, "last_sync": lastSync, "refreshing": atomic.LoadInt32(&upcomingRefreshing) == 1})
}

func handleUpcomingConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		user := r.Header.Get("X-User")
		role := ""
		db.QueryRow(`SELECT role FROM users WHERE username=?`, user).Scan(&role)
		if role != "owner" {
			writeJSON(w, 403, map[string]any{"error": "owner only"})
			return
		}
		var cfg upcomingConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		if cfg.Source != "manmankan" && cfg.Source != "tmdb" && cfg.Source != "off" {
			writeJSON(w, 400, map[string]any{"error": "source must be manmankan|tmdb|off"})
			return
		}
		setUpcomingConfig(cfg)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	cfg := getUpcomingConfig()
	writeJSON(w, 200, map[string]any{"source": cfg.Source, "has_tmdb_key": cfg.TMDBKey != ""})
}

// ---- 想看：与收藏平行的独立列表，带上映日期用于提醒 ----

type wantItem struct {
	Title       string `json:"title"`
	Cover       string `json:"cover"`
	Year        string `json:"year"`
	SourceName  string `json:"source_name"`
	ReleaseDate string `json:"release_date"`
	SaveTime    int64  `json:"save_time"`
}

func handleWants(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-User")
	if user == "" {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	switch r.Method {
	case "GET":
		rows, err := db.Query(`SELECT title, cover, year, source_name, release_date, save_time FROM wants WHERE username=? ORDER BY CASE WHEN release_date='' THEN 1 ELSE 0 END, release_date, save_time DESC`, user)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		defer rows.Close()
		items := []wantItem{}
		for rows.Next() {
			var it wantItem
			rows.Scan(&it.Title, &it.Cover, &it.Year, &it.SourceName, &it.ReleaseDate, &it.SaveTime)
			items = append(items, it)
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case "POST":
		var it wantItem
		if err := json.NewDecoder(r.Body).Decode(&it); err != nil || it.Title == "" {
			writeJSON(w, 400, map[string]any{"error": "title required"})
			return
		}
		db.Exec(`INSERT INTO wants(username, title, cover, year, source_name, release_date, save_time) VALUES(?,?,?,?,?,?,?) ON CONFLICT(username, title) DO UPDATE SET cover=excluded.cover, year=excluded.year, source_name=excluded.source_name, release_date=excluded.release_date, save_time=excluded.save_time`,
			user, it.Title, it.Cover, it.Year, it.SourceName, it.ReleaseDate, time.Now().Unix())
		writeJSON(w, 200, map[string]any{"ok": true})
	case "DELETE":
		title := r.URL.Query().Get("title")
		if title == "" {
			writeJSON(w, 400, map[string]any{"error": "title required"})
			return
		}
		db.Exec(`DELETE FROM wants WHERE username=? AND title=?`, user, title)
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}
