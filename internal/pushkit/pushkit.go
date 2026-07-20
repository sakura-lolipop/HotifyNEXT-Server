// 华为 Push Kit 客户端：服务账号 JWT 直当 Bearer → /v3/{project_id}/messages:send。
// 鉴权逻辑照 legacy Python 桥（../hotify/gotify_pushkit_bridge.py 的 get_bearer_token）移植——
// ⚠️ 决策#2：JWT 直当 Bearer，**不换 access_token**（旧桥走过弯路，已纠正）。
// 当前为架构占位：Send 是 TODO（需移植 JWT 签名 + v3 请求体），project_id 为空时静默跳过（调试模式）。
package pushkit

import (
	"fmt"
	"log"
)

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

// Send 向单个 push_token 推一条通知。
// category：消息分类（自用 testMessage 绕限频；正式 SUBSCRIPTION，已过审 2026-07-02）。
//
// TODO 实现真实推送（移植自 Python 桥）：
//  1. 签 JWT：header {kid, typ:JWT, alg:PS256}、payload {iss:sub_account, aud, iat, exp}（**无 sub**）；
//     用 private.json 的 RSA 私钥，PS256 签名。
//  2. JWT 直接当 Authorization: Bearer（**不调 oauth2/v3/token 换 access_token**——旧桥走过这弯路）。
//  3. POST https://push-api.cloud.huawei.com/v3/{project_id}/messages:send
//     body: target{token:[pushToken]} + payload{notification{title,body,category,clickAction{actionType:0}} + pushOptions{testMessage:true|false}}
//     header: push-type:0；成功码 80000000。
func (c *Client) Send(pushToken, title, body, category string) error {
	if c.cfg.ProjectID == "" || pushToken == "" {
		return nil // 调试模式：静默跳过
	}
	log.Printf("[pushkit] TODO Send → token=%s title=%q category=%s", mask(pushToken), title, category)
	return fmt.Errorf("pushkit.Send not yet implemented（待从 Python 桥移植 JWT+v3）")
}

// mask 脱敏 token（日志不泄露全量）。
func mask(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "..." + t[len(t)-4:]
}
