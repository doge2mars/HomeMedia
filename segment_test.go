package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func segFiles(t *testing.T, dir string) (ts, tmp int) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, "segment-cache"))
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".ts"):
			ts++
		case strings.HasSuffix(e.Name(), ".tmp"):
			tmp++
		}
	}
	return
}

func TestSegmentProxy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME_MEDIA_DATA", dir)

	good := bytes.Repeat([]byte{0x47, 0x00, 0x11, 0x10}, 5000)
	var okHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.ts", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&okHits, 1)
		time.Sleep(150 * time.Millisecond) // 让并发请求有机会重叠
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/heal.ts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(good)
	})
	mux.HandleFunc("/404.ts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2048))
	})
	mux.HandleFunc("/html.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>blocked</html>"))
	})
	mux.HandleFunc("/disguised.ts", func(w http.ResponseWriter, r *http.Request) {
		// 200 + 非 html content-type，但内容是错误页
		_, _ = w.Write([]byte("<!DOCTYPE html><html>hotlink denied</html>"))
	})
	mux.HandleFunc("/trunc.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write(good[:100]) // 声明 100000 字节却只发 100 字节
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/proxy/segment?url="+url.QueryEscape(srv.URL+path), nil)
		handleProxySegment(rr, req)
		return rr
	}

	// 1) 坏响应一律 502 且不落盘
	for _, p := range []string{"/404.ts", "/html.ts", "/disguised.ts", "/trunc.ts"} {
		rr := get(p)
		if rr.Code != 502 {
			t.Errorf("%s: want 502, got %d", p, rr.Code)
		}
	}
	if ts, tmp := segFiles(t, dir); ts != 0 || tmp != 0 {
		t.Errorf("bad responses must not be cached: .ts=%d .tmp=%d", ts, tmp)
	}

	// 2) 同一分片并发请求只打一次上游，且都拿到完整内容
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := get("/ok.ts")
			if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), good) {
				t.Errorf("concurrent get: code=%d len=%d", rr.Code, rr.Body.Len())
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&okHits); n != 1 {
		t.Errorf("upstream hit %d times for concurrent requests, want 1", n)
	}

	// 3) 之后命中缓存，不再请求上游
	rr := get("/ok.ts")
	if rr.Header().Get("X-Cache") != "hit" || !bytes.Equal(rr.Body.Bytes(), good) {
		t.Errorf("expected cache hit with full body, got X-Cache=%q len=%d", rr.Header().Get("X-Cache"), rr.Body.Len())
	}
	if n := atomic.LoadInt32(&okHits); n != 1 {
		t.Errorf("cache hit still hit upstream: %d", n)
	}
	if ts, tmp := segFiles(t, dir); ts != 1 || tmp != 0 {
		t.Errorf("want exactly 1 cached .ts and no .tmp, got .ts=%d .tmp=%d", ts, tmp)
	}

	// 4) 旧版本遗留的坏缓存会被自动识别、删除并重新拉取
	healURL := srv.URL + "/heal.ts"
	poisoned := segmentCachePath(healURL)
	_ = os.MkdirAll(filepath.Dir(poisoned), 0o755)
	_ = os.WriteFile(poisoned, []byte("<html>old poisoned cache</html>"), 0o644)
	rr = get("/heal.ts")
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), good) {
		t.Errorf("poisoned cache not healed: code=%d len=%d", rr.Code, rr.Body.Len())
	}
}

func TestProxyM3U8UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()
	rr := httptest.NewRecorder()
	// fresh=1 跳过 cacheGet（它依赖数据库），只验证上游非 2xx 的处理
	req := httptest.NewRequest("GET", "/api/proxy/m3u8?fresh=1&url="+url.QueryEscape(srv.URL+"/a.m3u8"), nil)
	handleProxyM3U8(rr, req)
	if rr.Code != 502 {
		t.Errorf("upstream 404 m3u8: want 502, got %d", rr.Code)
	}
}
