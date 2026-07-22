// 华为 Push Kit 推送（云函数中转，CP4）。
//
// ⚠️ 方向纠正（CP4）：legacy 实际是「云函数中转」——服务账号 RSA 私钥 / PS256 JWT 锁在云函数
// （hotifypushkit.netlify.app），本 Server 只 POST 云函数（带 token + notification），云函数签 JWT +
// 调华为 Push Kit v3 + 转发返回码。旧注释「JWT 直签 / 移植 Python 桥 get_bearer_token」是过时幻觉
// （get_bearer_token 早删——pushkit-transport.md §10.1；Python 桥 L13「桥不再直连 Push Kit」）。
//
// 多平台分派（Send switch dev.Platform）：harmony→云函数中转（CP4 真做）/ android(HMS)→stub
// （CP4.5）/ ios+macos→APNs stub（待 Apple 开发者账号）/ default→ErrUnknownPlatform。
// 加平台 = 加 adapter 文件 + case，不动 Pusher interface（CP3b 宽签名 Send(msg,dev) 已 ready）。
package pushkit

import (
	"errors"
	"fmt"
	"log"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/util"
)

// huaweiCategory 华为 notification.category 固定 SUBSCRIPTION（已过审 2026-07-02）。
// 业务 category（call/sms/verify/...）走 msg.Category → clickAction.data（不碰 notification.category）。
const huaweiCategory = "SUBSCRIPTION"

// 推送结果哨兵（CP4）。server.fanoutPush 据 ErrDeadToken 调 ClearPushToken 清死 token。
var (
	// ErrDeadToken 设备 PushToken 失效（华为 80100000/80300007）→ server ClearPushToken 清 token 防反复推。
	ErrDeadToken = errors.New("push token dead (huawei 80100000/80300007)")
	// ErrUnknownPlatform dev.Platform 不在支持集（harmony/android/ios/macos）。
	ErrUnknownPlatform = errors.New("unknown device platform")
	// ErrNotImplemented 该平台 adapter 未实装（android HMS = CP4.5；apns 待 Apple 开发者账号）。
	ErrNotImplemented = errors.New("platform push not yet implemented")
)

// Config 云函数中转配置（CP4，对齐 legacy hotify-bridge/go/config.go）。
// 私钥不在此（锁云函数）；Server 只持云函数入口 URL + AUTH_TOKEN（非机密，可入 config）。
type Config struct {
	CloudFunctionURLs  []string `json:"cloud_function_urls"`  // 云函数入口（1-n 个 fallback；空=推送禁用，只存不推）
	CloudFunctionToken string   `json:"cloud_function_token"` // 云函数 AUTH_TOKEN（防爬虫非防推送；空=云函数没开鉴权）
	SubscribeLabel     string   `json:"subscribe_label"`      // "true" 给标题加"订阅:"前缀（华为 SUBSCRIPTION 类目要求）；New 默认 "true"
}

// Client pushkit 客户端（多平台分派）。
type Client struct {
	cfg Config
}

// New 建 pushkit 客户端。SubscribeLabel 空→默认 "true"（华为 SUBSCRIPTION 要求）；CloudFunctionURLs 空→Send 静默跳过。
func New(cfg Config) *Client {
	if cfg.SubscribeLabel == "" {
		cfg.SubscribeLabel = "true" // 默认加"订阅:"前缀（对齐 legacy；华为 SUBSCRIPTION 类目要求标题/正文带订阅字样）
	}
	if len(cfg.CloudFunctionURLs) == 0 {
		log.Printf("[pushkit] cloud_function_urls 为空 → Send 静默跳过（调试模式，只存不推）")
	}
	return &Client{cfg: cfg}
}

// Send 推送消息到设备（Pusher 宽签名，CP3b）。按 dev.Platform 分派到平台 adapter。
//   - 调试模式（CloudFunctionURLs 空）/ 无 token → 静默跳过（返 nil，双保险——fanoutPush 已挡空 token）
//   - harmony → harmonySend 云函数中转（CP4 真做）
//   - android → HMS Push v1 stub（CP4.5）；ios/macos → APNs stub（待 Apple 账号）
//   - 未知 platform → ErrUnknownPlatform
//
// 返 nil=delivered / ErrDeadToken=死token（fanoutPush→ClearPushToken）/ 其他 err=系统错（保留 token）。
func (c *Client) Send(msg model.Message, dev model.Device) error {
	if len(c.cfg.CloudFunctionURLs) == 0 {
		return nil // 调试模式：只存不推
	}
	if dev.PushToken == "" {
		return nil // 无 token 不推（fanoutPush 已挡，双保险）
	}
	switch dev.Platform {
	case "harmony":
		return c.harmonySend(msg, dev)
	case "android":
		return c.androidSend(msg, dev)
	case "ios", "macos":
		return c.apnsSend(msg, dev)
	default:
		log.Printf("[pushkit] unknown platform=%s device=%s hlc=%d token=%s", dev.Platform, dev.UUID, msg.HLC, util.Mask(dev.PushToken))
		return fmt.Errorf("%w: %s", ErrUnknownPlatform, dev.Platform)
	}
}
