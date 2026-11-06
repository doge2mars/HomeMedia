package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// v0.3：订阅导入 + 账户管理 + 服务端缓存 + 每用户设置
// 依据（rea-lab 源码实测）：
//  - 配置订阅：admin/config_file 的 subscriptionUrl（ConfigSubscribtion.URL，支持 autoUpdate）
//  - 直播导入导出：admin/live/import-export，data.sources
//  - 直播格式检测（lib/live.ts）：TVBox JSON（lives 数组）或 TVBox TXT，另有 M3U
//  - 角色：owner / admin / user；注册可配邀请码（本原型暂不强制邀请码）
//  - 搜索缓存：getCacheTime() 取管理配置，Cache-Control max-age=cacheTime

func ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Role") != "owner" {
			writeJSON(w, 403, map[string]string{"error": "owner only"})
			return
		}
		next(w, r)
	}
}

// ---- 服务端缓存：cache 表（key, value, expires_at） ----
func cacheGet(key string) (string, bool) {
	var v string
	var exp int64
	err := db.QueryRow(`SELECT v, expires_at FROM cache WHERE k=?`, key).Scan(&v, &exp)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return v, true
}
func cacheSet(key, v string, ttl time.Duration) {
	db.Exec(`INSERT OR REPLACE INTO cache(k, v, expires_at) VALUES(?,?,?)`, key, v, time.Now().Add(ttl).Unix())
}
func cacheDelPrefix(prefix string) {
	db.Exec(`DELETE FROM cache WHERE k LIKE ?`, prefix+"%")
}

// ---- 订阅抓取（视频源 / 直播源共用，管理端可用局域网地址，如家里电视盒/ NAS 上的订阅） ----
func fetchSubscription(raw string) ([]byte, string, error) {
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	return body, resp.Header.Get("Content-Type"), err
}

type subSource struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	API  string `json:"api"`
}

// decodeSubscription：按 真实链路解码，再做兼容
// 顺序：明文 JSON -> Base58(JSON) -> Base64(JSON)，三者都试，Base58 订阅走第二步
func decodeSubscription(body []byte) ([]byte, string, error) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, "empty", fmt.Errorf("empty subscription content")
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		return body, "plain-json", nil
	}
	// 去掉订阅文件里常见的空白/换行再解
	compact := regexp.MustCompile(`\s+`).ReplaceAllString(text, "")
	if decoded, err := base58Decode(compact); err == nil {
		if t := strings.TrimSpace(string(decoded)); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			return decoded, "base58-json", nil
		}
	}
	if decoded, err := base64StdDecode(compact); err == nil {
		if t := strings.TrimSpace(string(decoded)); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			return decoded, "base64-json", nil
		}
	}
	// 实在解不开：把原文当 JSON 再试一次，报错里带上原文前 80 字，方便定位
	return body, "plain-json-fallback", fmt.Errorf("could not decode subscription: not plain JSON, not Base58 JSON, not Base64 JSON")
}

func base64StdDecode(s string) ([]byte, error) {
	// 标准 base64，缺 padding 时补齐
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64Decode(s)
}

func parseSourceSubscription(body []byte) ([]subSource, []string, int) {
	var out []subSource
	var keys []string
	skipped := 0
	add := func(k, n, a string) {
		if a == "" {
			skipped++
			return
		}
		if k == "" {
			// 没有 key 的源：按名称生成 key，很多源列表就是只有 name+api
			// 中文名经 sanitize 后会变空/只剩 "_"，那种情况改用序号 key，避免多个中文源撞成同一个 "_"
			k = regexp.MustCompile(`[^a-zA-Z0-9_-]+`).ReplaceAllString(n, "_")
			if strings.Trim(k, "_") == "" {
				k = fmt.Sprintf("sub_%d", len(out))
			}
		}
		out = append(out, subSource{Key: k, Name: n, API: a})
	}
	var arr []map[string]any
	if json.Unmarshal(body, &arr) == nil {
		for _, m := range arr {
			add(sField(m, "key"), sField(m, "name", "title"), sField(m, "api", "api_url", "url", "address"))
		}
		return out, keys, skipped
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return out, keys, skipped
	}
	for k := range obj {
		keys = append(keys, k)
	}
	for _, field := range []string{"apiSites", "sites", "sources"} {
		if list, ok := obj[field].([]any); ok {
			for _, el := range list {
				if m, ok := el.(map[string]any); ok {
					add(sField(m, "key"), sField(m, "name", "title"), sField(m, "api", "api_url", "url", "address"))
				}
			}
		}
	}
	// 配置文件格式：api_site: {key: {name, api, detail?}}，值也可能是纯字符串地址
	if m, ok := obj["api_site"].(map[string]any); ok {
		for k, v := range m {
			if sm, ok := v.(map[string]any); ok {
				add(k, sField(sm, "name", "title"), sField(sm, "api", "api_url", "url", "address"))
			} else if s, ok := v.(string); ok {
				add(k, k, s)
			}
		}
	}
	return out, keys, skipped
}

func parseLivesFromConfig(body []byte) []map[string]string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil
	}
	var out []map[string]string
	if m, ok := obj["lives"].(map[string]any); ok {
		for k, v := range m {
			if lm, ok := v.(map[string]any); ok {
				if u := sField(lm, "url"); u != "" {
					out = append(out, map[string]string{"key": k, "name": sField(lm, "name", "title"), "url": u, "ua": sField(lm, "ua"), "epg": sField(lm, "epg")})
				}
			}
		}
	}
	return out
}

func handleSourceSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	body, _, err := fetchSubscription(b.URL)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	decoded, format, decErr := decodeSubscription(body)
	if decErr != nil {
		preview := strings.TrimSpace(string(body))
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		writeJSON(w, 400, map[string]string{"error": decErr.Error(), "format_tried": "plain-json, base58-json, base64-json", "content_preview": preview, "content_length": strconv.Itoa(len(body))})
		return
	}
	srcs, topKeys, skipped := parseSourceSubscription(decoded)
	lives := parseLivesFromConfig(decoded)
	if len(srcs) == 0 && len(lives) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no sources parsed", "format": format, "top_level_keys": strings.Join(topKeys, ","), "skipped_no_api": strconv.Itoa(skipped)})
		return
	}
	n := 0
	deduped := 0
	for _, s := range srcs {
		if _, existed := dedupeOnSubscribe(s); existed {
			// 已有相同 api 的源只更新名称，不改归属：它可能是手动加的或别的订阅带来的
			deduped++
			continue
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO sources(key, name, api, disabled, sub_url) VALUES(?,?,?,0,?)`, s.Key, s.Name, s.API, b.URL); err == nil {
			n++
		}
	}
	ln := 0
	for _, lv := range lives {
		if _, err := db.Exec(`INSERT OR REPLACE INTO livesources(key, name, url, ua, epg, disabled) VALUES(?,?,?,?,?,0)`, lv["key"], lv["name"], lv["url"], lv["ua"], lv["epg"]); err == nil {
			ln++
		}
	}
	cacheDelPrefix("search:")
	// 订阅地址本身存库：之后可在设置里手动更新、删除，定时同步时也会顺带刷新
	saveSubscription(b.URL)
	writeJSON(w, 200, map[string]any{"imported": n, "total": len(srcs), "deduped": deduped, "lives_imported": ln, "format": format, "skipped_no_api": skipped})
}

// ---- 订阅保留：订阅地址本身存库，可手动/定时刷新；刷新只替换该订阅带来的源 ----

func subNameOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func saveSubscription(raw string) {
	now := time.Now().Unix()
	_, _ = db.Exec(`INSERT INTO subscriptions(url, name, created_at, updated_at) VALUES(?,?,?,?) ON CONFLICT(url) DO UPDATE SET name=excluded.name, updated_at=excluded.updated_at`, raw, subNameOf(raw), now, now)
}

// refreshSubscription：重抓订阅并替换该订阅带来的源。抓取/解析失败时原样保留，不动旧源。
func refreshSubscription(raw string) (imported, total int, err error) {
	body, _, err := fetchSubscription(raw)
	if err != nil {
		return 0, 0, err
	}
	decoded, _, decErr := decodeSubscription(body)
	if decErr != nil {
		return 0, 0, decErr
	}
	srcs, _, _ := parseSourceSubscription(decoded)
	if _, err := db.Exec(`DELETE FROM sources WHERE sub_url=?`, raw); err != nil {
		return 0, 0, err
	}
	for _, s := range srcs {
		if _, existed := dedupeOnSubscribe(s); existed {
			continue
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO sources(key, name, api, disabled, sub_url) VALUES(?,?,?,0,?)`, s.Key, s.Name, s.API, raw); err == nil {
			imported++
		}
	}
	saveSubscription(raw)
	cacheDelPrefix("search:")
	return imported, len(srcs), nil
}

func refreshAllSubscriptions() {
	rows, err := db.Query(`SELECT url FROM subscriptions`)
	if err != nil {
		return
	}
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err == nil {
			urls = append(urls, u)
		}
	}
	rows.Close()
	for _, u := range urls {
		if n, total, err := refreshSubscription(u); err != nil {
			log.Printf("subscription refresh failed %s: %v", u, err)
		} else {
			log.Printf("subscription refreshed %s: %d/%d", u, n, total)
		}
	}
}

func handleSubscriptionsList(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query(`SELECT s.url, s.name, s.updated_at, (SELECT COUNT(*) FROM sources WHERE sub_url=s.url) FROM subscriptions s ORDER BY s.updated_at DESC`)
	out := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var url, name string
			var updated int64
			var cnt int
			if err := rows.Scan(&url, &name, &updated, &cnt); err == nil {
				out = append(out, map[string]any{"url": url, "name": name, "updated_at": updated, "count": cnt})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"subscriptions": out})
}

func handleSubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	n, total, err := refreshSubscription(b.URL)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"imported": n, "total": total})
}

func handleSubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	res, _ := db.Exec(`DELETE FROM sources WHERE sub_url=?`, b.URL)
	removed, _ := res.RowsAffected()
	_, _ = db.Exec(`DELETE FROM subscriptions WHERE url=?`, b.URL)
	writeJSON(w, 200, map[string]any{"removed_sources": removed})
}

// ---- 直播订阅导入：M3U / TVBox TXT / TVBox JSON / live import JSON ----
func handleLiveSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "missing url"})
		return
	}
	body, _, err := fetchSubscription(b.URL)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	text := strings.TrimSpace(string(body))
	keyBase := b.Name
	if keyBase == "" {
		keyBase = "sub"
	}
	keyBase = regexp.MustCompile(`[^a-zA-Z0-9_-]+`).ReplaceAllString(keyBase, "_")
	imported := 0
	// A) TVBox JSON / live import JSON
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		var lives []any
		if v, ok := obj["lives"].([]any); ok {
			lives = v
		} else if v, ok := obj["sources"].([]any); ok {
			lives = v
		}
		for i, el := range lives {
			if m, ok := el.(map[string]any); ok {
				u := sField(m, "url")
				if u == "" {
					if urls, ok := m["urls"].([]any); ok && len(urls) > 0 {
						if s, ok := urls[0].(string); ok {
							u = s
						}
					}
				}
				if u == "" {
					continue
				}
				k := fmt.Sprintf("%s_%d", keyBase, i)
				if v := sField(m, "key"); v != "" {
					k = v
				}
				db.Exec(`INSERT OR REPLACE INTO livesources(key, name, url, ua, epg, disabled) VALUES(?,?,?,?,?,0)`, k, sField(m, "name"), u, sField(m, "ua"), sField(m, "epg"))
				imported++
			}
		}
		if imported > 0 {
			writeJSON(w, 200, map[string]any{"imported": imported, "format": "tvbox-json"})
			return
		}
	}
	// B) M3U 或 TXT：整条订阅存为一个直播源，频道按分组解析（parseM3U / parseTVBoxTXT）
	if strings.HasPrefix(text, "#EXTM3U") || strings.Contains(text, "#EXTINF") || strings.Contains(text, ",#genre#") {
		k := keyBase
		name := b.Name
		if name == "" {
			name = b.URL
		}
		db.Exec(`INSERT OR REPLACE INTO livesources(key, name, url, ua, epg, disabled) VALUES(?,?,?,?,?,0)`, k, name, b.URL, "", "")
		writeJSON(w, 200, map[string]any{"imported": 1, "format": "m3u-or-txt-subscription"})
		return
	}
	writeJSON(w, 400, map[string]string{"error": "unrecognized live subscription format"})
}

// TVBox TXT 解析：分组行 "分组,#genre#"，频道行 "名称,地址"（地址可多个 $ 分隔取第一个）
func parseTVBoxTXT(text string) []Channel {
	var out []Channel
	group := "未分组"
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, ",#genre#") {
			group = strings.SplitN(line, ",", 2)[0]
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) == 2 && strings.HasPrefix(strings.TrimSpace(parts[1]), "http") {
			u := strings.Split(parts[1], "$")[0]
			out = append(out, Channel{Name: strings.TrimSpace(parts[0]), URL: strings.TrimSpace(u), Group: group})
		}
	}
	return out
}

func handleLiveChannelsV3(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("source")
	fresh := r.URL.Query().Get("fresh") == "1"
	if key == "" {
		writeJSON(w, 400, map[string]string{"error": "missing source"})
		return
	}
	cacheKey := "livechannels:" + key
	if !fresh {
		if v, ok := cacheGet(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "hit")
			_, _ = w.Write([]byte(v))
			return
		}
	}
	var u string
	if err := db.QueryRow(`SELECT url FROM livesources WHERE key=?`, key).Scan(&u); err != nil {
		writeJSON(w, 404, map[string]string{"error": "live source not found"})
		return
	}
	// 单个 m3u8 地址 = 单个频道
	if strings.HasSuffix(strings.Split(u, "?")[0], ".m3u8") && !strings.Contains(u, ".m3u") {
		// 仍尝试当 M3U 订阅抓一次，抓不到则当单频道
		if ch, err := parseM3U(u); err == nil && len(ch) > 0 {
			writeLiveChannelsJSON(w, ch, cacheKey)
			return
		}
		var name string
		db.QueryRow(`SELECT name FROM livesources WHERE key=?`, key).Scan(&name)
		writeLiveChannelsJSON(w, []Channel{{Name: name, URL: u, Group: "直播"}}, cacheKey)
		return
	}
	ch, err := parseM3U(u)
	if err != nil || len(ch) == 0 {
		// 回退 TVBox TXT
		if body, _, ferr := fetchSubscription(u); ferr == nil {
			if tch := parseTVBoxTXT(string(body)); len(tch) > 0 {
				writeLiveChannelsJSON(w, tch, cacheKey)
				return
			}
		}
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
	}
	writeLiveChannelsJSON(w, ch, cacheKey)
}

func writeLiveChannelsJSON(w http.ResponseWriter, ch []Channel, cacheKey string) {
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
	data, _ := json.Marshal(map[string]any{"groups": groups, "order": order, "total": len(ch)})
	cacheSet(cacheKey, string(data), 6*time.Hour)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "miss")
	_, _ = w.Write(data)
}

// ---- 账户管理（owner） ----
func handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, _ := db.Query(`SELECT username, role, created_at FROM users ORDER BY created_at`)
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var u, role string
			var ct int64
			rows.Scan(&u, &role, &ct)
			out = append(out, map[string]any{"username": u, "role": role, "created_at": ct})
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var b struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Username) < 3 || len(b.Password) < 6 {
			writeJSON(w, 400, map[string]string{"error": "username>=3, password>=6"})
			return
		}
		role := b.Role
		if role != "admin" && role != "user" {
			role = "user"
		}
		if _, err := db.Exec(`INSERT INTO users(username, passhash, role, created_at) VALUES(?,?,?,?)`, b.Username, hashPass(b.Username, b.Password), role, time.Now().Unix()); err != nil {
			writeJSON(w, 409, map[string]string{"error": "user exists"})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case http.MethodDelete:
		u := r.URL.Query().Get("username")
		if u == "" || u == currentUser(r) {
			writeJSON(w, 400, map[string]string{"error": "cannot delete self or empty"})
			return
		}
		db.Exec(`DELETE FROM users WHERE username=?`, u)
		db.Exec(`DELETE FROM playrecords WHERE username=?`, u)
		db.Exec(`DELETE FROM favorites WHERE username=?`, u)
		writeJSON(w, 200, map[string]string{"ok": "true"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.New) < 6 {
		writeJSON(w, 400, map[string]string{"error": "new password>=6"})
		return
	}
	u := currentUser(r)
	var ph string
	db.QueryRow(`SELECT passhash FROM users WHERE username=?`, u).Scan(&ph)
	if ph != hashPass(u, b.Old) {
		writeJSON(w, 401, map[string]string{"error": "old password wrong"})
		return
	}
	db.Exec(`UPDATE users SET passhash=? WHERE username=?`, hashPass(u, b.New), u)
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// ---- 每用户设置（服务端存，换设备跟人走，不只 localStorage） ----
func handleUserSettings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if r.Method == http.MethodPost {
		var m map[string]string
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad body"})
			return
		}
		for k, v := range m {
			db.Exec(`INSERT OR REPLACE INTO user_settings(username, k, v) VALUES(?,?,?)`, u, k, v)
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
		return
	}
	rows, _ := db.Query(`SELECT k, v FROM user_settings WHERE username=?`, u)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		out[k] = v
	}
	writeJSON(w, 200, out)
}

func handleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	db.Exec(`DELETE FROM cache`)
	metaCacheClear() // 内存里的配置缓存也清掉
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

var _ = strconv.Itoa
var _ = url.QueryEscape
