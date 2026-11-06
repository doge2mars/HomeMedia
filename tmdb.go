package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// #12 TMDB“本周趋势”行：与 douban.go 同款结构（缓存 + 官方 API）。
// key 来源：环境变量 HOME_MEDIA_TMDB_API_KEY 优先，否则复用“即将上映”
// 设置里存的 tmdb_key（meta upcoming_config）。
// 无 key 时该行不出现、不报错（handleTMDBTrending 返回 no_key:true，前端跳过该行）。
// 两个坑：① zh-CN 标题常缺失（未在大陆发行的片中文标题为空）→ 用
// original_title/英文标题兜底，都没有就跳过该条；
// ② 海报走 image.tmdb.org，进服务端图片代理/缓存（前端 posterUrl 已统一走
// /api/image-proxy，不用额外处理）。

const tmdbCacheTTL = 6 * time.Hour

// tmdbBaseURL 供测试替换为 httptest 假 TMDB 服务。
var tmdbBaseURL = "https://api.themoviedb.org/3"

var (
	tmdbCacheMu sync.Mutex
	tmdbCache   = map[string]tmdbCacheEntry{}
)

type tmdbCacheEntry struct {
	items []map[string]any
	at    time.Time
}

func getTMDBKey() string {
	if k := strings.TrimSpace(getenv("HOME_MEDIA_TMDB_API_KEY", "")); k != "" {
		return k
	}
	return getUpcomingConfig().TMDBKey
}

type tmdbTrendingResp struct {
	Results []struct {
		Title         string  `json:"title"`
		Name          string  `json:"name"`
		OriginalTitle string  `json:"original_title"`
		OriginalName  string  `json:"original_name"`
		PosterPath    string  `json:"poster_path"`
		VoteAverage   float64 `json:"vote_average"`
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		MediaType     string  `json:"media_type"`
	} `json:"results"`
}

// fetchTMDBTrendingOne：拉 movie 或 tv 的本周趋势，返回卡片同款 map
// （title/poster/year/region/genre/type/rating），可直接喂前端 card()。
func fetchTMDBTrendingOne(mediaType string, limit int) ([]map[string]any, error) {
	key := getTMDBKey()
	if key == "" {
		return nil, fmt.Errorf("no tmdb key")
	}
	u := fmt.Sprintf("%s/trending/%s/week?api_key=%s&language=zh-CN&page=1", tmdbBaseURL, mediaType, url.QueryEscape(key))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tmdb upstream %d", resp.StatusCode)
	}
	var data tmdbTrendingResp
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	typeName := "电影"
	if mediaType == "tv" {
		typeName = "剧集"
	}
	for _, r := range data.Results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = strings.TrimSpace(r.Name)
		}
		if title == "" {
			// zh-CN 标题缺失 → 英文原名兜底
			title = strings.TrimSpace(r.OriginalTitle)
			if title == "" {
				title = strings.TrimSpace(r.OriginalName)
			}
		}
		if title == "" {
			continue // 中英文标题都没有，跳过不展示
		}
		poster := ""
		if r.PosterPath != "" {
			poster = "https://image.tmdb.org/t/p/w500" + r.PosterPath
		}
		year := ""
		if d := r.ReleaseDate; len(d) >= 4 {
			year = d[:4]
		} else if d := r.FirstAirDate; len(d) >= 4 {
			year = d[:4]
		}
		rating := ""
		if r.VoteAverage > 0 {
			rating = strconv.FormatFloat(r.VoteAverage, 'f', 1, 64)
		}
		out = append(out, map[string]any{
			"title":  title,
			"poster": poster,
			"year":   year,
			"region": "",
			"genre":  "",
			"type":   typeName,
			"rating": rating,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fetchTMDBTrending：type=movie|tv|all。all 时电影/剧集各半、交错排列。
func fetchTMDBTrending(mediaType string, limit int) ([]map[string]any, error) {
	if mediaType == "movie" || mediaType == "tv" {
		return fetchTMDBTrendingOne(mediaType, limit)
	}
	half := (limit + 1) / 2
	movies, err1 := fetchTMDBTrendingOne("movie", half)
	tvs, err2 := fetchTMDBTrendingOne("tv", limit-half)
	if err1 != nil && err2 != nil {
		return nil, err1
	}
	out := []map[string]any{}
	n := half
	if len(movies) < n {
		n = len(movies)
	}
	if len(tvs) < n {
		n = len(tvs)
	}
	for i := 0; i < n; i++ {
		out = append(out, movies[i], tvs[i])
	}
	// 余量（某一方多出的）依次补上
	for i := n; i < len(movies); i++ {
		out = append(out, movies[i])
	}
	for i := n; i < len(tvs); i++ {
		out = append(out, tvs[i])
	}
	return out, nil
}

func handleTMDBTrending(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	if typ != "movie" && typ != "tv" && typ != "all" {
		typ = "all"
	}
	limit := 12
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n >= 1 && n <= 50 {
		limit = n
	}
	if getTMDBKey() == "" {
		// 无 key：该行不出现、不报错
		writeJSON(w, 200, map[string]any{"items": []map[string]any{}, "no_key": true})
		return
	}
	ckey := typ + "|" + strconv.Itoa(limit)
	tmdbCacheMu.Lock()
	ce, hit := tmdbCache[ckey]
	tmdbCacheMu.Unlock()
	if hit && time.Since(ce.at) < tmdbCacheTTL {
		writeJSON(w, 200, map[string]any{"items": ce.items, "source": "tmdb"})
		return
	}
	items, err := fetchTMDBTrending(typ, limit)
	if err != nil {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{}, "error": err.Error()})
		return
	}
	tmdbCacheMu.Lock()
	tmdbCache[ckey] = tmdbCacheEntry{items: items, at: time.Now()}
	tmdbCacheMu.Unlock()
	writeJSON(w, 200, map[string]any{"items": items, "source": "tmdb"})
}
