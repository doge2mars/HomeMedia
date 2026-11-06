package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- #16 配置缓存 ----------
func TestMetaCacheSetGetInvalidate(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	if _, ok := metaGet("sync_config"); ok {
		t.Fatalf("metaGet on empty db should be absent")
	}
	if err := metaSet("sync_config", `{"pages":5}`); err != nil {
		t.Fatal(err)
	}
	v, ok := metaGet("sync_config")
	if !ok || v != `{"pages":5}` {
		t.Fatalf("metaGet after metaSet = %q, %v", v, ok)
	}
	// 绕过缓存直接删库里行：缓存未过期时仍读到旧值（证明缓存命中）
	if _, err := db.Exec(`DELETE FROM meta WHERE k='sync_config'`); err != nil {
		t.Fatal(err)
	}
	v, ok = metaGet("sync_config")
	if !ok || v != `{"pages":5}` {
		t.Fatalf("metaGet should return cached value, got %q, %v", v, ok)
	}
	// 失效后重新查库：行已删，返回 absent
	metaInvalidate("sync_config")
	if _, ok := metaGet("sync_config"); ok {
		t.Fatalf("metaGet after invalidate should be absent")
	}
	// metaSet 更新后立即生效
	if err := metaSet("sync_config", `{"pages":7}`); err != nil {
		t.Fatal(err)
	}
	v, ok = metaGet("sync_config")
	if !ok || v != `{"pages":7}` {
		t.Fatalf("metaGet after metaSet update = %q, %v", v, ok)
	}
	metaCacheClear()
}

// ---------- #13 hits_week 回填：多源冲突时取最大值，不清零 ----------
func TestCatalogHitsWeekUpsertMax(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	args := []any{"测试片", "", "2024", "大陆", "剧情", "剧情片", "", "8.0", "", 1000, "电影", 500}
	if _, err := db.Exec(upsertCatalogSQL, args...); err != nil {
		t.Fatal(err)
	}
	// 另一源不带点击量（0）：不能把 500 清零
	args[11] = 0
	if _, err := db.Exec(upsertCatalogSQL, args...); err != nil {
		t.Fatal(err)
	}
	var h int
	if err := db.QueryRow(`SELECT hits_week FROM catalog WHERE title='测试片'`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h != 500 {
		t.Fatalf("hits_week after zero upsert = %d, want 500", h)
	}
	// 另一源报更高：取 800
	args[11] = 800
	if _, err := db.Exec(upsertCatalogSQL, args...); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT hits_week FROM catalog WHERE title='测试片'`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h != 800 {
		t.Fatalf("hits_week after 800 upsert = %d, want 800", h)
	}
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM catalog WHERE title='测试片'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("upsert should not duplicate rows, got %d", cnt)
	}
}

// ---------- #13 hits_week 排序 + 回填前全 0 返回空 ----------
func TestCatalogHitsWeekSort(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	seed := []struct {
		title string
		hits  int
	}{
		{"低热片", 10},
		{"高热片", 9999},
		{"中热片", 500},
	}
	for _, s := range seed {
		if _, err := db.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind, hits_week) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.title, "", "2024", "大陆", "剧情", "剧情片", "", "8.0", "", 1000, "电影", s.hits); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/api/catalog?sort=hits_week&limit=10", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(d.Items))
	}
	if d.Items[0]["title"] != "高热片" || d.Items[1]["title"] != "中热片" || d.Items[2]["title"] != "低热片" {
		t.Fatalf("wrong order: %v", d.Items)
	}
	// 全 0 时返回空行（不冒充热榜）
	if _, err := db.Exec(`UPDATE catalog SET hits_week=0`); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	handleCatalog(rec2, httptest.NewRequest("GET", "/api/catalog?sort=hits_week&limit=10", nil))
	var d2 struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &d2); err != nil {
		t.Fatal(err)
	}
	if len(d2.Items) != 0 {
		t.Fatalf("all-zero hits_week should return empty, got %d items", len(d2.Items))
	}
}

// ---------- #10 搜索联想 ----------
func TestSearchSuggest(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	titles := []string{"繁花", "繁花似锦", "大江大河", "繁星"}
	for _, title := range titles {
		if _, err := db.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			title, "", "2024", "大陆", "剧情", "剧情片", "", "8.0", "", 1000, "电影"); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/api/search/suggest?q="+"繁", nil)
	rec := httptest.NewRecorder()
	handleSearchSuggest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Items) != 3 {
		t.Fatalf("suggest for 繁 = %v, want 3 prefix matches", d.Items)
	}
	for _, it := range d.Items {
		if !strings.HasPrefix(it, "繁") {
			t.Fatalf("suggest item %q is not a prefix match", it)
		}
	}
	// 空 q 返回空
	rec2 := httptest.NewRecorder()
	handleSearchSuggest(rec2, httptest.NewRequest("GET", "/api/search/suggest", nil))
	var d2 struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &d2); err != nil {
		t.Fatal(err)
	}
	if len(d2.Items) != 0 {
		t.Fatalf("empty q should return no items, got %v", d2.Items)
	}
}

func TestParseHits(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"123", 123},
		{"12.7", 12},
		{"", 0},
		{"abc", 0},
		{"0", 0},
		{" 456 ", 456},
	}
	for _, c := range cases {
		if got := parseHits(c.in); got != c.want {
			t.Errorf("parseHits(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---------- #12 TMDB：标题兜底 + 跳过无标题条目 ----------
func TestFetchTMDBTrendingFallback(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "testkey" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"中文名电影","original_title":"English Title","poster_path":"/a.jpg","vote_average":8.7,"release_date":"2026-11-01"},
			{"title":"","name":"","original_title":"No Chinese Title","poster_path":"","vote_average":7.2,"release_date":"2026-10-20"},
			{"title":"","name":"","original_title":"","original_name":"","poster_path":"/c.jpg","vote_average":9.0,"release_date":"2026-09-01"},
			{"title":"无海报片","poster_path":"","vote_average":0,"release_date":"2025-05-05"}
		]}`))
	}))
	defer srv.Close()
	oldBase := tmdbBaseURL
	tmdbBaseURL = srv.URL
	t.Cleanup(func() { tmdbBaseURL = oldBase })
	t.Setenv("HOME_MEDIA_TMDB_API_KEY", "testkey")

	items, err := fetchTMDBTrendingOne("movie", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (one title-less item skipped)", len(items))
	}
	if items[0]["title"] != "中文名电影" {
		t.Errorf("item0 title = %v", items[0]["title"])
	}
	if items[1]["title"] != "No Chinese Title" {
		t.Errorf("item1 title should fall back to original_title, got %v", items[1]["title"])
	}
	if items[1]["poster"] != "" {
		t.Errorf("item1 poster should be empty, got %v", items[1]["poster"])
	}
	if items[0]["poster"] != "https://image.tmdb.org/t/p/w500/a.jpg" {
		t.Errorf("item0 poster = %v", items[0]["poster"])
	}
	if items[0]["rating"] != "8.7" {
		t.Errorf("item0 rating = %v, want 8.7", items[0]["rating"])
	}
	if items[2]["rating"] != "" {
		t.Errorf("item2 rating should be empty for vote 0, got %v", items[2]["rating"])
	}
}

// 无 key 时 handler 返回 no_key:true（行不出现、不报错）
func TestHandleTMDBTrendingNoKey(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	t.Setenv("HOME_MEDIA_TMDB_API_KEY", "")
	req := httptest.NewRequest("GET", "/api/tmdb/trending?type=all&limit=12", nil)
	rec := httptest.NewRecorder()
	handleTMDBTrending(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var d struct {
		Items []map[string]any `json:"items"`
		NoKey bool             `json:"no_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.NoKey || len(d.Items) != 0 {
		t.Fatalf("no-key response = %+v", d)
	}
}

// 有 key 时 handler 走假 TMDB 服务并返回趋势
func TestHandleTMDBTrendingWithKey(t *testing.T) {
	openTestDB(t)
	metaCacheClear()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/movie/") {
			_, _ = w.Write([]byte(`{"results":[{"title":"电影A","poster_path":"/m.jpg","vote_average":8.1,"release_date":"2026-11-02"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"name":"剧集B","original_name":"Show B","poster_path":"/t.jpg","vote_average":9.2,"first_air_date":"2026-10-15"}]}`))
	}))
	defer srv.Close()
	oldBase := tmdbBaseURL
	tmdbBaseURL = srv.URL
	t.Cleanup(func() { tmdbBaseURL = oldBase })
	t.Setenv("HOME_MEDIA_TMDB_API_KEY", "testkey")

	req := httptest.NewRequest("GET", "/api/tmdb/trending?type=all&limit=6", nil)
	rec := httptest.NewRecorder()
	handleTMDBTrending(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Items []map[string]any `json:"items"`
		NoKey bool             `json:"no_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.NoKey || len(d.Items) != 2 {
		t.Fatalf("trending response = %+v", d)
	}
	if d.Items[0]["title"] != "电影A" || d.Items[1]["title"] != "剧集B" {
		t.Fatalf("wrong items: %v", d.Items)
	}
}
