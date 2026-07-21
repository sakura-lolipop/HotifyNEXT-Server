// key1 鉴权中间件 + key1 提取（CP2，§8/§19）。
//
// 设计（见 plan playful-dazzling-rabin）：鉴权决策下沉 store.AuthorizeRead（fail-closed bool），
// HTTP 层永不直接 GetKeys() 后判 Key1==""——消除"吞 err→误判窗口 A→绕过"脚枪（R1 结构性消除）。
// extractKey1 主源 = Authorization: Bearer header（凭证不进 URL / 默认 access log）。
// ?key1= query 作**扩展接口保留但默认关**（allowKey1Query=false）：CP5 WS upgrade 若遇客户端设不了 header 再开。

package server

import (
	"net/http"
	"strings"
)

// allowKey1Query 扩展接口开关：是否允许 ?key1= query 当 key1 来源。
// 默认 false——query 进 URL（access log/referrer/history 暴露），违背 B 方案"凭证不进 URL"。
// 留作扩展：CP5 WS upgrade 若某客户端无法设 header（App 客户端一般能设），评估后翻此常量。
const allowKey1Query = false

// extractKey1 取 key1：Authorization: Bearer header 为主（不进 URL）。
// allowKey1Query=true 时才退回 ?key1= query（扩展接口，默认关）。
func extractKey1(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	if allowKey1Query {
		return strings.TrimSpace(r.URL.Query().Get("key1"))
	}
	return ""
}

// requireKey1 读端点 key1 准入中间件（CP2）。套到 messages/media/cursor/stream（§8/§19 读端点必 key1）。
// 调 store.AuthorizeRead（fail-closed bool）：err→500（内部错，如 keys 桶损坏）/ false→401（未设窗口 或 key1 不符）/ true→放行。
// 是 *Server 方法（要 s.st）；返回包了 next 的 HandlerFunc，路由装配处 mux.HandleFunc(path, s.requireKey1(s.handleX))。
func (s *Server) requireKey1(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.st.AuthorizeRead(extractKey1(r))
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "auth check failed: "+err.Error())
			return
		}
		if !ok {
			unauthorized(w, "missing or invalid key1")
			return
		}
		next(w, r)
	}
}

// unauthorized 401 + JSON envelope（鉴权失败统一出口；native 无 timestamp）。
func unauthorized(w http.ResponseWriter, msg string) {
	writeAPIError(w, http.StatusUnauthorized, msg)
}
