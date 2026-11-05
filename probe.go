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
	Error      string  `json:"error,omitempty"`
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
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i, c := range b.Candidates {
		i, c := i, c
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
			kbs, res, err := probePlayURL(ctx, c.URL)
			pr := probeResult{Source: c.Source, SourceName: c.SourceName, SpeedKBs: kbs, Resolution: res}
			if err != nil {
				pr.Error = err.Error()
			} else {
				pr.OK = true
			}
			out[i] = pr
		}()
	}
	wg.Wait()
	// OK 在前按速度降序，失败的保持原相对顺序垫底
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return out[i].OK
		}
		return out[i].SpeedKBs > out[j].SpeedKBs
	})
	writeJSON(w, 200, map[string]any{"results": out})
}

// probePlayURL：取 m3u8 → 跟第一个变体（master 列表时）→ 下完整首分片算速度。
// 只有“完整下完 + 不是错误页 + 长度对得上”的分片才进分片缓存；超时/截断一律丢弃，绝不把半截写进缓存。
func probePlayURL(ctx context.Context, raw string) (float64, string, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, "GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, "", fmt.Errorf("upstream %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return 0, "", err
	}
	text := string(body)
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
			return 0, "", err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != 200 {
			return 0, "", fmt.Errorf("upstream %d", resp2.StatusCode)
		}
		b2, err := io.ReadAll(io.LimitReader(resp2.Body, 2<<20))
		if err != nil {
			return 0, "", err
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
		return 0, res, fmt.Errorf("no segment")
	}
	segURL = resolveURL(raw, segURL)
	start := time.Now()
	req3, _ := http.NewRequestWithContext(ctx2, "GET", segURL, nil)
	req3.Header.Set("User-Agent", "Mozilla/5.0 HomeMedia/0.1")
	resp3, err := httpClient.Do(req3)
	if err != nil {
		return 0, res, err
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		return 0, res, fmt.Errorf("upstream %d", resp3.StatusCode)
	}
	seg, err := io.ReadAll(io.LimitReader(resp3.Body, maxSegmentBytes))
	if err != nil {
		return 0, res, err
	}
	if cl := resp3.ContentLength; cl > 0 && int64(len(seg)) != cl {
		return 0, res, fmt.Errorf("truncated segment")
	}
	if looksLikeErrorPage(seg) {
		return 0, res, fmt.Errorf("error page")
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	// 完整分片落盘：播放时同一分片命中磁盘缓存，从局域网起手
	_ = cacheSegmentBytes(segURL, seg)
	return float64(len(seg)) / 1024 / elapsed, res, nil
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
