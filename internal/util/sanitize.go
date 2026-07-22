// TD-12（CP4）：点击跳转 URL 协议白名单。
//
// SanitizeActionURL 用于 PushKit clickAction.data / bark url / 原生 url 喂端侧点击跳转前。
// 防 javascript:（webview XSS）/ file: content:（本地文件泄露）/ data: blob: about: vbscript:
// / ws: wss:（非跳转语义）/ 无 scheme 相对路径（钓鱼）。
//
// 放行：http/https（须有 host）+ 自定义 app scheme（如 hotify:// weixin://，url.Parse 已保
// scheme 符合 RFC3986 [a-zA-Z][a-zA-Z0-9+.-]*）。拒绝一律返 ""（空串）——调用方据此「不放
// URL」（clickAction.data 不塞 url 字段），端侧不跳转（保守：宁可不跳不让跳到危险处）。
//
// 长度上限 2048（HTTP URL 常见上限；且 PushKit 通知 clickAction.data 在 4KB 通知上限内，
// 超长 URL 无意义且可能打爆载荷）。
//
// 鸿蒙 clickAction 的 URL 处理语义 CP4 真机联调时验（pushkit-delivery.md 只实证 clickAction.data
// 平铺投递，未实证 url 字段是否被系统 webview 打开）——先白名单保守放 http/https + app scheme。
package util

import (
	"net/url"
	"strings"
)

// maxActionURLLen 点击跳转 URL 长度上限（TD-12）。HTTP URL 常见上限 2048；超长无意义且占载荷。
const maxActionURLLen = 2048

// dangerousSchemes 点击跳转禁用 scheme 黑名单（TD-12）。端侧 webview/系统层有 XSS/泄露/非跳转语义。
var dangerousSchemes = map[string]bool{
	"javascript": true, // webview XSS
	"file":       true, // 本地文件泄露
	"content":    true, // content provider 文件泄露
	"data":       true, // data: URL 可载 HTML（XSS）
	"blob":       true, // blob URL
	"about":      true, // about:blank 等
	"vbscript":   true, // 脚本注入
	"ws":         true, // WebSocket 非跳转
	"wss":        true, // WebSocket 非跳转
}

// SanitizeActionURL 校验点击跳转 URL（TD-12，CP4）。放行返原 URL，拒绝返空串。
// 放行/拒绝规则见文件头注释。
func SanitizeActionURL(rawURL string) string {
	if len(rawURL) == 0 || len(rawURL) > maxActionURLLen {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" {
		return "" // 无 scheme（相对/裸主机/协议相对 URL）或解析失败 → 拒
	}
	scheme := strings.ToLower(parsed.Scheme)
	if dangerousSchemes[scheme] {
		return "" // 危险 scheme 黑名单
	}
	if scheme == "http" || scheme == "https" {
		if parsed.Host == "" {
			return "" // http/https 须有 host（防 http:opaque 之类无 host）
		}
		return rawURL
	}
	// 到这里的 scheme 是 url.Parse 认可的 RFC3986 合法 scheme（[a-zA-Z][a-zA-Z0-9+.-]*），
	// 且非危险黑名单 → 当自定义 app scheme 放行（hotify:// weixin:// myapp:// …）。
	return rawURL
}
