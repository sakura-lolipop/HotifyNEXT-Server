// apns.go APNs stub（ios + macos，待 Apple 开发者账号）。
//
// ios + macos 共享 .p8 Auth Key（Bundle ID 区分 ios/macos）。需 99 美元/年 Apple Developer + 苹果审核，
// 待用户需求触发实装（非 HMS 生态优先级低）。APNs 码表（Unregistered 等）不同于华为 Push Kit，
// 实装时单独定义死 token 判定。
package pushkit

import "github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"

// apnsSend APNs 推送（待 Apple 开发者账号 + .p8 + 审核，当前 stub）。
func (c *Client) apnsSend(msg model.Message, dev model.Device) error {
	_ = msg
	_ = dev
	return ErrNotImplemented
}
