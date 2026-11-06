package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bangumi 每日放送（第五批：动漫分类“每日放送”）：
// 上游 https://api.bgm.tv/calendar（无需 key），返回按星期分组的新番日历；
// 12 小时内存缓存；上游失败返回 502，前端回退到目录库动漫。
// 条目形状与 /api/douban 对齐（title/poster/rating/year/type），前端 card() 直接能用。

const bangumiCalendarTTL = 12 * time.Hour

// bangumiCalendarAPI：可覆盖（测试用假上游）。
var bangumiCalendarAPI = "https://api.bgm.tv/calendar"

var (
	bangumiCacheMu sync.Mutex
	bangumiCache   *bangumiCacheEntry
)

type bangumiCacheEntry struct {
	weekdays []map[string]any
	at       time.Time
}

type bangumiWeekday struct {
	ID int    `json:"id"`
	CN string `json:"cn"`
}

type bangumiSubject struct {
	Name    string `json:"name"`
	NameCN  string `json:"name_cn"`
	AirDate string `json:"air_date"`
	Rating  struct {
		Score float64 `json:"score"`
	} `json:"rating"`
	Images struct {
		Large  string `json:"large"`
		Common string `json:"common"`
		Medium string `json:"medium"`
	} `json:"images"`
}

type bangumiDay struct {
	Weekday bangumiWeekday   `json:"weekday"`
	Items   []bangumiSubject `json:"items"`
}

// parseBangumiCalendar：把上游日历解析成前端直接能用的 weekday 分组。
func parseBangumiCalendar(body []byte) ([]map[string]any, error) {
	var days []bangumiDay
	if err := json.Unmarshal(body, &days); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(days))
	for _, d := range days {
		items := make([]map[string]any, 0, len(d.Items))
		for _, s := range d.Items {
			title := strings.TrimSpace(s.NameCN)
			if title == "" {
				title = strings.TrimSpace(s.Name)
			}
			if title == "" {
				continue
			}
			poster := s.Images.Large
			if poster == "" {
				poster = s.Images.Common
			}
			if poster == "" {
				poster = s.Images.Medium
			}
			year := ""
			if len(s.AirDate) >= 4 {
				year = s.AirDate[:4]
			}
			rating := ""
			if s.Rating.Score > 0 {
				rating = strconv.FormatFloat(s.Rating.Score, 'f', 1, 64)
			}
			items = append(items, map[string]any{"title": title, "poster": poster, "rating": rating, "year": year, "type": "动漫"})
		}
		name := d.Weekday.CN
		if name == "" {
			name = "周" + strconv.Itoa(d.Weekday.ID)
		}
		out = append(out, map[string]any{"id": d.Weekday.ID, "name": name, "items": items})
	}
	return out, nil
}

func fetchBangumiCalendar() ([]map[string]any, error) {
	bangumiCacheMu.Lock()
	if bangumiCache != nil && time.Since(bangumiCache.at) < bangumiCalendarTTL {
		wd := bangumiCache.weekdays
		bangumiCacheMu.Unlock()
		return wd, nil
	}
	bangumiCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", bangumiCalendarAPI, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bangumi upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	weekdays, err := parseBangumiCalendar(body)
	if err != nil {
		return nil, err
	}
	bangumiCacheMu.Lock()
	bangumiCache = &bangumiCacheEntry{weekdays: weekdays, at: time.Now()}
	bangumiCacheMu.Unlock()
	return weekdays, nil
}

// handleBangumiCalendar：GET /api/bangumi/calendar（登录后可读，与 /api/douban 同款）。
func handleBangumiCalendar(w http.ResponseWriter, r *http.Request) {
	weekdays, err := fetchBangumiCalendar()
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"weekdays": weekdays})
}
