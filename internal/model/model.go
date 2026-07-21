// 数据模型：设备、消息、媒体、游标、密钥。
// CP3b：砍 PushSpec 归一中间层（CP1 死代码，零调用方）——bark 解析直出 Message + 原生 JSON 直映射 Message，共享 ingest。
//
// 重写对齐 NEXT 架构（docs/NEXT-Server.md）：
//   - uuid 路由（替代旧 device_key 当鉴权；鉴权归 key1，uuid 回归寻址，§7/§8）
//   - HLC 消息标识（替代自增 id；bbolt big-endian key 天然单调有序，§7）
//   - MediaIDs 引用数组（消息只存 id，metadata 在 media 桶——不重复存，§4b）
//   - 砍 read set（§14）、砍 key_ver/retired（§9 紧急重置不搞无感轮换）
package model

import "time"

// Device 已注册的接收设备。
// uuid 是路由 key + 设备身份（App 自生成 UUIDv4，跨平台稳定）；
// push_token 会轮换（onNewToken/重装/系统升级），uuid 稳定——故 uuid 当 key、token 当 value（解耦，§7）。
// 鉴权归 key1（§8），不靠 uuid——uuid 不背鉴权的活，只做寻址/路由终点。
type Device struct {
	UUID       string    `json:"uuid"`                   // App 自生成 UUIDv4；bbolt device 桶 key（替代旧 DeviceKey）
	Platform   string    `json:"platform"`               // harmony/ios/android/windows（adapter 出口分发，CP4）
	PushToken  string    `json:"push_token"`             // 华为 Push Kit token（会轮换；server 维护，onNewToken 刷新）
	Type       string    `json:"type"`                   // phone/watch/tablet/pc/2in1/tv（端侧显图标 + 呈现差异，§17）
	Name       string    `json:"name,omitempty"`         // 设备别名（可选，UI 展示）
	CreatedAt  time.Time `json:"created_at"`             // 首注册时间
	UpdatedAt  time.Time `json:"updated_at"`             // 最近 token 刷新时间
	LastSeenAt time.Time `json:"last_seen_at,omitempty"` // WS 连接更新（CP7）；离线时显"X 分钟前"（实时 online 是 CP7 内存 set + GET /devices 响应字段，两层别混）
}

// Media 媒体附件元数据（blob 走文件系统，bbolt media 桶只存 metadata，§4b）。
// Message 只存 MediaIDs 引用，不内联完整 Media——避免数据重复（改 metadata 只动 media 桶一处，不会 msgs/media 不一致）。
type Media struct {
	ID   string `json:"id"`   // media_id（store 内生成 UUIDv4）；bbolt media 桶 key
	Path string `json:"path"` // blob 文件系统路径（相对 config.Store.BlobDir）
	Size int64  `json:"size"` // 字节数
	MIME string `json:"mime"` // image/* 图片 / audio/* 语音 / 其他 文件（端侧按 mime 选渲染：图显/播放/下载）
}

// Message 一条入站消息（多源归一后的内部表示，存 msgs 桶）。
// HLC 是唯一标识 + 排序 key + 补漏高水位 + 游标基准（§7，全用 HLC 不用自增 id/裸 ts）。
// Body（文字）与 MediaIDs（附件引用）并列——支持"纯文字"(MediaIDs=nil)/"文字+图"([id])/"多图"([id1,id2])。
type Message struct {
	HLC        uint64            `json:"hlc,string"`          // HLC（pt+counter pack）；bbolt msgs 桶 big-endian key；json string 防客户端 Number 精度（2^53 上限，HLC 实际值远超）
	From       string            `json:"from,omitempty"`      // 发送方自报（跨户；域内空）——全局 key2 下无法可靠区分对端身份，安全上不依赖（§7/§11）
	Recipient  string            `json:"recipient,omitempty"` // 接收方自报（跨户 peer 标识）——§11 按 sender+recipient 分组对话线程；与 TargetUUID 正交（后者域内哪台设备，前者跨户哪个 peer）
	Category   string            `json:"category"`            // 业务 category（call/sms/verify/app_notify/history/default，§13b）；落不上走 default
	Title      string            `json:"title,omitempty"`
	Body       string            `json:"body,omitempty"`        // 文字内容（与 MediaIDs 并列，非二选一）
	TS         int64             `json:"ts"`                    // 物理时间戳 ns（展示用）；补漏靠 HLC 不靠它（HLC 单调，ts 可能 NTP 回退）
	MediaIDs   []string          `json:"media_ids,omitempty"`   // media_id 引用数组：nil=纯文字 / [id]=一附件 / [id1,id2]=多附件
	TargetUUID string            `json:"target_uuid,omitempty"` // 域内定向设备 uuid（少用，私密单发）；空=全广播（§7）
	URL        string            `json:"url,omitempty"`         // CP3b: 点击跳转 URL（bark url / 原生 url → PushKit clickAction.data，CP4 用）；CP3c 两边裸收未校验，协议白名单延 CP4（TD-12）
	Ext        map[string]string `json:"ext,omitempty"`         // CP3b/c bark 未映射字段留底（扩展性三层之数据层）。留底纪律三类：一等字段（title/body/category含call/group归一/url/media_ids）强类型；Ext 留底无映射目标（subtitle/image/icon/copy/autoCopy/level+未知兜底）；真丢（sound/badge/volume端侧profile + ciphertext/iv/isArchive/ttl §9§14）。level 不映射 category（语义正交）；原生 push 不填 Ext
}

// Cursor 阅读游标（覆盖式单值，不跑 TTL，§13）。
// Server 存 {view, focus_HLC, reported_at}；多设备并发报最新 reported_at 胜（last-write-wins）。
// 过期判断（now - reported_at > 3min）在 App 端，Server 不判——保持 server 哑。
type Cursor struct {
	View       string `json:"view,omitempty"`   // chat/list（IM 对话/通知列表；手表无视图可空）
	FocusHLC   uint64 `json:"focus_hlc,string"` // 看到的最新消息 HLC；json string 同 Message.HLC 防精度
	ReportedAt int64  `json:"reported_at"`      // 上报时间 ns（多设备并发：最新胜）
}

// Keys key1 + key2（§8/§9 两层正交鉴权）。bbolt keys 桶单值（一 server 一用户，全局一对）。
//
//	key1 = 域内读端点准入（register/messages/media/cursor/stream 带 key1）——空起始 first-set：
//	       首设备 register 不带 key1 → server 设 + 下发；之后所有 register/读端点必须带 key1。
//	key2 = 跨户 share secret（share URL bearer；server 启动生成；构造 https://{server}/share/{key2}）。
//
// 砍 retired/key_ver（§9：紧急重置不搞无感轮换——共享密钥 + WS 下发对已泄露方无效，YAGNI）。
type Keys struct {
	Key1 string `json:"key1,omitempty"` // 域内读端点准入（first-set；空 = 未设）
	Key2 string `json:"key2,omitempty"` // 跨户 share secret（启动生成）
}
