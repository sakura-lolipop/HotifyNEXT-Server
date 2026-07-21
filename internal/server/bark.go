// bark 协议入口（兼容皮）：接收发送端（SmsForwarder「Bark」通道等）消息。
// bark-server 兼容（设备 key 在路径 = 路由，无鉴权——bark.md §1.4 源码确认）。
//
//	POST /{key}                 JSON {title, body, ...}
//	POST /{key}/{title}/{body}  路径式（title/body 按 URL 编码；body 含斜杠也合并）
//	GET  /{key}/{title}/{body}  同上
//
// 成功返 bark 风格 {code:200, message:"success", timestamp}（bark.md §1.5，writeBark 带 timestamp）。
//
// 主从原则（ARCHITECTURE §一句话定位）：原生 /api/v1/push = 主（category/media 一等字段，CP3b 实装）；
// bark /{key} = 从（降级入口，尽力映射）。CP3b：存推接共享 ingest；CP3c：parseBark 重写九缺口 + 字段归宿表。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// handleBark bark 兼容皮入口（CP3a 搬进 server；CP3b 存推接共享 ingest）。
// key 当 uuid 路由（无鉴权，bark.md §1.4）→ msg.TargetUUID；CP1-temp 只解析 title/body，CP3c 按 bark.md §4 重写。
func (s *Server) handleBark(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		writeBark(w, http.StatusBadRequest, "missing device_key")
		return
	}
	key := segs[0]

	// bark 风格通知体（CP1-temp 子集；CP3c 扩 level/group/call 等映射 category + 字段归宿表）。
	var payload struct {
		Title string `json:"title,omitempty"`
		Body  string `json:"body,omitempty"`
	}
	switch {
	case len(segs) >= 3: // 路径式 /{key}/{title}/{body...}（body 含斜杠合并，bark 客户端惯例）
		payload.Title = segs[1]
		payload.Body = strings.Join(segs[2:], "/")
	case r.Method == http.MethodPost: // JSON 体 /{key}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeBark(w, http.StatusBadRequest, "bad json")
			return
		}
		// TODO CP3c：2 段 bug（GET /{key}/body 现被 len>=3 当 title）+ query/form 合并 +
		//   Content-Type 分派 + 字段全集 + 小写化 + URL 解码 + >4 段 404。
	}

	// 归一进 model.Message（key → TargetUUID）→ ingest（存+推共享，CP3b）。
	// category 空由 ingest 兜底 default；TS 由 store 内填（if ==0）；CP3c 重写 parseBark 填更多字段。
	msg := model.Message{
		Title:      payload.Title,
		Body:       payload.Body,
		TargetUUID: key,
	}
	hlc, err := s.ingest(msg)
	writeIngestResult(w, hlc, err, true) // bark（带 timestamp）
}
