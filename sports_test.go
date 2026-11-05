package main

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestIsSportsKind(t *testing.T) {
	for _, typ := range []string{"足球", "篮球", "斯诺克", "网球", "台球", "NBA", "CBA", "WCBA", "LPL", "西甲", "英超", "体育赛事"} {
		if !isSportsKind(typ, "") {
			t.Errorf("isSportsKind(%q) = false, want true", typ)
		}
	}
	if !isSportsKind("", "西甲赛事") {
		t.Errorf("isSportsKind(genre=西甲赛事) = false, want true")
	}
	for _, typ := range []string{"剧情片", "国产剧", "综艺", "动漫电影", "纪录片"} {
		if isSportsKind(typ, "") {
			t.Errorf("isSportsKind(%q) = true, want false", typ)
		}
	}
	// kindOf 仍把体育归为体育（供删除谓词与接口排除使用）
	if kindOf("足球", "") != "体育" {
		t.Errorf("kindOf(足球) = %q, want 体育", kindOf("足球", ""))
	}
	if kindOf("剧情片", "喜剧") != "电影" {
		t.Errorf("kindOf(剧情片) = %q, want 电影", kindOf("剧情片", "喜剧"))
	}
}

func TestCatalogExcludesSports(t *testing.T) {
	openTestDB(t)
	seedCatalogRow(t, "体育节目A", "pS", "2024", "大陆", "体育", "足球", "7.0", "体育")
	seedCatalogRow(t, "测试电影", "pM", "2024", "大陆", "剧情", "剧情片", "8.0", "电影")

	// 无过滤：体育行不应出现
	req := httptest.NewRequest("GET", "/api/catalog?limit=50", nil)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	items := decodeItems(t, rec)
	if got := titlesOf(items); len(got) != 1 || got[0] != "测试电影" {
		t.Fatalf("unfiltered catalog = %v, want only [测试电影]", got)
	}

	// ?type=体育：应为空
	req2 := httptest.NewRequest("GET", "/api/catalog?type="+url.QueryEscape("体育")+"&limit=50", nil)
	rec2 := httptest.NewRecorder()
	handleCatalog(rec2, req2)
	if got := decodeItems(t, rec2); len(got) != 0 {
		t.Fatalf("type=体育 returned %v, want empty", titlesOf(got))
	}

	// 相关推荐：以体育标题查，应为空
	req3 := httptest.NewRequest("GET", "/api/catalog/related?title="+url.QueryEscape("体育节目A"), nil)
	rec3 := httptest.NewRecorder()
	handleCatalogRelated(rec3, req3)
	if got := decodeItems(t, rec3); len(got) != 0 {
		t.Fatalf("related for sports title = %v, want empty", titlesOf(got))
	}
}
