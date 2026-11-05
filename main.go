// HomeMedia v0.1 — 自研家庭影音原型：Go + SQLite 单进程
// 设计约束（2026-11-04 定案）：
//   - 目录库只囤元数据，不囤播放链接；点开才即时搜源拿新鲜集数/m3u8
//   - 多用户独立播放记录/收藏；电视端契约优先（见 README）
//   - 去广告在服务端 m3u8 代理层做一次，电视 ExoPlayer 也受益
//   - 网页端同样是完整观看面，不只是管理后台
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB
var secret []byte

func main() {
	port := getenv("HOME_MEDIA_PORT", "8080")
	dataDir := getenv("HOME_MEDIA_DATA", "/opt/home-media/data")
	_ = os.MkdirAll(dataDir, 0o755)
	secret = []byte(getenv("HOME_MEDIA_SECRET", "home-media-dev-secret-change-me"))
	var err error
	// 不用 cache=shared：共享缓存模式下并发读写会直接返回 "database table is locked"，busy_timeout 管不了；
	// WAL + _busy_timeout 让写冲突排队等待（目录同步批量写时，前台写进度/收藏不再报锁）。
	db, err = sql.Open("sqlite3", filepath.Join(dataDir, "home-media.db")+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		log.Fatal(err)
	}
	if err := initDB(); err != nil {
		log.Fatal(err)
	}
	// 归类口径改过后，把历史错归类的 kind 一次性纠正（如喜剧片/剧情片曾被误判为剧集）
	if n := backfillCatalogKinds(); n > 0 {
		log.Printf("catalog kind backfill: %d rows reclassified", n)
	}
	seed()
	go syncSchedulerLoop()
	go healthRetestLoop() // #1：源健康度复测（启动 1 分钟后首次，之后每 6 小时）
	// 启动时也预取一遍当季海报：磁盘缓存还在，命中就是瞬回；服务常重启，重启后首页不再冷启动
	go prefetchCatalogPosters()

	mux := http.NewServeMux()
	exe, err := os.Executable()
	var binDir string
	if err == nil {
		binDir = filepath.Dir(exe)
	} else {
		binDir = filepath.Dir(os.Args[0])
	}
	webDir := filepath.Join(binDir, "web")
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		if _, err := os.Stat("web/index.html"); err == nil {
			webDir = "web"
		}
	}
	fs := http.FileServer(http.Dir(webDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			indexPath := filepath.Join(webDir, "index.html")
			data, err := os.ReadFile(indexPath)
			if err != nil {
				http.Error(w, "Cannot read index.html: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		fs.ServeHTTP(w, r)
	})
	// FileServer root fallback: also serve embedded web dir if binary dir has no web/
	mux.HandleFunc("/api/ping", handlePing)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/search/resources", auth(handleSearchResources))
	mux.HandleFunc("/api/search", auth(handleSearch))
	mux.HandleFunc("/api/search/ws", auth(handleSearchWS))
	mux.HandleFunc("/api/detail", auth(handleDetail))
	mux.HandleFunc("/api/catalog", auth(handleCatalog))
	mux.HandleFunc("/api/catalog/related", auth(handleCatalogRelated))
	mux.HandleFunc("/api/trending-household", auth(handleTrendingHousehold))
	mux.HandleFunc("/api/douban", auth(handleDouban))
	mux.HandleFunc("/api/douban-proxy", auth(handleDoubanProxyGet))
	mux.HandleFunc("/api/douban-proxy/save", auth(ownerOnly(handleDoubanProxySave)))
	mux.HandleFunc("/api/douban-proxy/test", auth(ownerOnly(handleDoubanProxyTest)))
	mux.HandleFunc("/api/catalog/sync", auth(handleCatalogSync))
	mux.HandleFunc("/api/catalog/meta", auth(handleCatalogMeta))
	mux.HandleFunc("/api/image-proxies", auth(handleImageProxiesGet))
	mux.HandleFunc("/api/image-proxies/save", auth(ownerOnly(handleImageProxiesSave)))
	mux.HandleFunc("/api/image-proxies/test", auth(ownerOnly(handleImageProxiesTest)))
	mux.HandleFunc("/api/sync-config", auth(handleSyncConfig))
	mux.HandleFunc("/api/me", auth(handleMe))
	mux.HandleFunc("/api/sources", auth(handleSourcesV2))
	mux.HandleFunc("/api/sources/subscribe", auth(ownerOnly(handleSourceSubscribe)))
	mux.HandleFunc("/api/subscriptions", auth(ownerOnly(handleSubscriptionsList)))
	mux.HandleFunc("/api/subscriptions/update", auth(ownerOnly(handleSubscriptionUpdate)))
	mux.HandleFunc("/api/subscriptions/delete", auth(ownerOnly(handleSubscriptionDelete)))
	mux.HandleFunc("/api/source/test", auth(handleSourceTest))
	mux.HandleFunc("/api/detail-aggregate", auth(handleDetailAggregate))
	mux.HandleFunc("/api/detail-aggregate-stream", auth(handleDetailAggregateStream))
	mux.HandleFunc("/api/sources/test-all", auth(ownerOnly(handleSourcesTestAll)))
	mux.HandleFunc("/api/sources/disable-failed", auth(ownerOnly(handleSourcesDisableFailed)))
	mux.HandleFunc("/api/sources/health", auth(handleSourcesHealth))
	mux.HandleFunc("/api/sources/health-retest", auth(ownerOnly(handleSourcesHealthRetest)))
	mux.HandleFunc("/api/sources/dedupe", auth(ownerOnly(handleSourcesDedupe)))
	mux.HandleFunc("/api/users", auth(ownerOnly(handleUsers)))
	mux.HandleFunc("/api/change-password", auth(handleChangePassword))
	mux.HandleFunc("/api/user-settings", auth(handleUserSettings))
	mux.HandleFunc("/api/cache/clear", auth(ownerOnly(handleCacheClear)))
	mux.HandleFunc("/api/playrecords", auth(handlePlayRecords))
	mux.HandleFunc("/api/favorites", auth(handleFavorites))
	mux.HandleFunc("/api/searchhistory", auth(handleSearchHistory))
	mux.HandleFunc("/api/live/sources", auth(handleLiveSourcesV2))
	mux.HandleFunc("/api/live/subscribe", auth(ownerOnly(handleLiveSubscribe)))
	mux.HandleFunc("/api/live/channels", auth(handleLiveChannelsV3))
	mux.HandleFunc("/api/live", auth(handleLive))
	mux.HandleFunc("/api/play/best", auth(handlePlayBest))
	mux.HandleFunc("/api/proxy/m3u8", handleProxyM3U8)
	mux.HandleFunc("/api/proxy/segment", handleProxySegment)
	mux.HandleFunc("/api/image-proxy", handleImageProxy)
	mux.HandleFunc("/api/upcoming", auth(handleUpcoming))
	mux.HandleFunc("/api/upcoming-config", auth(handleUpcomingConfig))
	mux.HandleFunc("/api/wants", auth(handleWants))

	log.Printf("HomeMedia v0.1 listening on :%s (LAN + Tailscale), data=%s", port, dataDir)
	log.Printf("default admin: admin / admin123 (change via HOME_MEDIA_ADMIN_PASS env on first run, or change-password after login)")
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func initDB() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(username TEXT PRIMARY KEY, passhash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user', created_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS sources(key TEXT PRIMARY KEY, name TEXT NOT NULL, api TEXT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS catalog(title TEXT, poster TEXT, year TEXT, region TEXT, genre TEXT, type TEXT, douban_id TEXT, rating TEXT, synopsis TEXT, updated_at INTEGER, kind TEXT, PRIMARY KEY(title, year, type))`,
		`CREATE TABLE IF NOT EXISTS playrecords(username TEXT, source TEXT, id TEXT, title TEXT, source_name TEXT, cover TEXT, year TEXT, idx INTEGER, total_episodes INTEGER, play_time INTEGER, total_time INTEGER, save_time INTEGER, search_title TEXT, PRIMARY KEY(username, source, id))`,
		`CREATE TABLE IF NOT EXISTS favorites(username TEXT, source TEXT, id TEXT, title TEXT, source_name TEXT, cover TEXT, year TEXT, total_episodes INTEGER, save_time INTEGER, origin TEXT, PRIMARY KEY(username, source, id))`,
		`CREATE TABLE IF NOT EXISTS searchhistory(username TEXT, keyword TEXT, updated_at INTEGER, PRIMARY KEY(username, keyword))`,
		`CREATE TABLE IF NOT EXISTS livesources(key TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL, ua TEXT, epg TEXT, disabled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT)`,
		`CREATE TABLE IF NOT EXISTS cache(k TEXT PRIMARY KEY, v TEXT, expires_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS user_settings(username TEXT, k TEXT, v TEXT, PRIMARY KEY(username, k))`,
		`CREATE TABLE IF NOT EXISTS subscriptions(url TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE IF NOT EXISTS upcoming(title TEXT, cover TEXT, type TEXT, release_date TEXT, genre TEXT, region TEXT, director TEXT, actors TEXT, source TEXT, updated_at INTEGER, PRIMARY KEY(title, release_date, type))`,
		`CREATE TABLE IF NOT EXISTS wants(username TEXT, title TEXT, cover TEXT, year TEXT, source_name TEXT, release_date TEXT, save_time INTEGER, PRIMARY KEY(username, title))`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	// 老库迁移：kind 列不存在时补上（已存在则报错忽略），并回填空 kind
	_, _ = db.Exec(`ALTER TABLE catalog ADD COLUMN kind TEXT`)
	// 订阅带来的源归属：sources.sub_url 记订阅地址，更新订阅时只替换它带来的源
	_, _ = db.Exec(`ALTER TABLE sources ADD COLUMN sub_url TEXT`)
	rows, err := db.Query(`SELECT title, year, type, genre FROM catalog WHERE kind IS NULL OR kind=''`)
	if err == nil {
		type row struct{ title, year, typ, genre string }
		var todo []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.title, &r.year, &r.typ, &r.genre); err == nil {
				todo = append(todo, r)
			}
		}
		rows.Close()
		for _, r := range todo {
			_, _ = db.Exec(`UPDATE catalog SET kind=? WHERE title=? AND year=? AND type=?`, kindOf(r.typ, r.genre), r.title, r.year, r.typ)
		}
	}
	return nil
}

func seed() {
	adminPass := getenv("HOME_MEDIA_ADMIN_PASS", "admin123")
	h := hashPass("admin", adminPass)
	db.Exec(`INSERT OR IGNORE INTO users(username, passhash, role, created_at) VALUES('admin', ?, 'owner', ?)`, h, time.Now().Unix())
	// 演示源：苹果 CMS 标准接口。用户可在网页“源管理”里改成自己的源。
	defaults := [][3]string{
		{"ffzy", "非凡资源", "https://cj.ffzyapi.com/api.php/provide/vod/"},
		{"bfzy", "暴风资源", "https://bfzyapi.com/api.php/provide/vod/"},
	}
	for _, d := range defaults {
		db.Exec(`INSERT OR IGNORE INTO sources(key, name, api, disabled) VALUES(?, ?, ?, 0)`, d[0], d[1], d[2])
	}
}

func hashPass(user, pass string) string {
	s := sha256.Sum256([]byte(user + ":" + pass + ":home-media"))
	return hex.EncodeToString(s[:])
}

func sign(username string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(username))
	return hex.EncodeToString(m.Sum(nil))
}

type authInfo struct {
	Username string
	Role     string
}

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := checkAuth(r)
		if !ok {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		r.Header.Set("X-User", u.Username)
		r.Header.Set("X-Role", u.Role)
		next(w, r)
	}
}

func checkAuth(r *http.Request) (authInfo, bool) {
	for _, name := range []string{"user_auth", "auth"} {
		c, err := r.Cookie(name)
		if err != nil {
			continue
		}
		v, err := url.QueryUnescape(c.Value)
		if err != nil {
			v = c.Value
		}
		var d struct {
			Username  string `json:"username"`
			Role      string `json:"role"`
			Signature string `json:"signature"`
		}
		if json.Unmarshal([]byte(v), &d) != nil {
			continue
		}
		if d.Username == "" || !hmac.Equal([]byte(d.Signature), []byte(sign(d.Username))) {
			continue
		}
		// 用户必须仍存在（删用户即失效）
		var role string
		if err := db.QueryRow(`SELECT role FROM users WHERE username=?`, d.Username).Scan(&role); err != nil {
			continue
		}
		return authInfo{Username: d.Username, Role: role}, true
	}
	return authInfo{}, false
}

func setAuthCookies(w http.ResponseWriter, username, role string) {
	payload, _ := json.Marshal(map[string]any{"username": username, "role": role, "signature": sign(username), "timestamp": time.Now().UnixMilli()})
	val := url.QueryEscape(string(payload))
	for _, name := range []string{"auth", "user_auth"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: 30 * 24 * 3600})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Username == "" {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	var role, passhash string
	err := db.QueryRow(`SELECT role, passhash FROM users WHERE username=?`, b.Username).Scan(&role, &passhash)
	if err != nil || passhash != hashPass(b.Username, b.Password) {
		writeJSON(w, 401, map[string]string{"error": "bad credentials"})
		return
	}
	setAuthCookies(w, b.Username, role)
	writeJSON(w, 200, map[string]string{"username": b.Username, "role": role})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{"auth", "user_auth"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	if len(b.Username) < 3 || len(b.Username) > 20 || len(b.Password) < 6 {
		writeJSON(w, 400, map[string]string{"error": "username 3-20, password >=6"})
		return
	}
	_, err := db.Exec(`INSERT INTO users(username, passhash, role, created_at) VALUES(?, ?, 'user', ?)`, b.Username, hashPass(b.Username, b.Password), time.Now().Unix())
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "user exists"})
		return
	}
	setAuthCookies(w, b.Username, "user")
	writeJSON(w, 200, map[string]string{"username": b.Username, "role": "user"})
}

func currentUser(r *http.Request) string { return r.Header.Get("X-User") }

func handleSearchResources(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query(`SELECT key, name, api, disabled FROM sources ORDER BY key`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var k, n, a string
		var dis int
		rows.Scan(&k, &n, &a, &dis)
		out = append(out, map[string]any{"key": k, "name": n, "api": a, "disabled": dis == 1})
	}
	// 电视端期望：JSON 数组 [{key,name,api,...}]
	writeJSON(w, 200, out)
}

func handleSources(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var b struct {
			Key  string `json:"key"`
			Name string `json:"name"`
			API  string `json:"api"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" || b.API == "" {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		db.Exec(`INSERT OR REPLACE INTO sources(key, name, api, disabled) VALUES(?, ?, ?, 0)`, b.Key, b.Name, b.API)
		writeJSON(w, 200, map[string]string{"ok": "true"})
		return
	}
	handleSearchResources(w, r)
}

// ---- 播放记录：电视端契约 ----
// GET  -> map["source+id"] = {title, source_name, year, cover, index, total_episodes, play_time, total_time, save_time, search_title}
// POST -> {key, record:{...}} ；DELETE ?key=... 或无 key=清空
func handlePlayRecords(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodGet:
		rows, _ := db.Query(`SELECT source, id, title, source_name, cover, year, idx, total_episodes, play_time, total_time, save_time, search_title FROM playrecords WHERE username=? ORDER BY save_time DESC`, u)
		defer rows.Close()
		out := map[string]any{}
		for rows.Next() {
			var src, id, title, sname, cover, year, st string
			var idx, tot, pt, tt, stime int64
			rows.Scan(&src, &id, &title, &sname, &cover, &year, &idx, &tot, &pt, &tt, &stime, &st)
			out[src+"+"+id] = map[string]any{"title": title, "source_name": sname, "cover": cover, "year": year, "index": idx, "total_episodes": tot, "play_time": pt, "total_time": tt, "save_time": stime, "search_title": st}
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var b struct {
			Key    string         `json:"key"`
			Record map[string]any `json:"record"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		parts := strings.SplitN(b.Key, "+", 2)
		if len(parts) != 2 {
			writeJSON(w, 400, map[string]string{"error": "bad key"})
			return
		}
		rec := b.Record
		getS := func(k string) string { v, _ := rec[k].(string); return v }
		getI := func(k string) int64 {
			switch v := rec[k].(type) {
			case float64:
				return int64(v)
			case string:
				n, _ := strconv.ParseInt(v, 10, 64)
				return n
			}
			return 0
		}
		db.Exec(`INSERT OR REPLACE INTO playrecords(username, source, id, title, source_name, cover, year, idx, total_episodes, play_time, total_time, save_time, search_title) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			u, parts[0], parts[1], getS("title"), getS("source_name"), getS("cover"), getS("year"), getI("index"), getI("total_episodes"), getI("play_time"), getI("total_time"), getI("save_time"), getS("search_title"))
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		if k := r.URL.Query().Get("key"); k != "" {
			parts := strings.SplitN(k, "+", 2)
			if len(parts) == 2 {
				db.Exec(`DELETE FROM playrecords WHERE username=? AND source=? AND id=?`, u, parts[0], parts[1])
			}
		} else {
			db.Exec(`DELETE FROM playrecords WHERE username=?`, u)
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleFavorites(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodGet:
		rows, _ := db.Query(`SELECT source, id, title, source_name, cover, year, total_episodes, save_time, origin FROM favorites WHERE username=? ORDER BY save_time DESC`, u)
		defer rows.Close()
		out := map[string]any{}
		for rows.Next() {
			var src, id, title, sname, cover, year, origin string
			var tot, stime int64
			rows.Scan(&src, &id, &title, &sname, &cover, &year, &tot, &stime, &origin)
			out[src+"+"+id] = map[string]any{"title": title, "source_name": sname, "cover": cover, "year": year, "total_episodes": tot, "save_time": stime, "origin": origin}
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var b struct {
			Key      string         `json:"key"`
			Favorite map[string]any `json:"favorite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		parts := strings.SplitN(b.Key, "+", 2)
		if len(parts) != 2 {
			writeJSON(w, 400, map[string]string{"error": "bad key"})
			return
		}
		f := b.Favorite
		getS := func(k string) string { v, _ := f[k].(string); return v }
		var tot int64
		if v, ok := f["total_episodes"].(float64); ok {
			tot = int64(v)
		}
		st := time.Now().UnixMilli()
		if v, ok := f["save_time"].(float64); ok && v > 0 {
			st = int64(v)
		}
		db.Exec(`INSERT OR REPLACE INTO favorites(username, source, id, title, source_name, cover, year, total_episodes, save_time, origin) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			u, parts[0], parts[1], getS("title"), getS("source_name"), getS("cover"), getS("year"), tot, st, getS("origin"))
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		if k := r.URL.Query().Get("key"); k != "" {
			parts := strings.SplitN(k, "+", 2)
			if len(parts) == 2 {
				db.Exec(`DELETE FROM favorites WHERE username=? AND source=? AND id=?`, u, parts[0], parts[1])
			}
		} else {
			db.Exec(`DELETE FROM favorites WHERE username=?`, u)
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleSearchHistory(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodGet:
		rows, _ := db.Query(`SELECT keyword FROM searchhistory WHERE username=? ORDER BY updated_at DESC LIMIT 50`, u)
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var k string
			rows.Scan(&k)
			out = append(out, k)
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var b struct {
			Keyword string `json:"keyword"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if b.Keyword != "" {
			db.Exec(`INSERT OR REPLACE INTO searchhistory(username, keyword, updated_at) VALUES(?,?,?)`, u, b.Keyword, time.Now().UnixMilli())
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		if k := r.URL.Query().Get("keyword"); k != "" {
			db.Exec(`DELETE FROM searchhistory WHERE username=? AND keyword=?`, u, k)
		} else {
			db.Exec(`DELETE FROM searchhistory WHERE username=?`, u)
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleLiveSources(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query(`SELECT key, name, url, ua, epg, disabled FROM livesources ORDER BY key`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var k, n, u2, ua, epg string
		var dis int
		rows.Scan(&k, &n, &u2, &ua, &epg, &dis)
		if dis == 1 {
			continue
		}
		out = append(out, map[string]any{"key": k, "name": n, "url": u2, "ua": ua, "epg": epg, "from": "custom"})
	}
	// 电视端期望：{data: [...]}
	writeJSON(w, 200, map[string]any{"data": out})
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	// 网页端直播：?url=<m3u地址> 直接解析频道分组返回
	u := r.URL.Query().Get("url")
	if u == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	ch, err := parseM3U(u)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"channels": ch})
}

func handleCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ, region, year, genre, keyword := q.Get("type"), q.Get("region"), q.Get("year"), q.Get("genre"), q.Get("q")
	limit := 60
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	sqlQ := `SELECT title, poster, year, region, genre, type, douban_id, rating, synopsis FROM catalog WHERE 1=1`
	// 体育已下线（2026-11-06）：目录接口不再返回体育类条目
	sqlQ += ` AND COALESCE(kind,'') != '体育'`
	args := []any{}
	// 网页粗分类（电影片/连续剧/国产剧/综艺片/动漫片/短剧）按 kind 匹配，
	// 其余值（动作片/喜剧片等体裁）仍按 type 精确匹配；两者互不干扰
	coarseKind := map[string]string{"电影片": "电影", "连续剧": "剧集", "国产剧": "剧集", "综艺片": "综艺", "动漫片": "动漫", "短剧": "短剧", "电影": "电影", "剧集": "剧集", "综艺": "综艺", "动漫": "动漫", "其他": "其他"}
	if typ != "" {
		if k, ok := coarseKind[typ]; ok {
			sqlQ += ` AND kind=?`
			args = append(args, k)
		} else {
			sqlQ += ` AND type=?`
			args = append(args, typ)
		}
	}
	if region != "" {
		// 地区归一化：库里“大陆/香港/台湾”与“中国大陆/中国香港/中国台湾”混用，
		// 去掉“中国”前缀再模糊匹配，两边写法都能命中
		reg := strings.TrimPrefix(region, "中国")
		sqlQ += ` AND region LIKE ?`
		args = append(args, "%"+reg+"%")
	}
	if year != "" {
		sqlQ += ` AND year=?`
		args = append(args, year)
	}
	if genre != "" {
		sqlQ += ` AND genre LIKE ?`
		args = append(args, "%"+genre+"%")
	}
	if keyword != "" {
		sqlQ += ` AND title LIKE ?`
		args = append(args, "%"+keyword+"%")
	}
	if q.Get("shuffle") == "1" {
		// “换一批”：约束随机，只出评分≥7 或近 3 年的，防翻出烂片
		sqlQ += ` AND (CAST(rating AS REAL) >= 7 OR CAST(year AS INTEGER) >= 2023)`
	}
	if q.Get("shuffle") == "1" {
		sqlQ += ` ORDER BY RANDOM() LIMIT ?`
	} else {
		sqlQ += ` ORDER BY updated_at DESC LIMIT ?`
	}
	args = append(args, limit)
	rows, err := db.Query(sqlQ, args...)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var title, poster, y, reg, gen, typ2, did, rating, syn string
		rows.Scan(&title, &poster, &y, &reg, &gen, &typ2, &did, &rating, &syn)
		out = append(out, map[string]any{"title": title, "poster": poster, "year": y, "region": reg, "genre": gen, "type": typ2, "douban_id": did, "rating": rating, "synopsis": syn})
	}
	writeJSON(w, 200, map[string]any{"items": out, "count": len(out)})
}

// handleCatalogMeta：货架下拉选项。地区按量排序并归一化显示
// （大陆→中国大陆等，查询侧去“中国”前缀，两边写法都命中）；
// 题材取 genre 字段切词后的高频 token（去掉 kind 级词），供类型下拉参考。
func handleCatalogMeta(w http.ResponseWriter, r *http.Request) {
	// ?type= 按粗分类过滤（与 handleCatalog 相同的 kind 映射），不传则全局；
	// 货架切分类时下拉选项跟着变，不再全库一套选项。
	typ := r.URL.Query().Get("type")
	kindFilter, typeFilter := "", ""
	coarseKind := map[string]string{"电影片": "电影", "连续剧": "剧集", "国产剧": "剧集", "综艺片": "综艺", "动漫片": "动漫", "短剧": "短剧", "电影": "电影", "剧集": "剧集", "综艺": "综艺", "动漫": "动漫", "其他": "其他"}
	if typ != "" {
		if k, ok := coarseKind[typ]; ok {
			kindFilter = k
		} else {
			typeFilter = typ
		}
	}
	regionQ := `SELECT region, COUNT(*) c FROM catalog WHERE region!='' AND COALESCE(kind,'') != '体育'`
	regionArgs := []any{}
	if kindFilter != "" {
		regionQ += ` AND kind=?`
		regionArgs = append(regionArgs, kindFilter)
	}
	if typeFilter != "" {
		regionQ += ` AND type=?`
		regionArgs = append(regionArgs, typeFilter)
	}
	regionQ += ` GROUP BY region ORDER BY c DESC LIMIT 40`
	regions := []string{}
	seen := map[string]bool{}
	if rows, err := db.Query(regionQ, regionArgs...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var reg string
			var c int
			if err := rows.Scan(&reg, &c); err != nil {
				continue
			}
			disp := reg
			switch reg {
			case "大陆":
				disp = "中国大陆"
			case "香港":
				disp = "中国香港"
			case "台湾":
				disp = "中国台湾"
			}
			if !seen[disp] {
				seen[disp] = true
				regions = append(regions, disp)
			}
			if len(regions) >= 16 {
				break
			}
		}
	}
	genreCount := map[string]int{}
	genreQ := `SELECT genre FROM catalog WHERE genre!='' AND COALESCE(kind,'') != '体育'`
	genreArgs := []any{}
	if kindFilter != "" {
		genreQ += ` AND kind=?`
		genreArgs = append(genreArgs, kindFilter)
	}
	if typeFilter != "" {
		genreQ += ` AND type=?`
		genreArgs = append(genreArgs, typeFilter)
	}
	if rows, err := db.Query(genreQ, genreArgs...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var g string
			if err := rows.Scan(&g); err != nil {
				continue
			}
			for _, tok := range strings.FieldsFunc(g, func(r rune) bool {
				return r == ',' || r == '，' || r == '、' || r == '/' || r == ' '
			}) {
				tok = strings.TrimSpace(tok)
				if len([]rune(tok)) < 2 {
					continue
				}
				switch tok {
				case "剧集", "短剧", "电影", "综艺", "动漫", "体育":
					continue
				}
				genreCount[tok]++
			}
		}
	}
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range genreCount {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
	genres := []string{}
	for i, p := range kvs {
		if i >= 40 {
			break
		}
		genres = append(genres, p.k)
	}
	writeJSON(w, 200, map[string]any{"regions": regions, "genres": genres})
}

// handleImageProxiesGet/Save：服务端拉图代理列表（设置页“图片代理”组）。
// 直连永远先试，代理只在直连失败后按顺序兜底；仅站长可改，普通用户可看。
func handleImageProxiesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"proxies": getImageProxies()})
}

func handleImageProxiesSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var body struct {
		Proxies []string `json:"proxies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	clean := []string{}
	for _, p := range body.Proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, 400, map[string]string{"error": "bad proxy: " + p})
			return
		}
		clean = append(clean, p)
	}
	b, _ := json.Marshal(clean)
	if _, err := db.Exec(`INSERT INTO meta(k, v) VALUES('image_proxies', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, string(b)); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"proxies": clean})
}

var catalogSyncRunning int32

// 单例锁：同步已在跑时不再起第二份，避免多点同步积压、写库打架
func runSyncIfFree(cfg syncConfig) bool {
	if !atomic.CompareAndSwapInt32(&catalogSyncRunning, 0, 1) {
		return false
	}
	go func() {
		defer atomic.StoreInt32(&catalogSyncRunning, 0)
		_ = syncCatalogWith(cfg)
	}()
	return true
}

func handleCatalogSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	if !runSyncIfFree(getSyncConfig()) {
		writeJSON(w, 409, map[string]string{"status": "sync already running"})
		return
	}
	writeJSON(w, 202, map[string]string{"status": "sync started"})
}

// 自动同步：每 5 分钟看一次配置，改配置不用重启、最多 5 分钟后生效；关掉自动后循环空转不跑。
// 两种模式（二选一）：
//   - 定时点模式（daily_time 非空，如 "03:00"，北京时间）：当天到点后、且今天还没同步过，就跑一轮；
//     到点时服务器没在跑，下次启动 5 分钟内会补跑一次。跑完货架同步后顺带刷一次上映数据（同一夜里一起拉齐）。
//   - 间隔模式（daily_time 为空）：距上次同步 >= 间隔才跑。
func syncSchedulerLoop() {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	for {
		time.Sleep(5 * time.Minute)
		cfg := getSyncConfig()
		if !cfg.Auto {
			continue
		}
		if cfg.DailyTime != "" {
			now := time.Now().In(loc)
			if now.Format("15:04") < cfg.DailyTime {
				continue
			}
			last := time.Unix(getLastSyncUnix(), 0).In(loc)
			if getLastSyncUnix() > 0 && last.Format("2006-01-02") == now.Format("2006-01-02") {
				continue
			}
		} else if time.Now().Unix()-getLastSyncUnix() < int64(cfg.IntervalHours)*3600 {
			continue
		}
		if runSyncIfFree(cfg) {
			triggerUpcomingRefresh()
			// 定时同步顺带刷新已保留的订阅（失败只记日志，不影响目录同步结果）
			refreshAllSubscriptions()
		}
	}
}

func handleSyncConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var count int
		_ = db.QueryRow(`SELECT COUNT(*) FROM catalog`).Scan(&count)
		running := atomic.LoadInt32(&catalogSyncRunning) == 1
		writeJSON(w, 200, map[string]any{"config": getSyncConfig(), "catalog_count": count, "last_sync": getLastSyncUnix(), "running": running})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	if r.Header.Get("X-Role") != "owner" {
		writeJSON(w, 403, map[string]string{"error": "owner only"})
		return
	}
	var cfg syncConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	if err := setSyncConfig(cfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"config": getSyncConfig()})
}

var _ = fmt.Sprintf
