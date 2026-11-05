package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDoubanListURL(t *testing.T) {
	cases := []struct {
		name       string
		proxyType  string
		proxyURL   string
		wantPrefix string
	}{
		{"direct", "direct", "", "https://movie.douban.com/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
		{"tencent", "tencent", "", "https://movie.douban.cmliussss.net/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
		{"ali", "ali", "", "https://movie.douban.cmliussss.com/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
		{"zwei", "zwei", "", "https://ciao-cors.is-an.org/https%3A%2F%2Fmovie.douban.com%2Fj%2Fsearch_subjects"},
		{"custom", "custom", "https://movie.douban.cmliussss.net", "https://movie.douban.cmliussss.net/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
		{"custom-empty-falls-back", "custom", "", "https://movie.douban.com/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
		{"bad-type-falls-back", "bogus", "", "https://movie.douban.com/j/search_subjects?type=movie&tag=%E7%83%AD%E9%97%A8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", c.proxyType)
			t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", c.proxyURL)
			got := doubanListURL("movie", "热门", "recommend", 12)
			if !strings.HasPrefix(got, c.wantPrefix) {
				t.Errorf("doubanListURL = %q, want prefix %q", got, c.wantPrefix)
			}
		})
	}
}

func TestFetchDoubanList(t *testing.T) {
	// 假豆瓣：返回 j/search_subjects 同款结构
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/j/search_subjects" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("tag") != "热门" || r.URL.Query().Get("type") != "movie" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		if r.Header.Get("Referer") != "https://movie.douban.com/" {
			t.Errorf("Referer = %q, want douban referer", r.Header.Get("Referer"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "测试电影", "cover": "https://img.example.com/p1.jpg", "rate": "8.5", "card_subtitle": "2026 / 中国大陆 / 剧情"},
			{"id": "2", "title": "无评分电影", "cover": "https://img.example.com/p2.jpg", "rate": "", "card_subtitle": "2025 / 美国 / 动作"},
			{"id": "3", "title": "", "cover": "https://img.example.com/p3.jpg", "rate": "7.0", "card_subtitle": "2024"},
		}})
	}))
	defer srv.Close()
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "custom")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", srv.URL)

	items, err := fetchDoubanList("movie", "热门", "recommend", 12)
	if err != nil {
		t.Fatalf("fetchDoubanList: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (empty title skipped)", len(items))
	}
	if items[0]["title"] != "测试电影" || items[0]["poster"] != "https://img.example.com/p1.jpg" || items[0]["rating"] != "8.5" || items[0]["year"] != "2026" || items[0]["type"] != "电影" {
		t.Errorf("item 0 = %v", items[0])
	}
	if items[1]["rating"] != "" || items[1]["year"] != "2025" {
		t.Errorf("item 1 = %v", items[1])
	}
}

func TestHandleDoubanCache(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "缓存电影", "cover": "https://img.example.com/p1.jpg", "rate": "9.0", "card_subtitle": "2026"},
		}})
	}))
	defer srv.Close()
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "custom")
	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_URL", srv.URL)
	doubanCacheMu.Lock()
	doubanCache = map[string]doubanCacheEntry{}
	doubanCacheMu.Unlock()

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/api/douban?type=movie&tag=热门&limit=12", nil)
		rec := httptest.NewRecorder()
		handleDouban(rec, req)
		if rec.Code != 200 {
			t.Fatalf("call %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second call served from cache)", hits)
	}
}

func TestHandleDoubanProxyTest(t *testing.T) {
	// 假豆瓣：只返回 1 条
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subjects": []map[string]any{
			{"id": "1", "title": "测试电影", "cover": "https://img.example.com/p1.jpg", "rate": "8.5", "card_subtitle": "2026"},
		}})
	}))
	defer srv.Close()

	// 成功：用 body 里的 custom 档位（未保存也能测）
	body := `{"type":"custom","custom":` + strconv.Quote(srv.URL) + `}`
	req := httptest.NewRequest("POST", "/api/douban-proxy/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleDoubanProxyTest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var d struct {
		OK    bool   `json:"ok"`
		Items int    `json:"items"`
		Ms    int64  `json:"ms"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !d.OK || d.Items != 1 {
		t.Fatalf("result = %+v, want ok with 1 item", d)
	}

	// 失败：指到已关掉的服务，应回 ok=false 而不是 5xx
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	body2 := `{"type":"custom","custom":` + strconv.Quote(dead.URL) + `}`
	req2 := httptest.NewRequest("POST", "/api/douban-proxy/test", strings.NewReader(body2))
	rec2 := httptest.NewRecorder()
	handleDoubanProxyTest(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("dead upstream: status %d, want 200 with ok=false", rec2.Code)
	}
	var d2 struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &d2); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if d2.OK || d2.Error == "" {
		t.Fatalf("dead upstream result = %+v, want ok=false with error", d2)
	}

	// 非 POST 应 405
	req3 := httptest.NewRequest("GET", "/api/douban-proxy/test", nil)
	rec3 := httptest.NewRecorder()
	handleDoubanProxyTest(rec3, req3)
	if rec3.Code != 405 {
		t.Fatalf("GET status %d, want 405", rec3.Code)
	}

	// 非法档位应 400
	req4 := httptest.NewRequest("POST", "/api/douban-proxy/test", strings.NewReader(`{"type":"bogus"}`))
	rec4 := httptest.NewRecorder()
	handleDoubanProxyTest(rec4, req4)
	if rec4.Code != 400 {
		t.Fatalf("bogus type status %d, want 400", rec4.Code)
	}
}
