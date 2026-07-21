// 华为 Push Kit 客户端：服务账号 JWT 直当 Bearer → /v3/{project_id}/messages:send。
// 鉴权逻辑照 legacy Python 桥（../hotify/gotify_pushkit_bridge.py 的 get_bearer_token）移植——
// ⚠️ 决策#2：JWT 直当 Bearer，**不换 access_token**（旧桥走过弯路，已纠正）。
// 当前为架构占位：Send 是 TODO（需移植 JWT 签名 + v3 请求体），project_id 为空时静默跳过（调试模式）。
package pushkit

import (
	"fmt"
	"log"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/util"
)

// huaweiCategory 华为 notification.category 固定 SUBSCRIPTION（已过审 2026-07-02）。
// 业务 category（call/sms/verify/...）走 msg.Category → PushKit clickAction.data（CP4），不碰 notification.category。
// 自用绕频控走 pushOptions.testMessage（CP4 实装），非 category。
const huaweiCategory = "SUBSCRIPTION"

type Config struct {
	ProjectID      string `json:"project_id"`       // 华为项目 ID（push API URL 路径用）
	PrivateKeyPath string `json:"private_key_path"` // 服务账号 JSON 路径（含 RSA 私钥/key_id/sub_account）
	Region         string `json:"region"`           // "CN"（仅中国境内，不含港澳台）
}

type Client struct {
	cfg Config
	// TODO: 缓存的 JWT + 过期时间（JWT 有 exp，过期重签；参考 Python 桥 _bearer 缓存）
}

func New(cfg Config) *Client {
	if cfg.ProjectID == "" {
		log.Printf("[pushkit] project_id 为空 → Send 静默跳过（调试模式，只存不推）")
	}
	return &Client{cfg: cfg}
}

// Send 推送消息到设备（CP3b 宽签名：收完整 Message + Device）。
// 宽签名让 pushkit 能用所有字段（url→clickAction.data、msg.Category→业务分类、Ext→未来字段），
// 鸿蒙 PushKit API 升级（v3→v4）或加新能力 = 改本函数，不动 Pusher 接口（plan「扩展性三层」）。
//
// TODO 实现真实推送（CP4 移植自 Python 桥）：
//  1. 签 JWT：header {kid, typ:JWT, alg:PS256}、payload {iss:sub_account, aud, iat, exp}（**无 sub**）；
//     用 private.json 的 RSA 私钥，PS256 签名。
//  2. JWT 直接当 Authorization: Bearer（**不调 oauth2/v3/token 换 access_token**——旧桥走过这弯路）。
//  3. POST https://push-api.cloud.huawei.com/v3/{project_id}/messages:send
//     body: target{token:[dev.PushToken]} + payload{notification{title:msg.Title, body:msg.Body,
//     category:huaweiCategory, clickAction{actionType:0, data: msg.Category+msg.URL}}
//     + pushOptions{testMessage:true|false}}
//     header: push-type:0；成功码 80000000；死 token 80100000/80300007 删（CP4 全局闸门）。
//
// CP3 stub：ProjectID 空 / dev.PushToken 空 → 静默跳过（return nil）；否则返 not implemented。
func (c *Client) Send(msg model.Message, dev model.Device) error {
	if c.cfg.ProjectID == "" || dev.PushToken == "" {
		return nil // 调试模式 / 无 token：静默跳过
	}
	log.Printf("[pushkit] TODO Send → token=%s title=%q cat=%s url=%s",
		util.Mask(dev.PushToken), msg.Title, msg.Category, msg.URL)
	return fmt.Errorf("pushkit.Send not yet implemented（待 CP4 从 Python 桥移植 JWT+v3）")
}
