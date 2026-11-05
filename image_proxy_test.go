package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeImageProxies(t *testing.T) {
	// 假图站：返回一小张 JPEG
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	}))
	defer img.Close()
	// 配一个坏代理（连不上），直连应成功、代理应失败
	t.Setenv("HOME_MEDIA_IMAGE_PROXIES", "http://127.0.0.1:1")

	results := probeImageProxies(img.URL + "/p.jpg")
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (direct + 1 bad proxy)", len(results))
	}
	if !results[0].OK || results[0].Name != "直连" {
		t.Errorf("direct = %+v, want ok", results[0])
	}
	if results[1].OK {
		t.Errorf("bad proxy = %+v, want failure", results[1])
	}
	if results[1].Error == "" {
		t.Errorf("bad proxy error empty, want a reason")
	}
}

func TestHandleImageProxiesTestMethod(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/image-proxies/test", nil)
	rec := httptest.NewRecorder()
	handleImageProxiesTest(rec, req)
	if rec.Code != 405 {
		t.Fatalf("GET status %d, want 405", rec.Code)
	}
}
