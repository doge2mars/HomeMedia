package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- 第五批跟进修复：豆瓣翻页 ----------

func TestDoubanListURLStart(t *testing.T) {
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "direct")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", "")
	u := doubanListURL("movie", "热门", "recommend", 30, 60)
	if !strings.Contains(u, "page_start=60") || !strings.Contains(u, "page_limit=30") {
		t.Errorf("doubanListURL start=60: %q", u)
	}
	u0 := doubanListURL("movie", "热门", "recommend", 30, 0)
	if !strings.Contains(u0, "page_start=0") {
		t.Errorf("doubanListURL start=0: %q", u0)
	}
}

func TestFetchDoubanListStart(t *testing.T) {
	var gotStart []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = append(gotStart, r.URL.Query().Get("page_start"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "第61部", "cover": "https://img.example.com/p1.jpg", "rate": "8.0", "card_subtitle": "2026 / 中国大陆 / 剧情"},
		}})
	}))
	defer srv.Close()
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "custom")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", srv.URL)

	items, err := fetchDoubanList("movie", "热门", "recommend", 30, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotStart) != 1 || gotStart[0] != "60" {
		t.Errorf("page_start sent = %v, want [60]", gotStart)
	}
	if len(items) != 1 || items[0]["title"] != "第61部" {
		t.Errorf("items = %v", items)
	}
}

func TestHandleDoubanStartCache(t *testing.T) {
	openTestDB(t)
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("page_start"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "片", "cover": "https://img.example.com/p1.jpg", "rate": "8.0"},
		}})
	}))
	defer srv.Close()
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "custom")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", srv.URL)

	get := func(start string) {
		req := httptest.NewRequest("GET", "/api/douban?type=movie&tag=喜剧&sort=recommend&limit=30&start="+start, nil)
		rec := httptest.NewRecorder()
		handleDouban(rec, req)
		if rec.Code != 200 {
			t.Fatalf("start=%s: status %d %s", start, rec.Code, rec.Body.String())
		}
	}
	get("0")
	get("30")
	get("30") // 命中缓存，不应再调上游
	if len(starts) != 2 || starts[0] != "0" || starts[1] != "30" {
		t.Errorf("upstream starts = %v, want [0 30]", starts)
	}
}

// ---------- 第五批跟进修复：货架默认排序 + 全部视图排除短剧 ----------

func seedCatalogRowHits(t *testing.T, title, kind string, hits, updated int64, rating string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind, hits_week) VALUES(?, '', '2024', '大陆', '剧情', '剧情片', '', ?, '', ?, ?, ?)`,
		title, rating, updated, kind, hits); err != nil {
		t.Fatal(err)
	}
}

func TestHandleCatalogDefaultSort(t *testing.T) {
	openTestDB(t)
	seedCatalogRowHits(t, "热片A", "电影", 100, 1000, "8.0")
	seedCatalogRowHits(t, "热片B", "电影", 50, 3000, "9.0")
	seedCatalogRowHits(t, "短剧X", "短剧", 9999, 5000, "9.5")
	seedCatalogRowHits(t, "冷片C", "电影", 0, 2000, "7.0")

	// 默认：按热度，且全部视图排除短剧（短剧点击量最高也不置顶）
	req := httptest.NewRequest("GET", "/api/catalog?limit=10", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := titlesOf(decodeItems(t, rec)); !equalTitles(got, []string{"热片A", "热片B", "冷片C"}) {
		t.Errorf("default sort = %v, want [热片A 热片B 冷片C]", got)
	}

	// 显式选短剧：仍能正常看
	req = httptest.NewRequest("GET", "/api/catalog?type=短剧&limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := titlesOf(decodeItems(t, rec)); !equalTitles(got, []string{"短剧X"}) {
		t.Errorf("type=短剧 = %v, want [短剧X]", got)
	}

	// sort=latest：最新入库优先（短剧仍排除）
	req = httptest.NewRequest("GET", "/api/catalog?sort=latest&limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := titlesOf(decodeItems(t, rec)); !equalTitles(got, []string{"热片B", "冷片C", "热片A"}) {
		t.Errorf("sort=latest = %v, want [热片B 冷片C 热片A]", got)
	}

	// sort=rating：评分优先
	req = httptest.NewRequest("GET", "/api/catalog?sort=rating&limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := titlesOf(decodeItems(t, rec)); !equalTitles(got, []string{"热片B", "热片A", "冷片C"}) {
		t.Errorf("sort=rating = %v, want [热片B 热片A 冷片C]", got)
	}

	// shuffle=1：仍返回行（短剧是否混入不作断言，只保行为不空不断）
	req = httptest.NewRequest("GET", "/api/catalog?shuffle=1&limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := decodeItems(t, rec); len(got) != 4 {
		t.Errorf("shuffle = %d items, want 4", len(got))
	}
}

func TestHandleCatalogHitsWeekAllZeroGuard(t *testing.T) {
	openTestDB(t)
	seedCatalogRowHits(t, "片A", "电影", 0, 1000, "8.0")
	seedCatalogRowHits(t, "片B", "电影", 0, 2000, "7.0")

	// 显式 sort=hits_week 全 0：返回空行（不冒充热榜，保护逻辑不动）
	req := httptest.NewRequest("GET", "/api/catalog?sort=hits_week&limit=10", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := decodeItems(t, rec); len(got) != 0 {
		t.Errorf("sort=hits_week all-zero = %d items, want 0", len(got))
	}

	// 默认排序全 0：仍返回行（按 updated_at 退化），货架不为空
	req = httptest.NewRequest("GET", "/api/catalog?limit=10", nil)
	rec = httptest.NewRecorder()
	handleCatalog(rec, req)
	if got := decodeItems(t, rec); len(got) != 2 {
		t.Errorf("default all-zero = %d items, want 2", len(got))
	}
}

func equalTitles(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
