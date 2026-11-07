package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBatch3SyncBatchTxCircuitBreakerAndSanitize(t *testing.T) {
	db := openTestDB(t)

	var deadCatCalls int32
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ac") == "list" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"class": []map[string]any{
					{"type_id": "1", "type_name": "动作片"},
					{"type_id": "2", "type_name": "喜剧片"},
					{"type_id": "3", "type_name": "爱情片"},
					{"type_id": "4", "type_name": "科幻片"},
					{"type_id": "5", "type_name": "恐怖片"},
				},
			})
			return
		}
		atomic.AddInt32(&deadCatCalls, 1)
		w.WriteHeader(500)
	}))
	defer deadSrv.Close()

	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ac") == "list" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"class": []map[string]any{
					{"type_id": "1", "type_name": "动作片"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"list": []map[string]any{
				{"vod_id": "10", "vod_name": "清洗测试A", "type_name": "动作片", "vod_year": "韩国", "vod_score": "99.9", "vod_hits_week": "120"},
				{"vod_id": "11", "vod_name": "清洗测试B", "type_name": "动作片", "vod_year": "2024年上映", "vod_score": "8.6", "vod_hits_week": "80"},
			},
		})
	}))
	defer goodSrv.Close()

	_, _ = db.Exec(`DELETE FROM sources`)
	_, _ = db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('dead', '坏源', ?, 0)`, deadSrv.URL)
	_, _ = db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('good', '好源', ?, 0)`, goodSrv.URL)

	if err := syncCatalogWith(syncConfig{Pages: 1}); err != nil {
		t.Fatalf("syncCatalogWith: %v", err)
	}

	// Circuit breaker should stop deadSrv after 3 consecutive failed categories (not 5)
	if got := atomic.LoadInt32(&deadCatCalls); got != 3 {
		t.Fatalf("expected circuit breaker to stop dead source at 3 category failures, got %d", got)
	}

	// Check sanitized year & rating on goodSrv items
	var yA, rA string
	if err := db.QueryRow(`SELECT year, rating FROM catalog WHERE title='清洗测试A'`).Scan(&yA, &rA); err != nil {
		t.Fatalf("清洗测试A not in catalog: %v", err)
	}
	if yA != "" || rA != "" {
		t.Fatalf("expected bogus year/rating to be sanitized to empty, got year=%q rating=%q", yA, rA)
	}

	var yB, rB string
	if err := db.QueryRow(`SELECT year, rating FROM catalog WHERE title='清洗测试B'`).Scan(&yB, &rB); err != nil {
		t.Fatalf("清洗测试B not in catalog: %v", err)
	}
	if yB != "2024" || rB != "8.6" {
		t.Fatalf("expected extracted year=2024 rating=8.6, got year=%q rating=%q", yB, rB)
	}
}

func TestBatch3DoubanFallbackChain(t *testing.T) {
	openTestDB(t)

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer badSrv.Close()

	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subjects": []map[string]any{
				{"id": "1", "title": "降级成功电影", "cover": "https://img.example.com/1.jpg", "rate": "9.1", "card_subtitle": "2025 / 中国大陆 / 剧情"},
			},
		})
	}))
	defer goodSrv.Close()

	oldDirect := doubanProxyTypes["direct"]
	oldTencent := doubanProxyTypes["tencent"]
	doubanProxyTypes["direct"] = badSrv.URL
	doubanProxyTypes["tencent"] = goodSrv.URL
	defer func() {
		doubanProxyTypes["direct"] = oldDirect
		doubanProxyTypes["tencent"] = oldTencent
	}()

	t.Setenv("HOME_MEDIA_DOUBAN_PROXY_TYPE", "direct")
	items, err := fetchDoubanList("movie", "热门", "recommend", 12, 0)
	if err != nil {
		t.Fatalf("expected automatic fallback to succeed when primary direct fails, got err: %v", err)
	}
	if len(items) != 1 || items[0]["title"] != "降级成功电影" {
		t.Fatalf("unexpected fallback items: %+v", items)
	}
}

func TestBatch3SecurityCookiesAndSecretAndSSRF(t *testing.T) {
	openTestDB(t)
	t.Setenv("HOME_MEDIA_SECRET", "")

	ensureCookieSecret()
	if len(secret) < 32 || string(secret) == "home-media-dev-secret-change-me" {
		t.Fatalf("expected random persisted cookie_secret >= 32 bytes, got %q", string(secret))
	}
	saved, ok := metaGet("cookie_secret")
	if !ok || saved != string(secret) {
		t.Fatalf("expected cookie_secret persisted in meta, got %q", saved)
	}

	rec := httptest.NewRecorder()
	setAuthCookies(rec, "admin", "owner")
	cookies := rec.Result().Cookies()
	if len(cookies) < 2 {
		t.Fatalf("expected auth cookies, got %d", len(cookies))
	}
	for _, c := range cookies {
		if !c.HttpOnly {
			t.Errorf("cookie %s should be HttpOnly", c.Name)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie %s should have SameSiteLaxMode", c.Name)
		}
	}

	// SSRF & scheme check on proxy endpoints
	for _, bad := range []string{
		"file:///etc/passwd",
		"http://192.168.1.1/internal.m3u8",
		"http://169.254.169.254/latest/meta-data",
	} {
		if _, err := validateProxyURL(bad); err == nil {
			t.Errorf("expected validateProxyURL(%q) to fail", bad)
		}
	}
}

func TestBatch3DramaMovieAndShortDramaClassification(t *testing.T) {
	// 1. 家庭篇 -> 电影
	if got := kindOf("家庭篇", "奇幻,歌舞,家庭"); got != "电影" {
		t.Errorf("kindOf(家庭篇) = %q, want 电影", got)
	}
	// 2. 竖屏短剧专属标签 -> 短剧
	for _, tc := range [][2]string{
		{"现代都市", "现代都市"},
		{"古装仙侠", "短-古装仙侠,古装仙侠"},
		{"成长逆袭", "成长逆袭"},
		{"反转爽剧", ""},
		{"反转爽文", ""},
		{"言情总裁", ""},
		{"闪婚离婚", ""},
		{"重生民国", ""},
	} {
		if got := kindOf(tc[0], tc[1]); got != "短剧" {
			t.Errorf("kindOf(%q, %q) = %q, want 短剧", tc[0], tc[1], got)
		}
	}
	// 3. 动漫中的 AI漫剧 仍归 动漫
	if got := kindOf("AI漫剧", ""); got != "动漫" {
		t.Errorf("kindOf(AI漫剧) = %q, want 动漫", got)
	}
	// 4. refineTypeWithPlayMeta: 单集 HD中字 误放进海外剧 -> 纠正为剧情片（电影）
	gotTyp := refineTypeWithPlayMeta("哈奴曼", "海外剧", "动作,奇幻", "HD中字", "HD中字$https://example.com/1.m3u8")
	if kindOf(gotTyp, "动作,奇幻") != "电影" {
		t.Errorf("expected single-ep HD movie in 海外剧 to be refined to 电影, got type=%q kind=%q", gotTyp, kindOf(gotTyp, "动作,奇幻"))
	}
	// 5. refineTypeWithPlayMeta: 多集连续剧误放进剧情片 -> 纠正为连续剧（剧集）
	gotTyp2 := refineTypeWithPlayMeta("狂飙", "剧情片", "犯罪", "全39集", "第01集$https://example.com/1.m3u8#第02集$https://example.com/2.m3u8#第03集$https://example.com/3.m3u8")
	if kindOf(gotTyp2, "犯罪") != "剧集" {
		t.Errorf("expected multi-ep series in 剧情片 to be refined to 剧集, got type=%q kind=%q", gotTyp2, kindOf(gotTyp2, "犯罪"))
	}
}

