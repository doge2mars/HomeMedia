package main

import "testing"

func TestExtractShareM3U8(t *testing.T) {
	cases := []struct {
		html, page, want string
	}{
		{`const url = "https://cdn.example.com/a.m3u8"`, "https://x.com/play/1", "https://cdn.example.com/a.m3u8"},
		{`const url = 'https://cdn.example.com/a.m3u8'`, "https://x.com/play/1", "https://cdn.example.com/a.m3u8"},
		{`var url = "https://cdn.example.com/a.m3u8"`, "https://x.com/play/1", "https://cdn.example.com/a.m3u8"},
		{`let url = 'https://cdn.example.com/a.m3u8'`, "https://x.com/play/1", "https://cdn.example.com/a.m3u8"},
		{`const url = "/play/a.m3u8"`, "https://v.gsuus.com/play/123", "https://v.gsuus.com/play/a.m3u8"},
		{`const url = "https://cdn.example.com/a.mp4"`, "https://x.com/play/1", ""},
		{"no url here", "https://x.com/play/1", ""},
	}
	for _, c := range cases {
		if got := extractShareM3U8(c.html, c.page); got != c.want {
			t.Errorf("extractShareM3U8(%q) = %q, want %q", c.html, got, c.want)
		}
	}
}
