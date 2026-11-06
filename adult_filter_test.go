package main

import "testing"

// ---------- 成人内容清理（2026-11-06）：同步拦截 ----------

func TestIsAdultContent(t *testing.T) {
	cases := []struct {
		typ, genre string
		want       bool
	}{
		{"伦理片", "", true},
		{"伦理", "", true},
		{"韩国伦理", "", true},
		{"日本伦理", "", true},
		{"西方伦理", "", true},
		{"港台三级", "", true},
		{"三级伦理", "", true},
		{"福利", "", true},
		{"理论片", "", true},
		{"里番动漫", "", true},
		{"擦边短剧", "", true},
		{"写真热舞", "", true},
		{"写真", "", true},
		{"两性课堂", "", true},
		{"理论片", "剧情,情色,韩国伦理", true}, // type 含理论即拦（genre 也含情色）
		{"爱情片", "剧情,爱情,情色", true},
		{"惊悚片", "悬疑,惊悚,情色,犯罪", true},
		{"爱情片", "剧情,爱情", false},
		{"喜剧片", "", false},
		{"科幻片", "科幻,美国,动作,犯罪,2018,超级武器,惊悚,剧情", false}, // genre 里的 2018 不是成人标记
		{"动作片", "", false},
	}
	for _, c := range cases {
		if got := isAdultContent(c.typ, c.genre); got != c.want {
			t.Errorf("isAdultContent(%q, %q) = %v, want %v", c.typ, c.genre, got, c.want)
		}
	}
}

// TestAdultFilterSyncGate：走一遍同步入库的门（与 syncCatalogWith 内循环同款判断），
// 种子一条伦理片、一条三级片、一条福利、一条理论片、一条里番、一条擦边短剧、一条写真、一条两性、一条 genre 含情色的爱情片、一条正常片，
// 断言库里只剩正常片。
func TestAdultFilterSyncGate(t *testing.T) {
	db := openTestDB(t)
	items := []catalogItem{
		{title: "伦理样片", typ: "伦理片", genre: "剧情,伦理"},
		{title: "三级样片", typ: "港台三级", genre: "剧情"},
		{title: "福利样片", typ: "福利", genre: ""},
		{title: "理论样片", typ: "理论片", genre: "剧情"},
		{title: "里番样片", typ: "里番动漫", genre: "动画"},
		{title: "擦边样片", typ: "擦边短剧", genre: "剧情"},
		{title: "写真样片", typ: "写真热舞", genre: ""},
		{title: "两性样片", typ: "两性课堂", genre: ""},
		{title: "情色样片", typ: "爱情片", genre: "剧情,爱情,情色"},
		{title: "正常样片", typ: "爱情片", genre: "剧情,爱情"},
	}
	for _, it := range items {
		if isSportsKind(it.typ, it.genre) || isAdultContent(it.typ, it.genre) {
			continue
		}
		if _, err := db.Exec(upsertCatalogSQL,
			it.title, it.poster, it.year, it.region, it.genre, it.typ, "", it.rating, it.synopsis, 0, kindOf(it.typ, it.genre), it.hitsWeek); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`SELECT title FROM catalog ORDER BY title`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			t.Fatal(err)
		}
		got = append(got, title)
	}
	if len(got) != 1 || got[0] != "正常样片" {
		t.Errorf("catalog after gate = %v, want [正常样片]", got)
	}
}
