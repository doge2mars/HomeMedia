package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

// 第二批发现功能（演进文档 §3.2 #5② #5③ #6）：
//   - /api/catalog/related：详情页“同类接着看”，本地 catalog 同 kind 优先、同 type/地区/年份加权、随机 8 部
//   - /api/catalog?shuffle=1：首页“换一批”，约束随机（评分≥7 或近 3 年），ORDER BY RANDOM()
//     （shuffle 参数在 main.go handleCatalog 里，只改 ORDER BY 与一条 WHERE 分支，默认路径不动）
//   - /api/trending-household：站内“家人在看”，playrecords 完成度加权 × 最近 7 天时间衰减
// 全是本地 SQL，零外部依赖；任何一步失败都返回空 items，前端自行回退/隐藏，首页永不因此挂掉。

// catalogRowItem：related 返回的条目形状，与 /api/catalog 返回字段对齐（前端 card() 直接能用）。
func catalogRowItem(title, poster, year, region, genre, typ, rating string) map[string]any {
	return map[string]any{"title": title, "poster": poster, "year": year, "region": region, "genre": genre, "type": typ, "rating": rating}
}

// handleCatalogRelated：GET /api/catalog/related?title=...&limit=8
// 先按 title 在 catalog 里找一行（精确 → 前缀 LIKE）拿到 kind/type/year/region，
// 再按“同 kind、同 type 优先、同地区优先、同年份优先、其余随机”取 limit 部，排除自身标题。
// title 在库里找不到、且 type/kind 参数也没给，返回空 items（前端隐藏该行）。
func handleCatalogRelated(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 8
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 20 {
			limit = n
		}
	}
	title := strings.TrimSpace(q.Get("title"))
	typ := strings.TrimSpace(q.Get("type"))
	kind := strings.TrimSpace(q.Get("kind"))
	year := strings.TrimSpace(q.Get("year"))
	region := strings.TrimSpace(q.Get("region"))
	if title != "" {
		var t, k, y, rg string
		err := db.QueryRow(`SELECT type, kind, year, region FROM catalog WHERE title=? ORDER BY updated_at DESC LIMIT 1`, title).Scan(&t, &k, &y, &rg)
		if err != nil {
			// 精确找不到就前缀 LIKE 再试一次（CMS 标题常带季/年份后缀）
			err = db.QueryRow(`SELECT type, kind, year, region FROM catalog WHERE title LIKE ? ORDER BY updated_at DESC LIMIT 1`, title+"%").Scan(&t, &k, &y, &rg)
		}
		if err == nil {
			if typ == "" {
				typ = t
			}
			if kind == "" {
				kind = k
			}
			if year == "" {
				year = y
			}
			if region == "" {
				region = rg
			}
		}
	}
	out := []map[string]any{}
	if kind == "" && typ == "" {
		writeJSON(w, 200, map[string]any{"items": out})
		return
	}
	// 地区归一化与 handleCatalog 同款：去掉“中国”前缀再模糊匹配
	reg := strings.TrimPrefix(region, "中国")
	var rows *sql.Rows
	var err error
	if kind != "" {
		rows, err = db.Query(`SELECT title, poster, year, region, genre, type, rating FROM catalog WHERE title<>? AND kind=? ORDER BY (type=?) DESC, (region LIKE ?) DESC, (year=?) DESC, RANDOM() LIMIT ?`,
			title, kind, typ, "%"+reg+"%", year, limit)
	} else {
		rows, err = db.Query(`SELECT title, poster, year, region, genre, type, rating FROM catalog WHERE title<>? AND type=? ORDER BY (region LIKE ?) DESC, (year=?) DESC, RANDOM() LIMIT ?`,
			title, typ, "%"+reg+"%", year, limit)
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t, p, y, rg, g, ty, rt string
		if err := rows.Scan(&t, &p, &y, &rg, &g, &ty, &rt); err != nil {
			continue
		}
		out = append(out, catalogRowItem(t, p, y, rg, g, ty, rt))
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// handleTrendingHousehold：GET /api/trending-household?limit=8
// 全家 playrecords 按标题聚合：完成度（play_time/total_time，上限 1，无总时长记 0.3）
// × 最近 7 天线性时间衰减（今天 1，7 天前 0），取 Top N。7 天内没人看过就返回空 items，
// 前端回退到豆瓣周榜。save_time 是毫秒（Date.now()）。
func handleTrendingHousehold(w http.ResponseWriter, r *http.Request) {
	limit := 8
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 20 {
			limit = n
		}
	}
	rows, err := db.Query(`
		SELECT title, MAX(cover), MAX(year),
			SUM((CASE WHEN total_time > 0 THEN MIN(1.0, play_time * 1.0 / total_time) ELSE 0.3 END)
				* MAX(0.0, 1.0 - (strftime('%s','now')*1000 - save_time) / 604800000.0)) AS score,
			MAX(save_time) AS last_seen
		FROM playrecords
		WHERE save_time > strftime('%s','now')*1000 - 604800000
		GROUP BY title
		ORDER BY score DESC, last_seen DESC
		LIMIT ?`, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var title, cover, year string
		var score float64
		var lastSeen int64
		if err := rows.Scan(&title, &cover, &year, &score, &lastSeen); err != nil {
			continue
		}
		out = append(out, map[string]any{"title": title, "poster": cover, "year": year, "score": score})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
