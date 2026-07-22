// harmony.go 鸿蒙 Push Kit 云函数中转（CP4，蓝本 legacy hotify-bridge/go/push.go 302 行）。
//
// 链路：Server → POST 云函数(hotifypushkit.netlify.app) → 云函数签 PS256 JWT + 调华为 Push Kit v3
// → 转发返回码。私钥锁云函数，Server 不持。云函数契约见 docs/pushkit-transport.md §4/§5。
//
// 搬 legacy 的：状态分类(delivered 80000000 / dead 80100000+80300007 / system_error / retry 502)
// + URL fallback（retry 用尽才试下一个 URL）+ 重试 3（同 notifyId 幂等，Push Kit 原生覆盖防重）。
// 全局闸门（本轮 delivered==0 不删）在全广播（CP6）才有意义——CP4 单设备定向推，死就直接返 ErrDeadToken
// 让 fanoutPush 清（store.ClearPushToken），无「本轮多台」概念。
package pushkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/util"
)

// 推送常量（对齐 legacy hotify-bridge/go/push.go + pushkit-transport.md §8.3）。
const (
	harmonyRetryLimit  = 3                // 502/超时重试次数（同 notifyId 幂等，Push Kit 原生覆盖防重）
	harmonyHTTPTimeout = 15 * time.Second // 云函数内部 10s 调 Push Kit + 余量
)

// harmonyRetryInterval retry 间隔（var 非 const：测试可覆盖避免 retry 测等 3s；生产 1s 固定，量小不指数退避）。
var harmonyRetryInterval = 1 * time.Second

// deadTokenCodes 死-token 码（仅这两个语义=token 无效，≈ APNs Unregistered）。
// 鉴权 802x / 权益 80300002 / 超长 80300008 / 频控 / 系统错 81xxxxx 都跟 token 死活无关——误删丢好 token。
// 码表见 docs/pushkit-transport.md §5.3（华为官方 + legacy 实测）。
var deadTokenCodes = map[string]bool{"80100000": true, "80300007": true}

// harmonyPushStatus 推送结果分类（对齐 legacy pushStatus）。
type harmonyPushStatus int

const (
	harmonyDelivered   harmonyPushStatus = iota // 80000000
	harmonyDead                                 // 80100000/80300007
	harmonySystemError                          // 其他 code / HTTP 5xx/401/400（保留 token）
	harmonyRetry                                // 502 / 网络异常（重试）
)

// cloudFuncRequestBody Server→云函数 POST body（对齐 pushkit-transport.md §4.1 契约）。
type cloudFuncRequestBody struct {
	Token        string             `json:"token"`        // 单 token（云函数内部包成 [token]）
	Notification harmonyNotification `json:"notification"` // 调用方构造，云函数原样透传不解释
	Data         string             `json:"data"`         // Push Kit data 载荷（JSON 字符串，云函数透传）
	TestMessage  bool               `json:"testMessage"`
}

// harmonyNotification Push Kit notification 对象（云函数透传）。
type harmonyNotification struct {
	Category    string             `json:"category"` // 固定 SUBSCRIPTION
	Title       string             `json:"title"`
	Body        string             `json:"body"`
	ClickAction harmonyClickAction `json:"clickAction"`
	NotifyID    int                `json:"notifyId,omitempty"` // 0 省略（Push Kit 自动分配）；重试同 id 幂等
}

// harmonyClickAction 点击行为（docs/pushkit-delivery.md 实证：actionType 必须 0；data 平铺进 want.parameters）。
type harmonyClickAction struct {
	ActionType int               `json:"actionType"` // 必须 0（1 要 action/uri → 80100003）
	Data       map[string]string `json:"data"`       // {ts, category, url?, media_ids?}：click 平铺进 App want.parameters
}

// harmonySend 推一条到鸿蒙设备（经云函数中转，CP4）。
// 返 nil=delivered / ErrDeadToken=死token（fanoutPush 调 ClearPushToken）/ 其他 err=系统错（保留 token）。
func (c *Client) harmonySend(msg model.Message, dev model.Device) error {
	title := msg.Title
	body := msg.Body
	if subscribeLabelEnabled(c.cfg.SubscribeLabel) {
		// 华为 SUBSCRIPTION 类目要求标题或正文带"订阅"字样（push-apply-right 订阅流程要点）。
		if title != "" {
			title = "订阅:" + title
		} else {
			body = strings.TrimSpace("订阅:" + body)
		}
	}

	// clickAction.data：click 时平铺进 App want.parameters（pushkit-delivery.md §4/§5 实证）。
	// ts=展示/排序，category=业务分类（端侧呈现），url=点击跳转（sanitize 防 XSS/钓鱼），media_ids=附件引用（端侧点开拉）。
	clickData := map[string]string{
		"ts":       strconv.FormatInt(msg.TS, 10),
		"category": msg.Category,
	}
	if safeURL := util.SanitizeActionURL(msg.URL); safeURL != "" {
		clickData["url"] = safeURL // TD-12 白名单：javascript:/file: 等拒（返空不塞）
	}
	if len(msg.MediaIDs) > 0 {
		clickData["media_ids"] = strings.Join(msg.MediaIDs, ",") // 数组扁平化（clickAction.data 是 map[string]string），端侧 split 拉各 media
	}

	// data 顶层载荷：透传 msg.Ext（bark 留底字段）+ ts。鸿蒙端控制数据走 clickAction.data（上），data 顶层留给安卓/兜底。
	dataObj := map[string]string{"ts": strconv.FormatInt(msg.TS, 10)}
	extCount := 0
	for extKey, extVal := range msg.Ext {
		if extKey == "ts" {
			continue // 防碰撞：Ext["ts"] 不覆盖 server 设的 ts（P3-2）
		}
		if extCount >= 16 {
			break // Ext 数量上限（防 bark 写开放灌大 payload 超 Push Kit 4KB 通知上限，P3-2）
		}
		dataObj[extKey] = extVal
		extCount++
	}
	dataBytes, err := json.Marshal(dataObj)
	if err != nil {
		return fmt.Errorf("harmony data marshal: %w", err)
	}

	requestBody := cloudFuncRequestBody{
		Token: dev.PushToken,
		Notification: harmonyNotification{
			Category: huaweiCategory,
			Title:    orVal(title, "Hotify"),
			Body:     body,
			ClickAction: harmonyClickAction{
				ActionType: 0,
				Data:       clickData,
			},
			NotifyID: int(msg.HLC & 0x7fffffff), // 重试/fallback 同 id → Push Kit 原生覆盖防重复通知（P1-1：HLC 单条唯一；&0x7fffffff 正 int32 防 Push Kit 溢出——delivery.md §8 实测 notifyId 是 int32；单用户量级碰撞可忽略）
		},
		Data:        string(dataBytes),
		TestMessage: false, // 有 SUBSCRIPTION 权益（服务/通讯类无频控），不自用 testMessage 绕频控
	}

	// 遍历 CloudFunctionURLs（fallback，pushkit-transport.md §11）：retry 用尽才试下一个 URL。
	for _, cfURL := range c.cfg.CloudFunctionURLs {
		status, diagMsg := c.postToCloudFunction(cfURL, &requestBody)
		switch status {
		case harmonyDelivered:
			log.Printf("[pushkit] ✓ harmony %s hlc=%d code=80000000 (url=%s)", dev.UUID, msg.HLC, cfURL)
			return nil
		case harmonyDead:
			log.Printf("[pushkit] ✗ harmony %s hlc=%d dead token (url=%s) msg=%s", dev.UUID, msg.HLC, cfURL, diagMsg)
			return fmt.Errorf("%w: %s", ErrDeadToken, diagMsg)
		case harmonySystemError:
			log.Printf("[pushkit] ⚠ harmony %s hlc=%d system_error (url=%s) msg=%s", dev.UUID, msg.HLC, cfURL, diagMsg)
			return fmt.Errorf("harmony push system error: %s", diagMsg) // 非 token 死活，保留 token
		}
		// harmonyRetry：本 URL 重试用尽 → 试下一个 URL（fallback）
	}
	// 所有 URL retry 用尽 → 保留 token（下次新消息再推），不返 ErrDeadToken（非死 token，疑云函数全挂）。
	log.Printf("[pushkit] ✗ harmony %s hlc=%d all cloud function URLs exhausted, keep token", dev.UUID, msg.HLC)
	return fmt.Errorf("harmony push all cloud function URLs exhausted")
}

// postToCloudFunction 向单个云函数 URL 发送（含重试 ≤ harmonyRetryLimit）。
// delivered/dead/system_error 终态即出；retry 重试 ≤ harmonyRetryLimit 后返 retry 终态（调用方试下一个 URL）。
func (c *Client) postToCloudFunction(cfURL string, body *cloudFuncRequestBody) (harmonyPushStatus, string) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return harmonySystemError, "body 序列化失败: " + err.Error()
	}
	var lastDiag string
	for attempt := 1; attempt <= harmonyRetryLimit; attempt++ {
		status, pushKitCode, diagMsg := c.doPost(cfURL, bodyBytes)
		lastDiag = diagMsg
		_ = pushKitCode // doPost 已把 code 融进 diagMsg（delivered/dead/system_error）；retry 无 code
		if status == harmonyDelivered || status == harmonyDead || status == harmonySystemError {
			return status, diagMsg // 终态即出
		}
		// harmonyRetry
		if attempt < harmonyRetryLimit {
			log.Printf("[pushkit] ↻ harmony retry %d/%d (url=%s) %s", attempt+1, harmonyRetryLimit, cfURL, diagMsg)
			time.Sleep(harmonyRetryInterval)
		}
	}
	return harmonyRetry, lastDiag // 重试用尽
}

// doPost 单次 POST 云函数 → 解析返回码分类（对齐 legacy postToPushService）。
// 返 (status, pushKitCode, diagMsg)。pushKitCode 仅 HTTP 200 时有意义（delivered/dead/system_error 的 code）。
func (c *Client) doPost(cfURL string, bodyBytes []byte) (harmonyPushStatus, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), harmonyHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return harmonySystemError, "", "URL 格式错（检查 cloud_function_urls 带 https://）: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.CloudFunctionToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.CloudFunctionToken) // 空 token 不发（空 Bearer 过不了云函数精确匹配，legacy hazard 4）
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return harmonyRetry, "", fmt.Sprintf("网络异常/超时: %v", err) // 超时/连不上 → 重试
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet := readSnippet(resp.Body, 160)
		switch resp.StatusCode {
		case 502, 503, 504:
			return harmonyRetry, "", fmt.Sprintf("HTTP %d %s", resp.StatusCode, snippet) // 网关/CDN/云函数→Push Kit 错/超时 → 重试（503/504 CDN 瞬时抖动也重试，P2-1）
		case 401:
			return harmonySystemError, "", "HTTP 401 unauthorized（cloud_function_token 配错？）" + snippet
		case 400:
			return harmonySystemError, "", "HTTP 400 bad request " + snippet
		default:
			return harmonySystemError, "", fmt.Sprintf("HTTP %d %s", resp.StatusCode, snippet)
		}
	}

	// HTTP 200：解析云函数透传的 Push Kit 原始响应（code 可能 string 或 JSON number→float64）。
	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Code any    `json:"code"` // any → 类型断言（string 或 float64）
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return harmonySystemError, "", "HTTP 200 但 body 非 JSON：" + truncate(string(respBody), 160)
	}
	pushKitCode := anyToCodeStr(result.Code)
	if pushKitCode == "80000000" {
		return harmonyDelivered, pushKitCode, result.Msg
	}
	if deadTokenCodes[pushKitCode] {
		return harmonyDead, pushKitCode, fmt.Sprintf("code=%s msg=%s", pushKitCode, result.Msg) // 折码进 diagMsg：排障区分 80100000（部分 invalid）/ 80300007（全无效），P3-1
	}
	return harmonySystemError, pushKitCode, fmt.Sprintf("code=%s msg=%s", pushKitCode, result.Msg)
}

// subscribeLabelEnabled 真值集 {true,1,yes,on}（小写）。不用 strconv.ParseBool（它拒 yes/on）。
func subscribeLabelEnabled(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// anyToCodeStr code 字段（string 或 JSON number→float64）转字符串。
// float64 须 FormatFloat 'f' 否则 80000000→"8e+07"（科学计数法比不等，legacy hazard 6）。
func anyToCodeStr(v any) string {
	switch num := v.(type) {
	case string:
		return num
	case float64:
		return strconv.FormatFloat(num, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(num)
	}
}

// readSnippet 读响应 body 前 maxLen 字节（错误诊断用）。
func readSnippet(reader io.Reader, maxLen int) string {
	buf := make([]byte, maxLen)
	readBytes, _ := reader.Read(buf)
	return string(buf[:readBytes])
}

// truncate rune 安全截断（中文不劈）。
func truncate(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

// orVal s 非空返 s 否则返 def（对齐 legacy title or "Hotify"）。
func orVal(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
