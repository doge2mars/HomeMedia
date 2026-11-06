package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// ---------- 第六批：meta 返回 kind 计数与年份 ----------

func TestCatalogMetaKindsYears(t *testing.T) {
	db := openTestDB(t)
	seed := []struct{ title, kind, year string }{
		{"片A", "电影", "2024"},
		{"片B", "电影", "2024"},
		{"片C", "剧集", "2023"},
		{"片D", "短剧", "2025"},
		{"片E", "体育", "2024"},
	}
	for _, s := range seed {
		if _, err := db.Exec(`INSERT INTO catalog(title, poster, year, region, genre, type, douban_id, rating, synopsis, updated_at, kind) VALUES(?, '', ?, '', '', '', '', '', '', 0, ?)`, s.title, s.year, s.kind); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("GET", "/api/catalog/meta", nil)
	rec := httptest.NewRecorder()
	handleCatalogMeta(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	kinds, _ := body["kinds"].([]any)
	got := map[string]float64{}
	for _, kv := range kinds {
		m, _ := kv.(map[string]any)
		got[m["kind"].(string)] = m["count"].(float64)
	}
	if got["电影"] != 2 || got["剧集"] != 1 || got["短剧"] != 1 {
		t.Errorf("kinds = %v", got)
	}
	if _, ok := got["体育"]; ok {
		t.Errorf("体育 should be excluded, kinds = %v", got)
	}
	years, _ := body["years"].([]any)
	if len(years) != 3 {
		t.Errorf("years = %v, want 3 distinct years", years)
	}
	if len(years) > 0 && years[0] != "2025" {
		t.Errorf("years[0] = %v, want 2025 (desc)", years[0])
	}
	// 按 kind 过滤时，年份列表跟着走
	req2 := httptest.NewRequest("GET", "/api/catalog/meta?type=电影", nil)
	rec2 := httptest.NewRecorder()
	handleCatalogMeta(rec2, req2)
	var body2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	years2, _ := body2["years"].([]any)
	if len(years2) != 1 || years2[0] != "2024" {
		t.Errorf("years?type=电影 = %v, want [2024]", years2)
	}
}
