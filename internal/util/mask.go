// Package util 通用工具（零依赖，server/pushkit 共用，避免循环依赖——pushkit 不 import server）。
package util

// Mask token 脱敏（日志用）：≤8 字符返 ***，否则首 4 + "..." + 末 4。
// server（register log）+ pushkit（Send log）共用，TD-2 批 DRY（跨审 E P2：server.go + pushkit.go 双份重复）。
func Mask(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
