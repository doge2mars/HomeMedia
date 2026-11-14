package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscribeClaimsLegacySource(t *testing.T) {
	testDB := openTestDB(t)
	// 模拟历史无归属源（sub_url 为空/NULL）
	if _, err := testDB.Exec(`INSERT INTO sources(key, name, api, disabled, sub_url) VALUES('old_ff', '旧非凡', 'https://example.com/api.php/provide/vod/', 0, NULL)`); err != nil {
		t.Fatal(err)
	}
	subSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_site":{"ff":{"name":"非凡","api":"https://example.com/api.php/provide/vod"},"new":{"name":"新源","api":"https://example2.com/api.php/provide/vod/"}}}`))
	}))
	defer subSrv.Close()

	body, _ := json.Marshal(map[string]string{"url": subSrv.URL})
	req := httptest.NewRequest("POST", "/api/sources/subscribe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSourceSubscribe(rec, req)
	if rec.Code != 200 {
		t.Fatalf("subscribe status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reqList := httptest.NewRequest("GET", "/api/subscriptions", nil)
	recList := httptest.NewRecorder()
	handleSubscriptionsList(recList, reqList)
	if recList.Code != 200 {
		t.Fatalf("list status = %d", recList.Code)
	}
	var listResp struct {
		Subscriptions []struct {
			URL   string `json:"url"`
			Count int    `json:"count"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(recList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Subscriptions) != 1 || listResp.Subscriptions[0].Count != 2 {
		t.Fatalf("expected 1 subscription with count=2 (claimed legacy + new), got %+v", listResp.Subscriptions)
	}

	// 手动更新应正常替换并保持 count=2
	reqUpd := httptest.NewRequest("POST", "/api/subscriptions/update", bytes.NewReader(body))
	recUpd := httptest.NewRecorder()
	handleSubscriptionUpdate(recUpd, reqUpd)
	if recUpd.Code != 200 {
		t.Fatalf("update status = %d, body = %s", recUpd.Code, recUpd.Body.String())
	}
	var updResp map[string]any
	_ = json.Unmarshal(recUpd.Body.Bytes(), &updResp)
	if updResp["total"] != float64(2) {
		t.Fatalf("expected total=2, got %+v", updResp)
	}
}

func TestLiveSubscriptionLifecycle(t *testing.T) {
	testDB := openTestDB(t)
	_ = testDB

	liveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1 group-title=\"央视\",CCTV-1\nhttp://live.example.com/cctv1.m3u8\n"))
	}))
	defer liveSrv.Close()

	body, _ := json.Marshal(map[string]string{"url": liveSrv.URL, "name": "客厅直播"})
	req := httptest.NewRequest("POST", "/api/live/subscribe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleLiveSubscribe(rec, req)
	if rec.Code != 200 {
		t.Fatalf("live subscribe status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reqList := httptest.NewRequest("GET", "/api/live/subscriptions", nil)
	recList := httptest.NewRecorder()
	handleLiveSubscriptionsList(recList, reqList)
	var listResp struct {
		Subscriptions []struct {
			URL   string `json:"url"`
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(recList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Subscriptions) != 1 || listResp.Subscriptions[0].Count != 1 {
		t.Fatalf("expected 1 live subscription with count=1, got %+v", listResp.Subscriptions)
	}

	updBody, _ := json.Marshal(map[string]string{"url": liveSrv.URL})
	reqUpd := httptest.NewRequest("POST", "/api/live/subscriptions/update", bytes.NewReader(updBody))
	recUpd := httptest.NewRecorder()
	handleLiveSubscriptionUpdate(recUpd, reqUpd)
	if recUpd.Code != 200 {
		t.Fatalf("live update status = %d, body = %s", recUpd.Code, recUpd.Body.String())
	}

	reqDel := httptest.NewRequest("POST", "/api/live/subscriptions/delete", bytes.NewReader(updBody))
	recDel := httptest.NewRecorder()
	handleLiveSubscriptionDelete(recDel, reqDel)
	if recDel.Code != 200 {
		t.Fatalf("live delete status = %d", recDel.Code)
	}
	var cnt int
	_ = testDB.QueryRow(`SELECT COUNT(*) FROM livesources WHERE sub_url=?`, liveSrv.URL).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("expected 0 livesources left for deleted sub, got %d", cnt)
	}
}

func TestParsePlayURLsPrefersM3U8(t *testing.T) {
	raw := "第1集$https://share.example.com/v/1$$$第1集$https://cdn.example.com/1.m3u8#第2集$https://cdn.example.com/2.m3u8"
	eps := parsePlayURLs(raw)
	if len(eps) != 2 || eps[0] != "https://cdn.example.com/1.m3u8" {
		t.Fatalf("expected m3u8 group, got %+v", eps)
	}
}

func TestLoginAndPinRateLimit(t *testing.T) {
	testDB := openTestDB(t)
	_, _ = testDB.Exec(`INSERT OR REPLACE INTO users(username, role, passhash) VALUES('tester', 'user', ?)`, hashPass("tester", "secret123"))

	loginLimiter.reset("10.0.0.1")
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/login", bytes.NewBufferString(`{"username":"tester","password":"wrong"}`))
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		handleLogin(rec, req)
		if rec.Code != 401 {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, rec.Code)
		}
	}
	req6 := httptest.NewRequest("POST", "/api/login", bytes.NewBufferString(`{"username":"tester","password":"secret123"}`))
	req6.RemoteAddr = "10.0.0.1:12345"
	rec6 := httptest.NewRecorder()
	handleLogin(rec6, req6)
	if rec6.Code != 429 {
		t.Fatalf("6th attempt expected 429, got %d", rec6.Code)
	}
	loginLimiter.reset("10.0.0.1")

	pinLimiter.reset("10.0.0.2")
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/restricted/unlock", bytes.NewBufferString(`{"pin":"9999"}`))
		req.RemoteAddr = "10.0.0.2:12345"
		rec := httptest.NewRecorder()
		handleRestrictedUnlock(rec, req)
		if rec.Code != 403 {
			t.Fatalf("pin attempt %d: expected 403, got %d", i+1, rec.Code)
		}
	}
	reqPin6 := httptest.NewRequest("POST", "/api/restricted/unlock", bytes.NewBufferString(`{"pin":"1234"}`))
	reqPin6.RemoteAddr = "10.0.0.2:12345"
	recPin6 := httptest.NewRecorder()
	handleRestrictedUnlock(recPin6, reqPin6)
	if recPin6.Code != 429 {
		t.Fatalf("6th pin attempt expected 429, got %d", recPin6.Code)
	}
	pinLimiter.reset("10.0.0.2")
}

func TestCatalogDedupeTitleYear(t *testing.T) {
	testDB := openTestDB(t)
	_, _ = testDB.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind, hits_week, restricted) VALUES
		('流浪地球', '', '2019', '中国大陆', '科幻', '电影', '', '8.0', '', 100, '电影', 50, 0),
		('流浪地球', '', '2019', '中国大陆', '科幻', '科幻片', '', '7.9', '', 90, '电影', 30, 0),
		('沙丘', '', '2021', '美国', '科幻', '电影', '', '7.8', '', 80, '电影', 20, 0)`)
	// 注意 catalog 表主键是 (title, year) 时第二条会冲突，这里验证不同年份或同标题跨源行为：
	// 若 schema 主键为 (title, year)，则再插入一条空格/大小写差异的同片名验证应用层去重
	_, _ = testDB.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind, hits_week, restricted) VALUES
		('流浪地球 ', '', '2019', '中国大陆', '科幻', '科幻片', '', '7.9', '', 90, '电影', 30, 0)`)

	req := httptest.NewRequest("GET", "/api/catalog?type=电影&limit=10", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if rec.Code != 200 {
		t.Fatalf("catalog status = %d", rec.Code)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 deduplicated items, got %d (%+v)", len(resp.Items), resp.Items)
	}
}

func TestLiveImportFile(t *testing.T) {
	openTestDB(t)
	t.Setenv("HOME_MEDIA_DATA", t.TempDir())

	// 1) M3U 本地文件导入
	m3uBody, _ := json.Marshal(map[string]string{
		"filename": "iptv.m3u",
		"content":  "#EXTM3U\n#EXTINF:-1 group-title=\"央视\",CCTV-1\nhttp://live.example.com/1.m3u8\n#EXTINF:-1 group-title=\"卫视\",湖南卫视\nhttp://live.example.com/2.m3u8\n",
	})
	req1 := httptest.NewRequest("POST", "/api/live/import-file", bytes.NewReader(m3uBody))
	rec1 := httptest.NewRecorder()
	handleLiveImportFile(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("m3u import status = %d, body = %s", rec1.Code, rec1.Body.String())
	}
	var r1 map[string]any
	_ = json.Unmarshal(rec1.Body.Bytes(), &r1)
	if r1["channels"] != float64(2) || r1["format"] != "m3u" {
		t.Fatalf("unexpected m3u import response: %+v", r1)
	}

	reqCh := httptest.NewRequest("GET", "/api/live/channels?source="+r1["key"].(string), nil)
	recCh := httptest.NewRecorder()
	handleLiveChannelsV3(recCh, reqCh)
	if recCh.Code != 200 {
		t.Fatalf("channels status = %d, body = %s", recCh.Code, recCh.Body.String())
	}
	var chResp map[string]any
	_ = json.Unmarshal(recCh.Body.Bytes(), &chResp)
	if chResp["total"] != float64(2) {
		t.Fatalf("expected total=2 channels, got %+v", chResp)
	}

	// 2) TVBox TXT 本地文件导入
	txtBody, _ := json.Marshal(map[string]string{
		"name":     "家里TXT",
		"filename": "tv.txt",
		"content":  "央视,#genre#\nCCTV-5,http://live.example.com/5.m3u8\n",
	})
	req2 := httptest.NewRequest("POST", "/api/live/import-file", bytes.NewReader(txtBody))
	rec2 := httptest.NewRecorder()
	handleLiveImportFile(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("txt import status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var r2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &r2)
	if r2["channels"] != float64(1) || r2["format"] != "tvbox-txt" {
		t.Fatalf("unexpected txt import response: %+v", r2)
	}
}
