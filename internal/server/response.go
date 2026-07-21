// HTTP 响应 envelope + helper（TD-3 类型化，2026-07-21 CP3a）。
//
// 消除散落的 inline map[string]any + 错误响应状态码双写漏改源（TD-3）。
// 兼容分开（主从原则的响应面体现）：
//   - native /api/v1 用 apiResp（干净，无 timestamp——native 不背 bark 无用字段）
//   - bark 兼容皮用 barkResp（bark.md §1.5 CommonResp 风格，带 timestamp）
//
// bark + native 共用 writeJSON（通用 JSON 写）；各自的成功/错误 helper 分开。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

// 响应 message 常量（CP3b 屎山审 P2-2：消除跨文件重复 + 测试锁字面量）。
const (
	msgSaveFailed = "save failed: "           // ingest 存失败前缀（接 err.Error）
	msgPushFailed = "saved but push failed: " // ingest 推失败前缀（消息已落库，push 失败不挡）
	msgSuccess    = "success"
	msgRegistered = "registered"
)

// apiResp native 响应外壳（Hotify 原生 /api/v1，无 timestamp）。
// 错误响应的 code = HTTP status（writeAPIError 一处定义，防双写漏改——TD-3 核心）。
type apiResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// barkResp bark 兼容皮响应外壳（bark.md §1.5 CommonResp，带 timestamp）。
// 仅 bark 端点 handleBark 用——兼容分开，native 不带无用 timestamp。
type barkResp struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

// registerResp 原生注册响应（顶层 code/message + key1/key2，CP2 client 契约；无 timestamp）。
// 不 embed apiResp——字段简单且要显式控制 JSON 形状（key1/key2 在顶层，client 直接读）。
type registerResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Key1    string `json:"key1"`
	Key2    string `json:"key2"`
}

// historyResp 历史响应（code + messages；无 Message 字段——client 按数组契约读 messages 不读 message；无 timestamp）。
// 不带 Message 是 client 契约（纯数组），与 registerResp（带 Message）不对称是有意——屎山审 P2-2。
// CP1 临时全局取最近 50 条，CP3b/c 改 /api/v1/messages?since=HLC 后此 struct 随之调。
type historyResp struct {
	Code     int             `json:"code"`
	Messages []model.Message `json:"messages"`
}

// writeOK native 成功响应（HTTP 200 + code:200，无 timestamp）。
func writeOK(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, apiResp{Code: http.StatusOK, Message: msg})
}

// writeAPIError native 错误响应（HTTP status = code，一处定义防双写漏改——TD-3 核心）。
// status 既是 HTTP status 又是 body.code，消除"`http.StatusXxx` + `\"code\": N` 双写改一处漏另一处"。
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiResp{Code: status, Message: msg})
}

// writeRegister 原生注册成功响应（registerResp，无 timestamp——native 干净）。
// 封装 Code=200 + Message="registered"，handler 不碰 Code（TD-3 防双写守到成功路径——屎山审 P2-1）。
func writeRegister(w http.ResponseWriter, key1, key2 string) {
	writeJSON(w, http.StatusOK, registerResp{
		Code:    http.StatusOK,
		Message: msgRegistered,
		Key1:    key1,
		Key2:    key2,
	})
}

// writeHistory 历史响应（historyResp，无 timestamp——native 干净）。
// 封装 Code=200，handler 不碰 Code（TD-3 防双写守到成功路径——屎山审 P2-1）。
func writeHistory(w http.ResponseWriter, msgs []model.Message) {
	writeJSON(w, http.StatusOK, historyResp{Code: http.StatusOK, Messages: msgs})
}

// writeBark bark 兼容皮响应（带 timestamp，bark.md §1.5 CommonResp 风格）。
// timestamp 在此一处填（time.Now().Unix() 秒级，对齐 bark-server router.go:19-24）。
func writeBark(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, barkResp{Code: status, Message: msg, Timestamp: time.Now().Unix()})
}

// writeJSON 通用 JSON 写（所有响应 struct 经此出口）。
// encode err 忽略：写 ResponseWriter 时连接已断无法有意义处理（返回值纪律④ 例外，注释理由）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // encode-to-ResponseWriter err 忽略：连接断无法处理（纪律④ 例外）
}

// classifyIngest 三态分类 ingest 结果（CP3b 屎山审 P2-1：消除 push/bark "判 hlc+写响应" 八行重复）。
// 0=存失败(hlc==0 挡) / 1=推失败(hlc!=0 不挡) / 2=全成功。
func classifyIngest(hlc uint64, err error) int {
	switch {
	case err == nil:
		return 2
	case hlc == 0:
		return 0
	default:
		return 1
	}
}

// writeIngestResult 按 ingest 结果写响应（CP3b 屎山审 P2-1）。
// bark=true 用 writeBark（带 timestamp），false 用 native（200→writeOK / >=400→writeAPIError）。
// handler 一行调，消除 push.go/bark.go 同构八行。
func writeIngestResult(w http.ResponseWriter, hlc uint64, err error, bark bool) {
	writeFn := func(status int, msg string) {
		if bark {
			writeBark(w, status, msg)
			return
		}
		if status == http.StatusOK {
			writeOK(w, msg)
		} else {
			writeAPIError(w, status, msg)
		}
	}
	// device not found 单判（CP3c 跨审修正：不落库 → handler 400，区别于存失败 500 / 推失败 200）
	if errors.Is(err, store.ErrNotFound) {
		writeFn(http.StatusBadRequest, "device not registered")
		return
	}
	switch classifyIngest(hlc, err) {
	case 0:
		writeFn(http.StatusInternalServerError, msgSaveFailed+err.Error())
	case 1:
		writeFn(http.StatusOK, msgPushFailed+err.Error())
	case 2:
		writeFn(http.StatusOK, msgSuccess)
	}
}
