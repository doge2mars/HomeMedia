package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const bangumiTestCalendar = `[{"weekday":{"en":"Mon","cn":"周一","ja":"月曜","id":1},"items":[
{"id":1,"name":"Test Anime","name_cn":"测试番","air_date":"2026-10-05","rating":{"score":8.5},"images":{"large":"https://lain.bgm.tv/pic/cover/l/1.jpg","common":"https://lain.bgm.tv/pic/cover/c/1.jpg","medium":""}},
{"id":2,"name":"No CN Name","name_cn":"","air_date":"2026-01-02","rating":{"score":0},"images":{"large":"","common":"https://lain.bgm.tv/pic/cover/c/2.jpg","medium":""}},
{"id":3,"name":"","name_cn":"","air_date":"","rating":{"score":0},"images":{}}]},
{"weekday":{"en":"Tue","cn":"周二","ja":"火曜","id":2},"items":[]}]`

func TestParseBangumiCalendar(t *testing.T) {
	wd, err := parseBangumiCalendar([]byte(bangumiTestCalendar))
	if err != nil {
		t.Fatalf("parseBangumiCalendar: %v", err)
	}
	if len(wd) != 2 || wd[0]["id"] != 1 || wd[0]["name"] != "周一" {
		t.Fatalf("weekdays = %v", wd)
	}
	items, _ := wd[0]["items"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (empty title skipped)", len(items))
	}
	if items[0]["title"] != "测试番" || items[0]["poster"] != "https://lain.bgm.tv/pic/cover/l/1.jpg" || items[0]["rating"] != "8.5" || items[0]["year"] != "2026" || items[0]["type"] != "动漫" {
		t.Errorf("item 0 = %v", items[0])
	}
	// name_cn 为空时用 name；评分为 0 时不带 rating；海报 large 为空时退 common
	if items[1]["title"] != "No CN Name" || items[1]["rating"] != "" || items[1]["poster"] != "https://lain.bgm.tv/pic/cover/c/2.jpg" || items[1]["year"] != "2026" {
		t.Errorf("item 1 = %v", items[1])
	}
	if items2, _ := wd[1]["items"].([]map[string]any); len(items2) != 0 {
		t.Errorf("weekday 2 items = %d, want 0", len(items2))
	}
}

func TestHandleBangumiCalendar(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(bangumiTestCalendar))
	}))
	defer srv.Close()
	old := bangumiCalendarAPI
	bangumiCalendarAPI = srv.URL
	defer func() { bangumiCalendarAPI = old }()
	bangumiCacheMu.Lock()
	bangumiCache = nil
	bangumiCacheMu.Unlock()

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/api/bangumi/calendar", nil)
		rec := httptest.NewRecorder()
		handleBangumiCalendar(rec, req)
		if rec.Code != 200 {
			t.Fatalf("call %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
		var d map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("call %d: bad json: %v", i, err)
		}
		if _, ok := d["weekdays"]; !ok {
			t.Fatalf("call %d: no weekdays in %v", i, d)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second call served from cache)", hits)
	}
}

func TestHandleBangumiCalendarUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	old := bangumiCalendarAPI
	bangumiCalendarAPI = srv.URL
	defer func() { bangumiCalendarAPI = old }()
	bangumiCacheMu.Lock()
	bangumiCache = nil
	bangumiCacheMu.Unlock()

	req := httptest.NewRequest("GET", "/api/bangumi/calendar", nil)
	rec := httptest.NewRecorder()
	handleBangumiCalendar(rec, req)
	if rec.Code != 502 {
		t.Fatalf("status %d, want 502, body %s", rec.Code, rec.Body.String())
	}
}
