package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func seedCatalogRow(t *testing.T, title, poster, year, region, genre, typ, rating, kind string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind) VALUES(?,?,?,?,?,?,?,?,?,strftime('%s','now'),?)`,
		title, poster, year, region, genre, typ, "", rating, "", kind); err != nil {
		t.Fatal(err)
	}
}

func decodeItems(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return d.Items
}

func titlesOf(items []map[string]any) []string {
	out := []string{}
	for _, it := range items {
		s, _ := it["title"].(string)
		out = append(out, s)
	}
	return out
}

func TestHandleCatalogRelated(t *testing.T) {
	openTestDB(t)
	seedCatalogRow(t, "测试剧A", "pA", "2024", "大陆", "剧情", "剧情片", "8.0", "剧集")
	seedCatalogRow(t, "测试剧B", "pB", "2024", "大陆", "剧情", "剧情片", "8.5", "剧集")  // 同 type+地区+年份，应排第一
	seedCatalogRow(t, "测试剧C", "pC", "2020", "香港", "喜剧", "喜剧片", "7.0", "剧集")  // 同 kind，不同 type/地区/年份
	seedCatalogRow(t, "测试剧E", "pE", "2020", "美国", "剧情", "剧情片", "7.5", "剧集")  // 同 type，不同地区/年份
	seedCatalogRow(t, "测试电影D", "pD", "2024", "大陆", "剧情", "剧情片", "9.0", "电影") // 不同 kind，不应出现

	req := httptest.NewRequest("GET", "/api/catalog/related?title="+url.QueryEscape("测试剧A")+"&limit=8", nil)
	rec := httptest.NewRecorder()
	handleCatalogRelated(rec, req)
	items := decodeItems(t, rec)
	got := titlesOf(items)
	if len(got) != 3 {
		t.Fatalf("got titles %v, want 3 (B, E, C)", got)
	}
	if got[0] != "测试剧B" {
		t.Errorf("first = %q, want 测试剧B (same type+region+year ranks first)", got[0])
	}
	if got[1] != "测试剧E" {
		t.Errorf("second = %q, want 测试剧E (same type ranks above different type)", got[1])
	}
	if got[2] != "测试剧C" {
		t.Errorf("third = %q, want 测试剧C", got[2])
	}

	// 标题在库里找不到 → 空 items，不报错
	req = httptest.NewRequest("GET", "/api/catalog/related?title="+url.QueryEscape("不存在的片名"), nil)
	rec = httptest.NewRecorder()
	handleCatalogRelated(rec, req)
	if items := decodeItems(t, rec); len(items) != 0 {
		t.Errorf("unknown title: got %d items, want 0", len(items))
	}
}

func TestHandleCatalogShuffle(t *testing.T) {
	openTestDB(t)
	seedCatalogRow(t, "高分老片", "p1", "2020", "大陆", "剧情", "剧情片", "8.5", "电影")
	seedCatalogRow(t, "新片未评分", "p2", "2024", "大陆", "剧情", "剧情片", "0.0", "电影")
	seedCatalogRow(t, "低分老片", "p3", "2019", "大陆", "剧情", "剧情片", "5.0", "电影")
	seedCatalogRow(t, "低分更老", "p4", "2015", "大陆", "剧情", "剧情片", "0.0", "电影")
	seedCatalogRow(t, "高分新片", "p5", "2025", "大陆", "剧情", "剧情片", "9.0", "电影")

	req := httptest.NewRequest("GET", "/api/catalog?shuffle=1&limit=10", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	got := titlesOf(decodeItems(t, rec))
	want := map[string]bool{"高分老片": true, "新片未评分": true, "高分新片": true}
	if len(got) != 3 {
		t.Fatalf("shuffle got %v, want exactly the 3 quality rows", got)
	}
	for _, title := range got {
		if !want[title] {
			t.Errorf("shuffle returned %q, should be excluded (low score and old)", title)
		}
	}

	// 不带 shuffle：默认路径不动，5 部全出
	req = httptest.NewRequest("GET", "/api/catalog?limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := decodeItems(t, rec); len(got) != 5 {
		t.Errorf("plain catalog got %d items, want 5", len(got))
	}
}

func seedPlayRecord(t *testing.T, user, source, id, title string, playTime, totalTime, saveTimeMs int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO playrecords(username, source, id, title, source_name, cover, year, idx, total_episodes, play_time, total_time, save_time, search_title) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		user, source, id, title, "源", "cover", "2024", 0, 10, playTime, totalTime, saveTimeMs, title); err != nil {
		t.Fatal(err)
	}
}

func TestHandleTrendingHousehold(t *testing.T) {
	openTestDB(t)
	now := time.Now().UnixMilli()
	hour := int64(3600 * 1000)
	day := int64(86400 * 1000)
	seedPlayRecord(t, "admin", "s1", "1", "剧X", 2000, 2500, now-hour)   // 完成度 0.8，今天
	seedPlayRecord(t, "user2", "s1", "1", "剧X", 1500, 2500, now-2*hour) // 同标题第二人，0.6
	seedPlayRecord(t, "admin", "s1", "2", "剧Y", 100, 3000, now-hour)    // 完成度 0.03
	seedPlayRecord(t, "admin", "s1", "3", "剧W", 0, 0, now-hour)         // 无总时长，记 0.3
	seedPlayRecord(t, "admin", "s1", "4", "剧Z", 2500, 2500, now-8*day)  // 8 天前，窗口外

	req := httptest.NewRequest("GET", "/api/trending-household?limit=8", nil)
	rec := httptest.NewRecorder()
	handleTrendingHousehold(rec, req)
	got := titlesOf(decodeItems(t, rec))
	want := []string{"剧X", "剧W", "剧Y"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d = %q, want %q (full list %v)", i, got[i], w, got)
		}
	}
}

func TestHandleTrendingHouseholdEmpty(t *testing.T) {
	openTestDB(t)
	req := httptest.NewRequest("GET", "/api/trending-household", nil)
	rec := httptest.NewRecorder()
	handleTrendingHousehold(rec, req)
	if items := decodeItems(t, rec); len(items) != 0 {
		t.Errorf("empty db: got %d items, want 0 (frontend falls back to douban weekly)", len(items))
	}
}

func TestDoubanTagPassthrough(t *testing.T) {
	// 新行用的 tag/sort 组合（港片/rank、国产剧/rank、美剧/recommend…）必须原样透传给豆瓣
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "无间道", "cover": "https://img.example.com/p1.jpg", "rate": "9.3", "card_subtitle": "2002 / 中国香港 / 剧情"},
		}})
	}))
	defer srv.Close()
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "custom")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", srv.URL)

	items, err := fetchDoubanList("movie", "港片", "rank", 12)
	if err != nil {
		t.Fatalf("fetchDoubanList: %v", err)
	}
	if gotQuery.Get("tag") != "港片" || gotQuery.Get("sort") != "rank" || gotQuery.Get("type") != "movie" {
		t.Errorf("upstream query = %v, want tag=港片 sort=rank type=movie", gotQuery)
	}
	if len(items) != 1 || items[0]["title"] != "无间道" || items[0]["year"] != "2002" {
		t.Errorf("items = %v", items)
	}
}
