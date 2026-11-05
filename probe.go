package main

// 优选播放探测：详情页已聚合好的某集候选 m3u8 地址发过来，后端并行探测，返回按实测速度排好的名次。
// 与详情页后台 SSE 测速的区别：SSE 是“测完再告诉你”、要你手动点播最快源；这里是“一键直达”——
// 点优选播放 → 测速遮罩 → 自动播最快，失败走现有 failover 顺延。
// 探测只测将要播的那一集；完整下完的首分片直接进分片缓存，播放时同一分片走局域网缓存起手，
// 探测花的流量不浪费。单源 5 秒封顶，超时判负不阻塞；探不通的源只垫底、不剔除（播时 failover 还能再试）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type probeCandidate struct {
	Source     string `json:"source"`
	SourceName string `json:"source_name"`
	URL        string `json:"url"`
}

type probeResult struct {
	Source     string  `json:"source"`
	SourceName string  `json:"source_name"`
	OK         bool    `json:"ok"`
	SpeedKBs   float64 `json:"speed_kbs,omitempty"`
	Resolution string  `json:"resolution,omitempty"`
	LatencyMs  int64   `json:"latency_ms,omitempty"`
	Score      float64 `json:"score,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// ---- #3 加权评分（演进文档 §3.1，复用 LunaTV 的 40/40/20）：
// 分辨率 40% + 加载速度 40% + 首包延迟 20%。失败源不评分（score 0，排最后）。 ----

// scoreProbe：三项加权总分（0–1）。
func scoreProbe(resolution string, speedKBs float64, latencyMs int64) float64 {
	return 0.4*resolutionScore(resolution) + 0.4*speedScore(speedKBs) + 0.2*latencyScore(latencyMs)
}

// resolutionScore：按像素归一化，1080p=1.0（4K 也封顶 1.0）；无分辨率信息按 0.5 中性计。
func resolutionScore(res string) float64 {
	var w, h int
	if _, err := fmt.Sscanf(res, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0.5
	}
	s := float64(w*h) / 2073600.0
	if s > 1 {
		s = 1
	}
	return s
}

// speedScore：5 MB/s 封顶 1.0（1080p 流畅约需 1–2 MB/s，5 MB/s 已绰绰有余）。
func speedScore(kbs float64) float64 {
	s := kbs / 5120.0
	if s > 1 {
		s = 1
	}
	if s < 0 {
		s = 0
	}
	return s
}

// latencyScore：首包延迟 0ms=1.0，2000ms 以上=0。
func latencyScore(ms int64) float64 {
	if ms < 0 {
		ms = 0
	}
	if ms >= 2000 {
		return 0
	}
	return 1 - float64(ms)/2000.0
}

// ---- 探针结果缓存：同一候选 URL 5 分钟内不再重复探测（进页测速 2 秒目标主要靠它；
// 重复进同一标题的详情页直接命中）。失败结果只记 2 分钟，让抽风的源更快重测。
// 缓存只存探测指标（速度/分辨率/延迟/分数/错误），不存源名——同一 URL 万一被
// 两个源共用，也不会把 A 源的名字算到 B 源头上。 ----
type probeMetrics struct {
	OK         bool
	SpeedKBs   float64
	Resolution string
	LatencyMs  int64
	Score      float64
	Error      string
}

var (
	probeCacheMu  sync.Mutex
	probeCache    = map[string]probeCacheEntry{}
	probeCacheTTL = 5 * time.Minute
	probeCacheFailTTL = 2 * time.Minute
)

type probeCacheEntry struct {
	m   probeMetrics
	exp time.Time
}

func probeCacheGet(rawURL string) (probeMetrics, bool) {
	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	e, ok := probeCache[rawURL]
	if !ok {
		return probeMetrics{}, false
	}
	if time.Now().After(e.exp) {
		delete(probeCache, rawURL)
		return probeMetrics{}, false
	}
	return e.m, true
}

func probeCacheSet(rawURL string, m probeMetrics) {
	ttl := probeCacheFailTTL
	if m.OK {
		ttl = probeCacheTTL
	}
	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	probeCache[rawURL] = probeCacheEntry{m: m, exp: time.Now().Add(ttl)}
}

func resetProbeCacheForTest() {
	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	probeCache = map[string]probeCacheEntry{}
}

func handlePlayBest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Candidates []probeCandidate `json:"candidates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Candidates) == 0 {
		writeJSON(w, 400, map[string]string{"error": "missing candidates"})
		return
	}
	if len(b.Candidates) > 30 {
		b.Candidates = b.Candidates[:30]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	out := make([]probeResult, len(b.Candidates))
	sem := make(chan struct{}, 4) // 固定 4 并发（演进文档 §3.1 #3）
	var wg sync.WaitGroup
	for i, c := range b.Candidates {
		i, c := i, c
		// 缓存命中直接填，不占并发槽
		if m, ok := probeCacheGet(c.URL); ok {
			out[i] = probeResult{Source: c.Source, SourceName: c.SourceName, OK: m.OK,
				SpeedKBs: m.SpeedKBs, Resolution: m.Resolution, LatencyMs: m.LatencyMs,
				Score: m.Score, Error: m.Error}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = probeResult{Source: c.Source, SourceName: c.SourceName, Error: "timeout"}
				return
			}
			kbs, res, lat, err := probePlayURL(ctx, c.URL)
			m := probeMetrics{SpeedKBs: kbs, Resolution: res, LatencyMs: lat}
			if err != nil {
				m.Error = err.Error()
			} else {
				m.OK = true
				m.Score = scoreProbe(res, kbs, lat)
			}
			probeCacheSet(c.URL, m)
			out[i] = probeResult{Source: c.Source, SourceName: c.SourceName, OK: m.OK,
				SpeedKBs: m.SpeedKBs, Resolution: m.Resolution, LatencyMs: m.LatencyMs,
				Score: m.Score, Error: m.Error}
		}()
	}
	wg.Wait()
	// OK 在前按加权分降序，失败的（score 0）保持原相对顺序垫底；全失败时客户端
	// 回退第一个源（见 web/index.html playBestProbe / directPlay）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		return out[i].Score > out[j].Score
	})
	writeJSON(w, 200, map[string]any{"results": out})
}

// 测速只读首分片前 5MB（足够算速度）；完整拉取 4K 大分片会占满家庭宽带。
const probeSegmentMaxBytes = 5 << 20

// probePlayURL：取 m3u8 → 跟第一个变体（master 列表时）→ 只读首分片前 5MB 算速度。
// 只有“完整下完 + 不是错误页 + 长度对得上”的分片才进分片缓存；超时/截断/被 5MB 上限截断的
// 一律不进缓存，绝不把半截写进缓存。
// 返回：速度 KB/s、最高分辨率、首包延迟 ms（第一次取播放列表的 TTFB，供加权评分用）。
func probePlayURL(ctx context.Context, raw string) (float64, string, int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fetchStart := time.Now()
	req, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", 0, err
	}
	latencyMs := time.Since(fetchStart).Milliseconds()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, "", latencyMs, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, "", latencyMs, err
	}
	text := string(body)
	// 分享页：候选地址是 HTML 落地页时，先取出真 m3u8 再走正常探测；
	// 取不到真链时直接判负，别把 HTML 行当成分片地址去取。
	if !strings.Contains(text, "#EXTM3U") {
		if real := extractShareM3U8(text, raw); real != "" {
			raw = real
			reqS, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
			reqS.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
			respS, err := httpClient.Do(reqS)
			if err != nil {
				return 0, "", latencyMs, err
			}
			defer respS.Body.Close()
			if respS.StatusCode != 200 {
				return 0, "", latencyMs, fmt.Errorf("upstream %d", respS.StatusCode)
			}
			bS, err := io.ReadAll(io.LimitReader(respS.Body, 2<<20))
			if err != nil {
				return 0, "", latencyMs, err
			}
			text = string(bS)
		} else {
			return 0, "", latencyMs, fmt.Errorf("not a playlist")
		}
	}
	res := ""
	if strings.Contains(text, "#EXT-X-STREAM-INF") {
		// master 列表：显示用最大分辨率，实测跟第一个变体（与 speedTestSource 同款做法）
		re := regexp.MustCompile(`RESOLUTION=(\d+x\d+)`)
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if m[1] > res {
				res = m[1]
			}
		}
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "#EXT-X-STREAM-INF") && i+1 < len(lines) {
				u := strings.TrimSpace(lines[i+1])
				if u != "" && !strings.HasPrefix(u, "#") {
					raw = resolveURL(raw, u)
					break
				}
			}
		}
		// 变体 m3u8 再取一次
		req2, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
		req2.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
		resp2, err := httpClient.Do(req2)
		if err != nil {
			return 0, "", latencyMs, err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != 200 {
			return 0, "", latencyMs, fmt.Errorf("upstream %d", resp2.StatusCode)
		}
		b2, err := io.ReadAll(io.LimitReader(resp2.Body, 2<<20))
		if err != nil {
			return 0, "", latencyMs, err
		}
		text = string(b2)
	}
	segURL := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			segURL = t
			break
		}
	}
	if segURL == "" {
		return 0, res, latencyMs, fmt.Errorf("no segment")
	}
	segURL = resolveURL(raw, segURL)
	start := time.Now()
	req3, _ := http.NewRequestWithContext(ctx2, "GET", segURL, nil)
	req3.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp3, err := httpClient.Do(req3)
	if err != nil {
		return 0, res, latencyMs, err
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		return 0, res, latencyMs, fmt.Errorf("upstream %d", resp3.StatusCode)
	}
	seg, err := io.ReadAll(io.LimitReader(resp3.Body, probeSegmentMaxBytes))
	if err != nil {
		return 0, res, latencyMs, err
	}
	// 测速只读前 5MB：4K 高码率分片常 20–30MB，完整拉取会瞬时占满家庭宽带
	// （5 源并发可达上百 MB）；5MB 样本已足够算出稳定速度。被上限截断的
	// 分片不是“截断”，只用来算速度，绝不进分片缓存。
	capped := int64(len(seg)) == probeSegmentMaxBytes && resp3.ContentLength != probeSegmentMaxBytes
	if cl := resp3.ContentLength; cl > 0 && !capped && int64(len(seg)) != cl {
		return 0, res, latencyMs, fmt.Errorf("truncated segment")
	}
	if looksLikeErrorPage(seg) {
		return 0, res, latencyMs, fmt.Errorf("error page")
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	if !capped {
		// 完整分片落盘：播放时同一分片命中磁盘缓存，从局域网起手
		_ = cacheSegmentBytes(segURL, seg)
	}
	return float64(len(seg)) / 1024 / elapsed, res, latencyMs, nil
}

// resolveURL：相对地址按基准 URL 解析，绝对地址原样回
func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http") {
		return ref
	}
	if b, err := url.Parse(base); err == nil {
		if uu, err := b.Parse(ref); err == nil {
			return uu.String()
		}
	}
	return ref
}

// cacheSegmentBytes：完整分片原子落盘（tmp + rename），与 handleProxySegment 的写盘同款做法
func cacheSegmentBytes(segURL string, seg []byte) error {
	cachePath := segmentCachePath(segURL)
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "probe-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(seg); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, cachePath)
}

