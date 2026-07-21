// HTTP server 装配 + 路由（Go 1.22 ServeMux 方法路由）。
//
//	POST /api/v1/register    原生设备注册（CP2）：key1 first-set 鉴权 + 下发 key1/key2（§2 client 契约）
//	POST /register           旧字段名 {device_key, push_token, name} 兼容（无 key1，不 first-set；老 App）
//	ANY  /{key}...           bark 入口（消息投递；兜底走 bark.Handler 自解析路径）
//	GET  /messages/{key}     拉历史（最近 50 条；套 requireKey1，CP3 改 /api/v1/messages?since=HLC）
//	POST /read/{key}         已读（§14 砍 read set，返 410 Gone 防旧 App 404 噪声）
//	GET  /read/{key}         同上
//
// CP2：key1 域内读准入接上（requireKey1 + store.AuthorizeRead/ResolveRegisterKey）；CP3 改 /api/v1/* 主体 + WS /stream（CP5）。
// 部署前提（R4）：first-set 窗口由首个原生 /api/v1/register 关闭——NEXT 部署须跑 NEXT App（仅 legacy /register 会留窗口 A）。
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
	mux.HandleFunc("POST /api/v1/register", s.handleAPIRegister)         // 原生（CP2，key1 first-set）
	mux.HandleFunc("POST /register", s.handleRegister)                   // 旧字段兼容（无 key1）
	mux.HandleFunc("GET /messages/{key}", s.requireKey1(s.handleHistory)) // 读端点 key1 准入（CP2，§19）
	mux.HandleFunc("POST /read/{key}", s.handleMarkRead) // §14 砍 read set，返 410
	mux.HandleFunc("GET /read/{key}", s.handleReadSet)   // 同上
	mux.HandleFunc("/", s.bark.ServeHTTP) // 兜底 bark 入口 /{key}...（§鉴权表"域内无"= design：bark 零摩擦卖点，写开放靠部署层 LAN 兜，非漏接；与 §19 读端点 key1 准入正交）
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
	log.Printf("[register-legacy] device=%s token=%s (old App, no key1 first-set)", b.DeviceKey, mask(b.PushToken))
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "message": "registered"})
}

// POST /api/v1/register 原生设备注册（CP2，NEXT-client §2 契约）。
// key1 first-set 鉴权：空 server 首设备不带 key1 → first-set + 下发；之后必须带对 key1（防 B 蹭全广播）。
// 鉴权先于设备注册（拒登不建设备记录）。key1 走 Authorization: Bearer header（B 方案，不进 URL）。
//
// Request:  Authorization: Bearer {key1}（窗口 A 可省）+ JSON {uuid, platform, push_token, type?, name?}
// Response: {code:200, message, key1, key2}  // key1=域内读端点准入，key2=构造 share URL
func (s *Server) handleAPIRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID      string `json:"uuid"`
		Platform  string `json:"platform"`
		PushToken string `json:"push_token"`
		Type      string `json:"type"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.UUID == "" || body.Platform == "" || body.PushToken == "" {
		http.Error(w, "uuid, platform, push_token required", http.StatusBadRequest)
		return
	}

	// key1 三态决策 + first-set（atomic CAS，fail-closed bool——HTTP 层不直接判 Keys{}）。
	key1, allowed, err := s.st.ResolveRegisterKey(extractKey1(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500, "message": "resolve key1 failed: " + err.Error(),
		})
		return
	}
	if !allowed {
		unauthorized(w, "invalid or missing key1")
		return
	}

	// 鉴权通过 → 注册设备（patch 语义；token 刷新走同路径）。
	if err := s.st.RegisterDevice(model.Device{
		UUID:      body.UUID,
		Platform:  body.Platform,
		PushToken: body.PushToken,
		Type:      body.Type,
		Name:      body.Name,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500, "message": "register failed: " + err.Error(),
		})
		return
	}

	// key2 回显（启动 EnsureKey2 已生成；GetKeys 只读 Key2，非鉴权决策）。
	keys, err := s.st.GetKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500, "message": "read keys failed: " + err.Error(),
		})
		return
	}

	log.Printf("[register] device=%s platform=%s token=%s", body.UUID, body.Platform, mask(body.PushToken))
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    200,
		"message": "registered",
		"key1":    key1,
		"key2":    keys.Key2,
	})
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
