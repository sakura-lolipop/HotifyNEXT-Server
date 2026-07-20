// bark 协议入口（兼容皮）：接收发送端（SmsForwarder「Bark」通道等）消息。
// bark-server 兼容（设备 key 在路径 = 路由 + 鉴权，无 token）：
//
//	POST /{key}                 JSON {title, body, ...}
//	POST /{key}/{title}/{body}  路径式（title/body 按 URL 编码；body 含斜杠也合并）
//	GET  /{key}/{title}/{body}  同上
//
// 成功返 bark 风格 {code:200, message:"success"}。
//
// 主从原则（docs §6）：原生 /api/v1/push = 主（category/media 一等字段，CP3 实装）；
// bark /{key} = 从（降级入口，尽力映射）。CP1：category=default 临时（bark level/group/call→category 归一在 CP3）。
package bark

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

type Handler struct {
	Store store.Store
	Push  *pushkit.Client
	// Category：消息分类（自用 testMessage 绕限频；正式 SUBSCRIPTION 已过审）。默认 SUBSCRIPTION。
	Category string
}

// payload JSON 通知体（bark 风格子集；CP3 扩 level/group/call 等映射 category）。
type payload struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		writeJSON(w, http.StatusBadRequest, resp{Code: 400, Message: "missing device_key"})
		return
	}
	key := segs[0]

	var p payload
	switch {
	case len(segs) >= 3: // 路径式 /{key}/{title}/{body...}
		p.Title = segs[1]
		p.Body = strings.Join(segs[2:], "/")
	case r.Method == http.MethodPost: // JSON 体 /{key}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, resp{Code: 400, Message: "bad json"})
			return
		}
	}

	// 1) 落历史（无论推送成败，消息先存）。
	// CP1：category=default 临时（bark level/group/call→category 归一在 CP3）；忽略返回的 HLC。
	if _, err := h.Store.SaveMessage(model.Message{
		Category: "default",
		Title:    p.Title,
		Body:     p.Body,
		TS:       time.Now().UnixNano(),
	}); err != nil {
		// 存失败挡 500（消息没落库，推送无意义）——CLAUDE.md ④ 返回值不吞错
		writeJSON(w, http.StatusInternalServerError, resp{Code: 500, Message: "save failed: " + err.Error()})
		return
	}

	// 2) 推送：按 key 查 token（key 当 uuid；TODO CP6 改全广播 AllDevices 扇出 + CP4 搬 push.go 健壮性）。
	d, err := h.Store.GetDevice(key)
	switch {
	case err == nil && d.PushToken != "":
		if pushErr := h.Push.Send(d.PushToken, p.Title, p.Body, h.Category); pushErr != nil {
			// 推送失败不挡 200（消息已落库；TODO CP4 死 token 清理/重试矩阵）
			writeJSON(w, http.StatusOK, resp{Code: 200, Message: "saved but push failed: " + pushErr.Error()})
			return
		}
	case errors.Is(err, store.ErrNotFound):
		// 设备未注册——消息已落库（主目的达成），跳过推送但留痕（CLAUDE.md ③ 异常不静默吞）
		log.Printf("[bark] device %s not registered, message saved but not pushed", key)
	case err != nil:
		// GetDevice 内部错（非 NotFound）记 log
		log.Printf("[bark] GetDevice %s error: %v", key, err)
	default:
		// err==nil 但 PushToken 空——register 校验 token 非空，理论不触发；留痕防静默 success 假绿
		log.Printf("[bark] device %s empty push token, message saved but not pushed", key)
	}
	writeJSON(w, http.StatusOK, resp{Code: 200, Message: "success"})
}

type resp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
