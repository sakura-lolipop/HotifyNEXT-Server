// HTTP server 装配 + 路由（Go 1.22 ServeMux 方法路由）。
//
//	POST /register           客户端上报 {device_key, push_token, name}（旧字段名兼容；CP3/CP4 改 /api/v1/register）
//	ANY  /{key}...           bark 入口（消息投递；兜底走 bark.Handler 自解析路径）
//	GET  /messages/{key}     拉历史（最近 50 条；CP3/CP4 改 /api/v1/messages?since=HLC）
//	POST /read/{key}         已读（§14 砍 read set，返 410 Gone 防旧 App 404 噪声）
//	GET  /read/{key}         同上
//
// CP1 最小适配 store 新 interface（不动物理路由协议，CP3/CP4 改 /api/v1/* + key1 鉴权 + WS /stream）。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/bark"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

type Server struct {
	cfg  *config.Config
	st   store.Store
	pk   *pushkit.Client
	bark *bark.Handler
}

func New(cfg *config.Config, st store.Store, pk *pushkit.Client) *Server {
	return &Server{
		cfg: cfg,
		st:  st,
		pk:  pk,
		bark: &bark.Handler{
			Store:    st,
			Push:     pk,
			Category: "SUBSCRIPTION", // 默认 SUBSCRIPTION（已过审 2026-07-02）；自用改 testMessage 在桥侧
		},
	}
}

// Handler 返回 HTTP handler（mux + log 中间件），供 httptest 测试 + main 装配。
// 路由表集中此处，Run() 复用。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("GET /messages/{key}", s.handleHistory)
	mux.HandleFunc("POST /read/{key}", s.handleMarkRead) // §14 砍 read set，返 410
	mux.HandleFunc("GET /read/{key}", s.handleReadSet)   // 同上
	mux.HandleFunc("/", s.bark.ServeHTTP)                // 兜底 bark 入口 /{key}...
	return logReq(mux)
}

func (s *Server) Run() error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

// POST /register {device_key, push_token, name}
// CP1：旧字段名 device_key 兼容（值映射 Device.UUID）；Platform 临时默认 "harmony"；
// Type 缺失（NEXT-client §1 注册要上报 type，CP3/CP4 改 /api/v1/register 补 platform+type 真字段）。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DeviceKey string `json:"device_key"`
		PushToken string `json:"push_token"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if b.DeviceKey == "" || b.PushToken == "" {
		http.Error(w, "device_key and push_token required", http.StatusBadRequest)
		return
	}
	if err := s.st.RegisterDevice(model.Device{
		UUID:      b.DeviceKey, // 旧 device_key 当 uuid（CP3/CP4 改真 uuid）
		Platform:  "harmony",   // 旧 App 是鸿蒙（CP3/CP4 真上报 platform）
		PushToken: b.PushToken,
		Name:      b.Name,
	}); err != nil {
		http.Error(w, "register failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[register] device=%s token=%s", b.DeviceKey, mask(b.PushToken))
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "message": "registered"})
}

// handleHistory CP1 临时：忽略 {key} 路径参数，全局取最近 50 条（CP3/CP4 改 /api/v1/messages?since=HLC）。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.st.MessagesSince(0, 50)
	if err != nil {
		http.Error(w, "history failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "messages": msgs})
}

// §14 砍 read set——返 410 Gone 防旧 App 404 噪声，CP3/CP4 删路由。
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"code": 410, "message": "read set deprecated (docs NEXT-Server §14)",
	})
}

func (s *Server) handleReadSet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"code": 410, "message": "read set deprecated (docs NEXT-Server §14)",
	})
}

func logReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// mask token 脱敏（日志用）。
func mask(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "..." + t[len(t)-4:]
}
