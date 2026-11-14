package main

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var restrictedKeywords = []string{
	"福利", "伦理", "三级", "理论", "里番", "擦边", "写真", "两性",
	"成人", "情色", "情欲", "大尺度", "热舞", "诱惑", "风月", "艳情",
	"无码", "有码", "R18", "18+",
}

// isRestrictedContent：判断条目是否属于受 4 位 PIN 锁保护的内容。
// 覆盖两类：1) 粗分类落入“其他”的杂项；2) 虽因带“片”字归入“电影”等分类、但细分类或题材含福利/伦理/擦边/情色等敏感词的条目（如“福利片”）。
func isRestrictedContent(typ, genre, kind string) bool {
	if kind == "其他" || isAdultContent(typ, genre) {
		return true
	}
	s := typ + " " + genre
	for _, kw := range restrictedKeywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func validPinCode(pin string) bool {
	if len(pin) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}
	return true
}

func getRestrictedPIN() string {
	if v, ok := metaGet("restricted_pin"); ok {
		v = strings.TrimSpace(v)
		if validPinCode(v) {
			return v
		}
	}
	return "1234"
}

func setRestrictedPIN(pin string) error {
	pin = strings.TrimSpace(pin)
	if !validPinCode(pin) {
		return http.ErrNotSupported
	}
	return metaSet("restricted_pin", pin)
}

func pinUnlockToken(username string) string {
	return sign("pin_unlock:" + username + ":" + getRestrictedPIN())
}

func isRestrictedUnlocked(r *http.Request) bool {
	if r == nil {
		return false
	}
	c, err := r.Cookie("hm_pin_unlock")
	if err != nil || c.Value == "" {
		return false
	}
	want := pinUnlockToken(currentUser(r))
	return hmac.Equal([]byte(c.Value), []byte(want))
}

func handleRestrictedStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"unlocked": isRestrictedUnlocked(r)})
}

func handleRestrictedUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	ip := clientIP(r)
	if ok, retryAfter := pinLimiter.allow(ip); !ok {
		writeJSON(w, 429, map[string]any{"error": fmt.Sprintf("尝试次数过多，请 %d 秒后再试", retryAfter)})
		return
	}
	var b struct {
		Pin string `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	if strings.TrimSpace(b.Pin) != getRestrictedPIN() {
		pinLimiter.recordFailure(ip)
		writeJSON(w, 403, map[string]string{"error": "PIN 码错误"})
		return
	}
	pinLimiter.reset(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     "hm_pin_unlock",
		Value:    pinUnlockToken(currentUser(r)),
		Path:     "/",
		MaxAge:   2 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "unlocked": true})
}

func handleRestrictedLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hm_pin_unlock",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "unlocked": false})
}

func handleRestrictedPinSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method"})
		return
	}
	var b struct {
		Pin string `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	pin := strings.TrimSpace(b.Pin)
	if !validPinCode(pin) {
		writeJSON(w, 400, map[string]string{"error": "PIN 必须是 4 位数字"})
		return
	}
	if err := setRestrictedPIN(pin); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
