// android.go HMS Push v1 stub（仅华为系安卓，CP4.5 实装）。
//
// 走独立安卓云函数（CloudFuctionAndroid/netlify，OAuth2 access_token → HMS Push v1），
// 复用鸿蒙云函数中转架构（Server→云函数→HMS）。非 HMS 安卓（海外/原生/小米/vivo/OPPO 无华为移动服务）
// 无系统推送 token，走 CP5 WS 在线推 + 本地通知（不建本文件路径——CP5 WS 另一条路）。
package pushkit

import "github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"

// androidSend HMS Push v1 推送（CP4.5 实装，当前 stub）。
// CP4.5：搬安卓云函数中转（access_token 签名 + HMS Push v1 码表），复用 harmony.go 的 fallback/重试骨架。
func (c *Client) androidSend(msg model.Message, dev model.Device) error {
	_ = msg
	_ = dev
	return ErrNotImplemented
}
