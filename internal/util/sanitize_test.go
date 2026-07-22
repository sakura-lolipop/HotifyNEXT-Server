package util

import (
	"strings"
	"testing"
)

func TestSanitizeActionURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // "" 表拒绝
	}{
		// 放行：http/https
		{"http", "http://example.com", "http://example.com"},
		{"https with path query fragment", "https://example.com/a/b?x=1&y=2#frag", "https://example.com/a/b?x=1&y=2#frag"},
		{"http upper scheme lowercased compare", "HTTP://Example.COM", "HTTP://Example.COM"},

		// 放行：自定义 app scheme
		{"app scheme hotify", "hotify://open/msg/123", "hotify://open/msg/123"},
		{"app scheme weixin", "weixin://dl/business", "weixin://dl/business"},

		// 拒绝：危险 scheme（XSS / 文件泄露 / 非跳转）
		{"javascript xss", "javascript:alert(1)", ""},
		{"file leak", "file:///etc/passwd", ""},
		{"content leak", "content://provider/xxx", ""},
		{"data url html", "data:text/html,<script>", ""},
		{"vbscript", "vbscript:msgbox", ""},
		{"ws not navigation", "ws://server/stream", ""},
		{"wss not navigation", "wss://server/stream", ""},

		// 拒绝：无 scheme（相对/裸主机/协议相对）
		{"empty", "", ""},
		{"relative path", "/relative/path", ""},
		{"protocol relative", "//host/path", ""},
		{"bare host no scheme", "example.com", ""},
		{"bare host with path", "example.com/a/b", ""},

		// 拒绝：http 无 host
		{"http no host opaque", "http:///path", ""},

		// 拒绝：超长（>2048）
		{"too long", "http://example.com/" + strings.Repeat("a", 2048), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeActionURL(tt.in)
			if got != tt.want {
				t.Errorf("SanitizeActionURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
