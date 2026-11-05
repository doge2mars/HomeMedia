package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func TestProbePlayURLCapsSegment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME_MEDIA_DATA", dir)

	// 20MB 分片：测速只读前 5MB，探测成功、速度>0，但半截分片绝不进缓存。
	big := make([]byte, 20<<20)
	small := make([]byte, 1<<20)

	mux := http.NewServeMux()
	mux.HandleFunc("/big.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:10,\n/big.ts\n")
	})
	mux.HandleFunc("/big.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.Write(big)
	})
	mux.HandleFunc("/small.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:10,\n/small.ts\n")
	})
	mux.HandleFunc("/small.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(small)))
		w.Write(small)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	speed, _, err := probePlayURL(context.Background(), srv.URL+"/big.m3u8")
	if err != nil {
		t.Fatalf("probe 20MB segment: %v", err)
	}
	if speed <= 0 {
		t.Fatalf("speed = %v, want > 0", speed)
	}
	if _, err := os.Stat(segmentCachePath(srv.URL + "/big.ts")); !os.IsNotExist(err) {
		t.Fatalf("capped segment was cached (want no cache file)")
	}

	// 1MB 完整分片：照常进缓存（正常路径没被破坏）。
	if _, _, err := probePlayURL(context.Background(), srv.URL+"/small.m3u8"); err != nil {
		t.Fatalf("probe 1MB segment: %v", err)
	}
	if _, err := os.Stat(segmentCachePath(srv.URL + "/small.ts")); err != nil {
		t.Fatalf("complete segment was not cached: %v", err)
	}
}
