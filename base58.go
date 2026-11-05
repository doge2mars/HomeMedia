package main

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
)

func base64Decode(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// Base58（Bitcoin 字母表，与 bs58 库一致）
// 订阅的真实格式（源码实测，cron/route.ts 与 config_subscription/fetch/route.ts 两处一致）：
//
//	fetch(url) -> response.text() -> bs58.decode(text) -> TextDecoder -> JSON.parse
//
// 也就是订阅内容本身就是 Base58 编码后的 配置 JSON，不是明文 JSON。
// HomeMedia v0.3 只试了明文 JSON，所以 这类订阅必然报 no sources parsed——这是我的错，不是用户贴错地址。
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	// 前导 '1' = 前导零字节
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	n := big.NewInt(0)
	base := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(base58Alphabet, s[i])
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 char %q at %d", s[i], i)
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	decoded := n.Bytes()
	out := make([]byte, zeros+len(decoded))
	copy(out[zeros:], decoded)
	return out, nil
}
