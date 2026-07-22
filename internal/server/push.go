// 原生 /api/v1/push 端点 + 共享 ingest（CP3b）。
//
// ingest 是 bark 皮 + 原生 push 共享的"存库+推送"路径（去耦合：不拿 http.ResponseWriter，
// CP5 WS 也能复用）。fanoutPush 封装"查目标设备 + Pusher.Send"。
//
// Pusher 宽签名（Send(msg, dev)）：pushkit 能用所有字段（url→clickAction、category→业务分类、Ext→未来），
// 鸿蒙 API 升级改 pushkit 内部不动 Pusher 接口（plan「扩展性三层」）。
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/util"
)

// Pusher 推送能力抽象（CP3b）：server 依赖 interface 非 *pushkit.Client 具体类型，
// 便于测试 mock（failPusher）+ 未来多平台 adapter（鸿蒙/iOS/安卓，CP4+）。
// *pushkit.Client 已满足此 interface（Send 宽签名），server.New 传 *pushkit.Client 零改。
type Pusher interface {
	Send(msg model.Message, dev model.Device) error
}

// ingest 存库 + 推送（bark 皮 + 原生 push 共享）。去耦合：不拿 ResponseWriter，CP5 WS 可复用。
// 返回 (hlc, err)：
//   - errors.Is(err, store.ErrNotFound) → **device not found（挡，不落库，handler 返 400）**
//   - err!=nil && hlc==0 → GetDevice 内部错或存失败（挡，消息没落库，handler 返 500）
//   - err!=nil && hlc!=0 → 推失败（不挡，消息已落库，handler 返 200 + 留痕）
//   - err==nil           → 全成功
//
// **device not found 不落库**（CP3c 跨审修正，从根杀"随便编 key 灌库"向量）：
// 定向消息（TargetUUID 非空）先 GetDevice 验存在——不存在直接返 ErrNotFound 不 SaveMessage。
// 攻击者必须知道真实 uuid 才能灌（uuid 2^128 枚举不可能 + 泄露概率低），灌库向量从根杀。
// 对齐 bark-server（key 无效返 400 不落库）。全广播（TargetUUID 空，CP6 扇已注册设备）不验，落库。
//
// category 空 → 兜底 "default"（bark/native 都走这；bark CP3c 映射后非空不覆盖）。
func (s *Server) ingest(msg model.Message) (uint64, error) {
	if msg.Category == "" {
		msg.Category = "default" // category 兜底（业务 category 值集含 default，§13b）
	}
	if msg.TS == 0 {
		msg.TS = time.Now().UnixNano() // 预填 TS（CP3c 跨审 D P1：store SaveMessage 内填 TS 是值传递副本，ingest 的 msg.TS 还是 0 → fanoutPush/Pusher.Send 收 TS=0；CP4 PushKit showBeginTime/归并 key 会全 0）。store if TS==0 不覆盖（已非 0）；与 store 内 nowNs 差几 ns 无害（HLC 单调靠 store 自己的 counter 不靠 msg.TS）
	}
	// 定向消息先验设备存在（从根杀随便编 key 灌库）；全广播（TargetUUID 空）跳过验，落库等 CP6 扇
	var dev model.Device
	if msg.TargetUUID != "" {
		var err error
		dev, err = s.st.GetDevice(msg.TargetUUID)
		if errors.Is(err, store.ErrNotFound) {
			log.Printf("[push] device not found target=%s (不落库，从根杀编 key 灌库向量)", msg.TargetUUID)
			return 0, err // device not found → 不落库（handler 400）
		}
		if err != nil {
			return 0, err // GetDevice 内部错 → handler 500
		}
	}
	hlc, err := s.st.SaveMessage(msg) // store 内填 HLC（TS 已在上面预填）
	if err != nil {
		return 0, err // 存失败：消息没落库——挡
	}
	msg.HLC = hlc // 值传递：store 改的是副本，原 msg.HLC 还是 0；回填让 fanoutPush log/未来 CP6 全广播用对 HLC
	// 存库留痕（HLC 归因）：定向/全广播都打，覆盖「定向推成功只在 [pushkit] ✓ 有、server 层无 hlc」+ 调试模式黑洞。
	log.Printf("[push] saved hlc=%d target=%s category=%s", msg.HLC, msg.TargetUUID, msg.Category)
	if msg.TargetUUID == "" {
		// CP3：无定向目标，落库不推（CP6 改全广播 AllDevices 扇出）。
		return hlc, nil
	}
	if pushErr := s.fanoutPush(msg, dev); pushErr != nil {
		return hlc, pushErr // 推失败不挡（消息已落库）——返 pushErr 让 handler 决定响应
	}
	return hlc, nil
}

// fanoutPush 推送单台（dev 已在 ingest GetDevice 验存在，签名收 dev 不再重复查）。
// 返 nil=推成功或死token已清（消息已落库，handler 200）/ err=系统错（handler 200+push failed 留痕）。
//   - 空 token：留痕不推（防静默 success 假绿；理论 register 校验非空，但 legacy/未来 DELETE 可能造空 token 设备）
//   - 死 token（pushkit.ErrDeadToken，华为 80100000/80300007）：ClearPushToken 清 token（CP4 闸门），返 nil
//   - 其他错（system_error/网络）：返 err（保留 token，下次新消息再推）
func (s *Server) fanoutPush(msg model.Message, dev model.Device) error {
	if dev.PushToken == "" {
		log.Printf("[push] device %s empty push token hlc=%d, saved but not pushed", msg.TargetUUID, msg.HLC)
		return nil
	}
	if err := s.pusher.Send(msg, dev); err != nil {
		if errors.Is(err, pushkit.ErrDeadToken) {
			// 死 token → 清 PushToken，防反复推死 token 浪费云函数/Push Kit 配额（CP4 死 token 闸门）。
			if clearErr := s.st.ClearPushToken(dev.UUID); clearErr != nil {
				log.Printf("[push] device %s hlc=%d dead token but ClearPushToken failed: %v (token kept)", dev.UUID, msg.HLC, clearErr)
			} else {
				log.Printf("[push] device %s hlc=%d dead token, PushToken cleared", dev.UUID, msg.HLC)
			}
			return nil // 死 token 非系统错：消息已落库，不挡（ingest 返 nil → handler 200 success）
		}
		return err // 其他推送错（system_error/网络）→ ingest 返 err，handler 决定（200 + push failed 留痕）
	}
	return nil
}

// handleAPIPush 原生推送端点（CP3b，Hotify App 主路径）。
// 套 requireKey1（key1 域内准入）+ JSON body → model.Message → ingest。
//
// Request:  Authorization: Bearer {key1} + JSON {category?, title?, body?, from?, recipient?, target_uuid?, media_ids?, url?}
// Response 成功: {code:200, message:"success"}（apiResp，无 timestamp——native 干净；不回显 HLC，走 /messages 拉）
// Response 错误: {code:400/401/500, message}
func (s *Server) handleAPIPush(w http.ResponseWriter, r *http.Request) {
	// body limit ~1MB（防 OOM/恶意灌，plan CP3b 输入校验）
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// Content-Type 校验（功能审 P2-1：拒 text/plain 等非 JSON 防误解析；bark 留 CP3c Content-Type 分派）
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}

	var body struct {
		Category   string   `json:"category"`
		Title      string   `json:"title"`
		Body       string   `json:"body"`
		From       string   `json:"from"`
		Recipient  string   `json:"recipient"`
		TargetUUID string   `json:"target_uuid"`
		MediaIDs   []string `json:"media_ids"`
		URL        string   `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad json")
		return
	}
	// 必填：title/body/media_ids 至少一个（主路径该有内容；空消息兜底是 bark 兼容行为）
	if body.Title == "" && body.Body == "" && len(body.MediaIDs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "at least one of title/body/media_ids required")
		return
	}
	// category 不校验值集（server 哑，端侧 profile infer 兜底未知值）；Ext 原生不填（bark 才留底）

	msg := model.Message{
		Category:   body.Category,
		Title:      body.Title,
		Body:       body.Body,
		From:       body.From,
		Recipient:  body.Recipient,
		TargetUUID: body.TargetUUID,
		MediaIDs:   body.MediaIDs,
		URL:        util.SanitizeActionURL(body.URL), // TD-12：收 url 进 msg.URL 前 sanitize（javascript:/file: 拒→空；harmony clickAction.data 再 sanitize 双保险）
		// TS: store 内填（if ==0）
	}

	hlc, err := s.ingest(msg)
	// ⚠️ 200 不代表推送成功：code:200 + message="saved but push failed: ..." 表消息已落库但推送失败。
	// client 必须查 message 不能只看 code（功能审 P2-2；完整错误码文档排 CP6 client 契约）。
	writeIngestResult(w, hlc, err, false) // native（无 timestamp）
}
