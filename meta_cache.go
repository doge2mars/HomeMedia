package main

import (
	"database/sql"
	"sync"
	"time"
)

// #16 配置缓存：meta 表读 5 分钟内存缓存；读库失败回退旧缓存（哪怕过期），
// 读库失败且无旧缓存时返回 ("", false)，调用方按“未配置”处理。
// 写配置时同步更新缓存，改完立即生效，不用等 TTL。
// 覆盖的 key：sync_config、douban_proxy、image_proxies、upcoming_config、
// last_catalog_sync、last_upcoming_sync。

type metaCacheEntry struct {
	val string
	ok  bool
	at  time.Time
}

var (
	metaCacheMu sync.RWMutex
	metaCache   = map[string]metaCacheEntry{}
)

const metaCacheTTL = 5 * time.Minute

func metaGet(k string) (string, bool) {
	metaCacheMu.RLock()
	e, hit := metaCache[k]
	metaCacheMu.RUnlock()
	if hit && time.Since(e.at) < metaCacheTTL {
		return e.val, e.ok
	}
	if db == nil {
		return "", false
	}
	var v string
	err := db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err != nil {
		if hit {
			return e.val, e.ok // 读库失败：回退旧缓存
		}
		if err == sql.ErrNoRows || v == "" {
			// key 不存在也缓存（避免热点 key 反复查库），ok=false
			metaCacheMu.Lock()
			metaCache[k] = metaCacheEntry{val: "", ok: false, at: time.Now()}
			metaCacheMu.Unlock()
		}
		return "", false
	}
	if v == "" {
		metaCacheMu.Lock()
		metaCache[k] = metaCacheEntry{val: "", ok: false, at: time.Now()}
		metaCacheMu.Unlock()
		return "", false
	}
	metaCacheMu.Lock()
	metaCache[k] = metaCacheEntry{val: v, ok: true, at: time.Now()}
	metaCacheMu.Unlock()
	return v, true
}

// metaSet：写 meta 并同步更新缓存（配置改完立即生效）。
func metaSet(k, v string) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(`INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v); err != nil {
		return err
	}
	metaCacheMu.Lock()
	metaCache[k] = metaCacheEntry{val: v, ok: v != "", at: time.Now()}
	metaCacheMu.Unlock()
	return nil
}

// metaInvalidate：删某 key 的缓存（下次读重新查库）。
func metaInvalidate(k string) {
	metaCacheMu.Lock()
	delete(metaCache, k)
	metaCacheMu.Unlock()
}

// metaCacheClear：清空全部配置缓存（/api/cache/clear 时调用）。
func metaCacheClear() {
	metaCacheMu.Lock()
	metaCache = map[string]metaCacheEntry{}
	metaCacheMu.Unlock()
}
