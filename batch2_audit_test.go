package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatch2RestrictedPinAndCatalogFilter(t *testing.T) {
	db := openTestDB(t)

	// 1. Seed a normal movie, a 福利片 (which kindOf maps to 电影!), and an 其他 item
	rows := []struct {
		title, year, typ, genre string
	}{
		{"正常大片", "2024", "动作片", "动作"},
		{"敏感福利电影", "2024", "福利片", "剧情"},
		{"未归类短片", "2024", "未知杂项", ""},
	}
	for _, r := range rows {
		if _, err := db.Exec(upsertCatalogSQL, r.title, "", r.year, "中国", r.genre, r.typ, "", "8.0", "", 100, kindOf(r.typ, r.genre), 10); err != nil {
			t.Fatal(err)
		}
	}
	// Backfill should mark 敏感福利电影 (kind=电影) and 未归类短片 (kind=其他) as restricted=1
	updated := backfillCatalogKinds()
	if updated < 2 {
		t.Fatalf("expected backfillCatalogKinds to update at least 2 restricted rows, got %d", updated)
	}

	// 2. Query /api/catalog without PIN unlock -> only 正常大片 should appear
	req := httptest.NewRequest("GET", "/api/catalog", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if !strings.Contains(rec.Body.String(), "正常大片") {
		t.Fatalf("expected 正常大片 in locked catalog: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "敏感福利电影") || strings.Contains(rec.Body.String(), "未归类短片") {
		t.Fatalf("restricted items leaked into locked catalog: %s", rec.Body.String())
	}

	// 3. Wrong PIN should return 403
	badUnlock := httptest.NewRequest("POST", "/api/restricted/unlock", bytes.NewBufferString(`{"pin":"0000"}`))
	badRec := httptest.NewRecorder()
	handleRestrictedUnlock(badRec, badUnlock)
	if badRec.Code != 403 {
		t.Fatalf("expected 403 for wrong PIN, got %d", badRec.Code)
	}

	// 4. Correct default PIN (1234) should return 200 and set HttpOnly cookie
	okUnlock := httptest.NewRequest("POST", "/api/restricted/unlock", bytes.NewBufferString(`{"pin":"1234"}`))
	okRec := httptest.NewRecorder()
	handleRestrictedUnlock(okRec, okUnlock)
	if okRec.Code != 200 {
		t.Fatalf("expected 200 for default PIN 1234, got %d", okRec.Code)
	}
	cookies := okRec.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly unlock cookie, got %+v", cookies)
	}

	// 5. Query /api/catalog with unlock cookie -> both 正常大片 and 敏感福利电影 appear
	reqUnlocked := httptest.NewRequest("GET", "/api/catalog", nil)
	reqUnlocked.AddCookie(cookies[0])
	recUnlocked := httptest.NewRecorder()
	handleCatalog(recUnlocked, reqUnlocked)
	if !strings.Contains(recUnlocked.Body.String(), "敏感福利电影") || !strings.Contains(recUnlocked.Body.String(), "未归类短片") {
		t.Fatalf("expected restricted items after PIN unlock, got: %s", recUnlocked.Body.String())
	}

	// 6. Change PIN to 5678 -> old cookie should immediately become invalid
	pinChange := httptest.NewRequest("POST", "/api/restricted/pin", bytes.NewBufferString(`{"pin":"5678"}`))
	pinRec := httptest.NewRecorder()
	handleRestrictedPinSave(pinRec, pinChange)
	if pinRec.Code != 200 {
		t.Fatalf("expected 200 when saving 4-digit PIN, got %d", pinRec.Code)
	}
	if isRestrictedUnlocked(reqUnlocked) {
		t.Fatal("expected old cookie signature to be invalidated after PIN change")
	}
}

func TestBatch2SearchFiltersAdultAndRestricted(t *testing.T) {
	db := openTestDB(t)
	resetHealthState()

	cmsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"list": []map[string]any{
				{"vod_id": "1", "vod_name": "测试正常片", "type_name": "动作片", "vod_class": "动作"},
				{"vod_id": "2", "vod_name": "测试伦理片", "type_name": "伦理片", "vod_class": "伦理"},
				{"vod_id": "3", "vod_name": "测试福利片", "type_name": "福利片", "vod_class": "剧情"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer cmsSrv.Close()

	_, _ = db.Exec(`DELETE FROM sources`)
	_, _ = db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('s1', '测试源', ?, 0)`, cmsSrv.URL)

	// 1. Locked search: 伦理片 is dropped by searchSource, 福利片 is hidden by handleSearch
	req := httptest.NewRequest("GET", "/api/search?q=测试", nil)
	rec := httptest.NewRecorder()
	handleSearch(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "测试正常片") {
		t.Fatalf("expected 测试正常片 in search results: %s", body)
	}
	if strings.Contains(body, "测试伦理片") || strings.Contains(body, "测试福利片") {
		t.Fatalf("expected adult/restricted items filtered from locked search: %s", body)
	}
}

func TestBatch2ImageNegCacheAndPrune(t *testing.T) {
	openTestDB(t)
	resetImageNegCache()
	tmp := t.TempDir()
	t.Setenv("HOME_MEDIA_DATA", tmp)

	var hits int32
	badImgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>EdgeOne Bot Challenge</html>"))
	}))
	defer badImgSrv.Close()

	// 1st request to HTML challenge page -> error and cached in imgNegCache
	if _, _, err := fetchAndCache(badImgSrv.URL + "/p1.jpg"); err == nil {
		t.Fatal("expected error on HTML challenge response")
	}
	// 2nd request should hit negative cache without calling upstream server again
	if _, _, err := fetchAndCache(badImgSrv.URL + "/p1.jpg"); err == nil {
		t.Fatal("expected error on negative cache hit")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected upstream to be called only once due to negative cache, got %d", got)
	}

	// Test pruneImageCache evicts oldest .bin + .meta files
	dir := filepath.Join(tmp, "image-cache")
	_ = os.MkdirAll(dir, 0755)
	f1 := filepath.Join(dir, "old.bin")
	m1 := filepath.Join(dir, "old.meta")
	f2 := filepath.Join(dir, "new.bin")
	m2 := filepath.Join(dir, "new.meta")
	_ = os.WriteFile(f1, bytes.Repeat([]byte("a"), 600), 0644)
	_ = os.WriteFile(m1, []byte("image/jpeg"), 0644)
	oldTime := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(f1, oldTime, oldTime)
	_ = os.WriteFile(f2, bytes.Repeat([]byte("b"), 600), 0644)
	_ = os.WriteFile(m2, []byte("image/jpeg"), 0644)

	oldMax, oldTrim := imageCacheMax, imageCacheTrim
	imageCacheMax, imageCacheTrim = 1000, 700
	defer func() {
		imageCacheMax, imageCacheTrim = oldMax, oldTrim
	}()

	// Total is ~1200 bytes; prune with maxBytes=1000, targetBytes=700 -> old.bin & old.meta evicted
	pruneImageCache()
	if _, err := os.Stat(f1); !os.IsNotExist(err) {
		t.Fatalf("expected old.bin to be evicted")
	}
	if _, err := os.Stat(m1); !os.IsNotExist(err) {
		t.Fatalf("expected old.meta to be evicted alongside old.bin")
	}
	if _, err := os.Stat(f2); err != nil {
		t.Fatalf("expected new.bin to be kept: %v", err)
	}
}
