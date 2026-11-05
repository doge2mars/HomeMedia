package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openTestDB：给需要 db 的测试开一个临时库（建全部表），跑完恢复全局 db。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	old := db
	t.Cleanup(func() { db = old })
	var err error
	db, err = sql.Open("sqlite3", filepath.Join(dir, "test.db")+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if err := initDB(); err != nil {
		t.Fatal(err)
	}
	return db
}

// resetHealthState：清掉健康计数、负缓存、探针缓存（各测试互不干扰）。
func resetHealthState() {
	healthMu.Lock()
	healthMap = map[string]*srcHealth{}
	healthMu.Unlock()
	negMu.Lock()
	negCache = map[string]time.Time{}
	negMu.Unlock()
	resetProbeCacheForTest()
}

func TestNegCacheSetHitExpiry(t *testing.T) {
	resetHealthState()
	old := negCacheTTL
	negCacheTTL = 50 * time.Millisecond
	defer func() { negCacheTTL = old }()

	if negCacheHit("s1", "繁花") {
		t.Fatal("unexpected hit before set")
	}
	negCacheSet("s1", "繁花")
	if !negCacheHit("s1", "繁花") {
		t.Fatal("expected hit after set")
	}
	if negCacheHit("s1", "别的词") {
		t.Fatal("different query must not hit")
	}
	if negCacheHit("s2", "繁花") {
		t.Fatal("different source must not hit")
	}
	time.Sleep(80 * time.Millisecond)
	if negCacheHit("s1", "繁花") {
		t.Fatal("expected expiry after TTL")
	}
}

func TestRecordSearchOutcomeAutoDisable(t *testing.T) {
	openTestDB(t)
	resetHealthState()
	if _, err := db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('bad1','坏源','http://127.0.0.1:9/api.php/provide/vod/',0)`); err != nil {
		t.Fatal(err)
	}
	boom := fmt.Errorf("boom")
	recordSearchOutcome("bad1", boom, 10)
	recordSearchOutcome("bad1", boom, 10)
	var dis int
	if err := db.QueryRow(`SELECT disabled FROM sources WHERE key='bad1'`).Scan(&dis); err != nil {
		t.Fatal(err)
	}
	if dis != 0 {
		t.Fatalf("disabled=%d after 2 fails, want 0 (single hiccup must not disable)", dis)
	}
	recordSearchOutcome("bad1", boom, 10)
	if err := db.QueryRow(`SELECT disabled FROM sources WHERE key='bad1'`).Scan(&dis); err != nil {
		t.Fatal(err)
	}
	if dis != 1 {
		t.Fatalf("disabled=%d after 3 consecutive fails, want 1", dis)
	}
	// 第 4 次失败不再重复写库；一次成功把计数清零
	recordSearchOutcome("bad1", boom, 10)
	if h := getHealth("bad1"); h.ConsecutiveFails != 4 {
		t.Fatalf("fails=%d, want 4", h.ConsecutiveFails)
	}
	recordSearchOutcome("bad1", nil, 5)
	if h := getHealth("bad1"); h.ConsecutiveFails != 0 {
		t.Fatalf("fails=%d after success, want 0", h.ConsecutiveFails)
	}
	found := false
	for _, k := range getHealthDisabled() {
		if k == "bad1" {
			found = true
		}
	}
	if !found {
		t.Fatal("bad1 not in health_disabled list")
	}
}

func TestRetestReenables(t *testing.T) {
	openTestDB(t)
	resetHealthState()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":1,"msg":"ok","list":[{"vod_id":"1","vod_name":"繁花","vod_pic":"","vod_year":"2023","vod_play_url":"第1集$http://example.com/1.m3u8"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('good1','好源',?,1)`, srv.URL+"/api.php/provide/vod/"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(key, name, api, disabled) VALUES('dead1','死源','http://127.0.0.1:9/api.php/provide/vod/',1)`); err != nil {
		t.Fatal(err)
	}
	setHealthDisabled([]string{"good1", "dead1"})

	out := retestHealthSources()
	if len(out) != 2 {
		t.Fatalf("retest results=%d, want 2", len(out))
	}
	var dis int
	if err := db.QueryRow(`SELECT disabled FROM sources WHERE key='good1'`).Scan(&dis); err != nil {
		t.Fatal(err)
	}
	if dis != 0 {
		t.Fatalf("good1 disabled=%d, want 0 (recovered source re-enabled)", dis)
	}
	if err := db.QueryRow(`SELECT disabled FROM sources WHERE key='dead1'`).Scan(&dis); err != nil {
		t.Fatal(err)
	}
	if dis != 1 {
		t.Fatalf("dead1 disabled=%d, want 1 (still failing)", dis)
	}
	for _, k := range getHealthDisabled() {
		if k == "good1" {
			t.Fatal("good1 still in health_disabled list after recovery")
		}
		if k != "dead1" {
			t.Fatalf("unexpected key %q in health_disabled list", k)
		}
	}
}

func TestScoreProbe(t *testing.T) {
	// 1080p 中速 应胜过 720p 高速：分辨率权重把纯速度排序翻过来
	a := scoreProbe("1920x1080", 4096, 100) // 0.4*1.0 + 0.4*0.8 + 0.2*0.95 = 0.91
	b := scoreProbe("1280x720", 5120, 100)  // 0.4*0.444 + 0.4*1.0 + 0.2*0.95 = 0.768
	if !(a > b) {
		t.Fatalf("1080p mid-speed score %v <= 720p fast %v", a, b)
	}
	if got := resolutionScore(""); got != 0.5 {
		t.Fatalf("resolutionScore(\"\")=%v, want 0.5", got)
	}
	if got := resolutionScore("3840x2160"); got != 1.0 {
		t.Fatalf("4K resolutionScore=%v, want 1.0 (capped)", got)
	}
	if got := resolutionScore("640x480"); got >= 0.5 {
		t.Fatalf("480p resolutionScore=%v, want < 0.5", got)
	}
	if got := latencyScore(5000); got != 0 {
		t.Fatalf("latencyScore(5000)=%v, want 0", got)
	}
	if got := latencyScore(0); got != 1.0 {
		t.Fatalf("latencyScore(0)=%v, want 1.0", got)
	}
}

// fakeProbeServer：两个候选源（B 720p 在前、A 1080p 在后），分片都在本机、速度双双封顶，
// 纯速度排序会保持原序 B 在前；加权排序必须让 1080p 的 A 排第一。
func fakeProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	seg := make([]byte, 256<<10)
	mux := http.NewServeMux()
	mux.HandleFunc("/b.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\n/b-v.m3u8\n")
	})
	mux.HandleFunc("/b-v.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:10,\n/b.ts\n")
	})
	mux.HandleFunc("/b.ts", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(seg) })
	mux.HandleFunc("/a.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1920x1080\n/a-v.m3u8\n")
	})
	mux.HandleFunc("/a-v.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:10,\n/a.ts\n")
	})
	mux.HandleFunc("/a.ts", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(seg) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func callPlayBest(t *testing.T, body string) []probeResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/play/best", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handlePlayBest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var d struct {
		Results []probeResult `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d.Results
}

func TestPlayBestWeightedOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME_MEDIA_DATA", dir)
	resetHealthState()
	srv := fakeProbeServer(t)
	body := fmt.Sprintf(`{"candidates":[{"source":"b","source_name":"B源","url":%q},{"source":"a","source_name":"A源","url":%q}]}`,
		srv.URL+"/b.m3u8", srv.URL+"/a.m3u8")
	res := callPlayBest(t, body)
	if len(res) != 2 {
		t.Fatalf("results=%d, want 2", len(res))
	}
	if !res[0].OK || !res[1].OK {
		t.Fatalf("both should be OK: %+v", res)
	}
	if res[0].Source != "a" {
		t.Fatalf("first=%s, want a (1080p must rank first by weighted score)", res[0].Source)
	}
	if res[0].Score <= res[1].Score {
		t.Fatalf("scores not ordered: %v <= %v", res[0].Score, res[1].Score)
	}
	if res[0].Resolution != "1920x1080" || res[0].LatencyMs < 0 {
		t.Fatalf("missing resolution/latency in result: %+v", res[0])
	}
}

func TestPlayBestProbeCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME_MEDIA_DATA", dir)
	resetHealthState()
	seg := make([]byte, 128<<10)
	var playlistHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/c.m3u8", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&playlistHits, 1)
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:10,\n/c.ts\n")
	})
	mux.HandleFunc("/c.ts", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(seg) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := fmt.Sprintf(`{"candidates":[{"source":"c","source_name":"C源","url":%q}]}`, srv.URL+"/c.m3u8")
	res1 := callPlayBest(t, body)
	if len(res1) != 1 || !res1[0].OK {
		t.Fatalf("first call not OK: %+v", res1)
	}
	if playlistHits != 1 {
		t.Fatalf("playlist hits after first call=%d, want 1", playlistHits)
	}
	res2 := callPlayBest(t, body)
	if len(res2) != 1 || !res2[0].OK {
		t.Fatalf("second call not OK: %+v", res2)
	}
	if playlistHits != 1 {
		t.Fatalf("playlist hits after second call=%d, want 1 (cached)", playlistHits)
	}
	if res2[0].SpeedKBs != res1[0].SpeedKBs || res2[0].Score != res1[0].Score {
		t.Fatalf("cached result differs: %+v vs %+v", res2[0], res1[0])
	}
}

func TestRetestProbeBypassesNegCache(t *testing.T) {
	openTestDB(t)
	resetHealthState()
	// 假源：第一次请求 500，之后恢复 200。先搜一次让它失败、进负缓存，
	// 再跑复测探针——探针必须真的去问源站（绕开负缓存），看到恢复才算健康。
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, "boom")
			return
		}
		fmt.Fprint(w, `{"code":1,"list":[{"vod_id":"1","vod_name":"繁花","vod_play_url":"第1集$http://example.com/1.m3u8"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := Source{Key: "flap1", Name: "抽风源", API: srv.URL + "/api.php/provide/vod/"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := searchSourceCtx(ctx, s, "繁花"); err == nil {
		t.Fatal("first search should fail (500)")
	}
	if !negCacheHit("flap1", "繁花") {
		t.Fatal("negative cache should be set after failure")
	}
	if !probeSourceHealth(s) {
		t.Fatal("retest probe must bypass negative cache and see the recovered source")
	}
}

func TestProbeSourceHealthRejectsSlowSource(t *testing.T) {
	openTestDB(t)
	resetHealthState()
	// 慢源：12 秒才响应——15 秒人工探针能过，但 10 秒搜索预算下必超时，
	// 复测探针必须判它不健康（否则恢复后立刻又被连续失败停用，来回抖动）。
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(12 * time.Second)
		fmt.Fprint(w, `{"code":1,"list":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := Source{Key: "slow1", Name: "慢源", API: srv.URL + "/api.php/provide/vod/"}
	start := time.Now()
	if probeSourceHealth(s) {
		t.Fatal("probeSourceHealth must reject a source slower than the search budget")
	}
	if d := time.Since(start); d > 11*time.Second {
		t.Fatalf("probe took %v, want ~10s timeout", d)
	}
}
