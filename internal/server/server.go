// HTTP server 装配 + 路由（Go 1.22 ServeMux 方法路由）。
//
//	POST /api/v1/register    原生设备注册（CP2）：key1 first-set 鉴权 + 下发 key1/key2（§2 client 契约）
//	POST /register           旧字段名 {device_key, push_token, name} 兼容（无 key1，不 first-set；老 App）
//	ANY  /{key}...           bark 入口（消息投递；handleBark 自解析路径）
//	GET  /messages/{key}     拉历史（最近 50 条；套 requireKey1，CP3b 改 /api/v1/messages?since=HLC）
//
// CP3a：bark 包搬进 server（internal/bark 删，TD-3 同源消除）+ envelope 类型化（response.go，TD-3）+
//   删 /read 410 双胞胎（TD-4：§14 砍 read set，端点重排时清干净）。
// CP3b：原生 /api/v1/push + 共享 ingest + Pusher 宽签名 + bark 接 ingest。
// 部署前提（R4）：first-set 窗口由首个原生 /api/v1/register 关闭——NEXT 部署须跑 NEXT App（仅 legacy /register 会留窗口 A）。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

type Server struct {
	cfg *config.Config
	st  store.Store
	pk  Pusher // CP3b: Pusher interface（pushkit/iOS/安卓 adapter，CP4+）；*pushkit.Client 已满足
}

// New 装配 server。pk 是 Pusher（*pushkit.Client 已满足，main.go 传 pushkit.New(...) 零改）。
func New(cfg *config.Config, st store.Store, pk Pusher) *Server {
	return &Server{cfg: cfg, st: st, pk: pk}
}

// Handler 返回 HTTP handler（mux + log 中间件），供 httptest 测试 + main 装配。
// 路由表集中此处，Run() 复用。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/register", s.handleAPIRegister)          // 原生（CP2，key1 first-set）
	mux.HandleFunc("POST /api/v1/push", s.requireKey1(s.handleAPIPush))   // 原生推送（CP3b，key1 准入）
	mux.HandleFunc("POST /register", s.handleRegister)                    // 旧字段兼容（无 key1）
	mux.HandleFunc("GET /messages/{key}", s.requireKey1(s.handleHistory)) // 读端点 key1 准入（CP2，§19）
	mux.HandleFunc("/read/{key}", s.handleReadDeprecated) // §14 砍 read set → 410（TD-4 合一：保留路由避免旧 App 落 bark 兜底灌空消息）
	mux.HandleFunc("/api/", s.handleNotFound)      // /api/ 子树兜底 404（POST /api/v1/* 精确优先；防失配 method 落 bark 兜底污染 msgs——CP3b 功能审 #15）
	mux.HandleFunc("/share/", s.handleNotFound)    // /share/ 子树兜底 404（CP6 跨户端点前缀，同防）
	mux.HandleFunc("/messages/", s.handleNotFound) // /messages/ 子树兜底 404（GET /messages/{key} 精确优先；防 POST/PUT 等 method 失配落 bark 兜底建 TargetUUID="messages" 污染——CP3c 跨层审）
	mux.HandleFunc("/register/", s.handleNotFound) // /register/ 子树兜底 404（POST /register 精确优先；防 /register/xxx 子路径落 bark 兜底建 TargetUUID="register" 污染——CP3c 跨层审）
	mux.HandleFunc("/", s.handleBark) // 兜底 bark 入口 /{key}...（§鉴权表"域内无"= design：bark 零摩擦卖点，写开放靠部署层 LAN 兜，非漏接；与 §19 读端点 key1 准入正交）
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
// 错误响应保持 http.Error 纯文本（legacy 老兼容，CP3a 行为不变——成功用 writeOK JSON，错误纯文本是原状）。
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
	writeOK(w, msgRegistered)
}

// POST /api/v1/register 原生设备注册（CP2，NEXT-client §2 契约）。
// key1 first-set 鉴权：空 server 首设备不带 key1 → first-set + 下发；之后必须带对 key1（防 B 蹭全广播）。
// 鉴权先于设备注册（拒登不建设备记录）。key1 走 Authorization: Bearer header（B 方案，不进 URL）。
//
// Request:  Authorization: Bearer {key1}（窗口 A 可省）+ JSON {uuid, platform, push_token, type?, name?}
// Response 成功: {code:200, message, key1, key2}（registerResp，无 timestamp——native 干净）
// Response 错误: {code:400/500, message}（writeAPIError JSON——native 错误统一 JSON 化，TD-3 顺带；
//   legacy /register 例外保留 http.Error 纯文本兼容老 App——审查 P1 承认此设计变更）
func (s *Server) handleAPIRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UUID      string `json:"uuid"`
		Platform  string `json:"platform"`
		PushToken string `json:"push_token"`
		Type      string `json:"type"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad json")
		return
	}
	if body.UUID == "" || body.Platform == "" || body.PushToken == "" {
		writeAPIError(w, http.StatusBadRequest, "uuid, platform, push_token required")
		return
	}

	// key1 三态决策 + first-set（atomic CAS，fail-closed bool——HTTP 层不直接判 Keys{}）。
	key1, allowed, err := s.st.ResolveRegisterKey(extractKey1(r))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "resolve key1 failed: "+err.Error())
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
		writeAPIError(w, http.StatusInternalServerError, "register failed: "+err.Error())
		return
	}

	// key2 回显（启动 EnsureKey2 已生成；GetKeys 只读 Key2，非鉴权决策）。
	keys, err := s.st.GetKeys()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "read keys failed: "+err.Error())
		return
	}

	log.Printf("[register] device=%s platform=%s token=%s", body.UUID, body.Platform, mask(body.PushToken))
	writeRegister(w, key1, keys.Key2)
}

// handleHistory CP1 临时：忽略 {key} 路径参数，全局取最近 50 条（CP3b 改 /api/v1/messages?since=HLC）。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.st.MessagesSince(0, 50)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "history failed: "+err.Error())
		return
	}
	writeHistory(w, msgs)
}

// handleReadDeprecated §14 砍 read set → 返 410 Gone。
// TD-4：原 handleMarkRead/handleReadSet 双胞胎合一（Go 1.22 "/read/{key}" 匹配全方法，一个 handler）。
// 保留路由不删——避免旧 App /read 请求落 / bark 兜底（read 当 device_key）灌空消息 + 给明确废弃信号（审查 P3 采纳）。
func (s *Server) handleReadDeprecated(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusGone, "read set deprecated (docs NEXT-Server §14)")
}

// handleNotFound 拒绝非注册端点（/api/ /share/ 子树的失配 method）。
// 防失配请求落 / bark 兜底建空消息污染 msgs（CP3b 功能审 #15：GET /api/v1/push 曾落 bark 建 TargetUUID="api" 空消息）。
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusNotFound, "not found: "+r.URL.Path)
}

func logReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// mask token 脱敏（日志用）。
func mask(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "..." + t[len(t)-4:]
}
