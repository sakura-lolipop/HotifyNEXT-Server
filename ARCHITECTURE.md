# HotifyServer 架构

> 自建 Go 后端。对标 **bark-server**（设备注册 + bark 入口 + BBolt 存储）。定位：通知转发领域的"小鸿蒙"——自主内核（Go 自建）+ 生态兼容面（bark 入口）。
> **架构决策记录（为什么 / 否决了啥）见 `docs/NEXT-Server.md`（私有）；本文件是工程蓝图（是什么 + 怎么实现，自包含）。**

## 一句话定位 + 主从原则

**bark 兼容皮（根路径）+ Hotify 自有 API `/api/v1/`（主）+ Push Kit 引擎。**

- **原生 `/api/v1/` = 主**：定义 Hotify 完整能力，`category` 等业务字段是一等字段（想要啥值有啥）。Hotify 能力上限由它定，不由 bark 定。
- **bark 兼容皮 `/{key}` = 从**：降级入口，尽力把 bark 字段映射进内部 PushSpec，老客户端（含 SmsForwarder）只用得到一部分能力。Hotify 不是 bark 挂件，bark 是众多"可入源"之一。

## 三层架构

| 层 | 路径 | 使用者 | 鉴权 | 角色 |
|---|---|---|---|---|
| 兼容皮（根） | `/{key}`、`/{key}/{title}/{body}` | SmsForwarder 等 bark 发送端 | 域内无 | bark 协议入口，零摩擦 |
| 跨户写 | `POST /share/{key2}` | 跨户对端 | **key2** | 跨户投递 |
| 自有内核 `/api/v1/` | register/messages/media/cursor/stream/profile/present_profile | Hotify App | **key1**（profile 用 key2） | Hotify 原生能力 |
| 鸿蒙引擎 | `internal/pushkit/` | 上两层都调 | — | Push Kit 推送 |

## 包结构

```
HotifyServer/
  main.go              入口：load 配置 → 建 database/pushkit/server → 启动
  config.example.json  配置模板（真实 config.json 含机密，gitignore）
  internal/
    config/            配置加载
    model/             Device / Message(HLC) / Cursor / Profile 类型
    database/          bbolt 存储（HLC key + 空间阈值 FIFO + media metadata；移植自 bark-server，MIT）
    pushkit/           华为 Push Kit 客户端（JWT Bearer + v3，移植自 legacy 桥；CP3b Send 宽签名 stub，CP4 真接）
    server/            HTTP 装配 + 路由（Go 1.22 ServeMux）+ 鉴权（key1/key2）+ bark 兼容皮（CP3a 搬进，砍独立 bark 包）+ 共享 ingest（CP3b，bark+native 共用存推）
```

## 路由

| 方法 路径 | 鉴权 | 作用 |
|---|---|---|
| `POST /api/v1/register` | key1（首注 first-set 不带） | 设备注册：uuid+platform+token+type+key_ver；首设备设 key1 |
| `POST /api/v1/push` | key1 | 原生推送（{category,title,body,url,media_ids,...} → Message → 共享 ingest，CP3b） |
| `GET /api/v1/messages?since=&limit=` | key1 | 历史分页（HLC since，新→旧） |
| `GET /api/v1/messages/{hlc}` | key1 | 取单条 |
| `POST /media` / `GET /media/{id}` | key1 | 媒体上传/取（blob 文件系统 + metadata） |
| `POST /cursor` / `GET /cursor` | key1 | 阅读游标上报/接续（{view, focus_HLC}，覆盖式） |
| `GET/POST /api/v1/present_profile` | key1 | 呈现配置拉/改 |
| `GET /profile` | **key2** | 拉对端 profile（icon+别名） |
| `POST /share/{key2}` | key2 | 跨户投递（{category,title,body,from,media,target}） |
| `WS /stream` | key1 + uuid | 实时推：新消息 + profile_update |
| `ANY /{key}...` | 域内无 | bark 兼容皮（POST JSON / 路径式 / GET）；空 content 400 拒（CP3c，跟原生 push 必填对称） |

ServeMux 更具体优先：`/api/v1/*`、`/share/*`、`/messages/*`、`/register/*` 显式路由优于 `/` 兜底；bark 入口走 `/` 自解析首段为 device_key（UUID 与 "api"/"share"/"messages"/"register" 撞不上）。失配 method（如 `POST /messages/abc`、`GET /api/v1/push`）落 `/api/` `/share/` `/messages/` `/register/` 子树 → 404（防落 bark 兜底建 `TargetUUID="messages"` 等空消息污染 msgs，CP3b/c 跨层审）。

## 存储：BBolt + HLC

**HLC（Hybrid Logical Clock）= 消息标识 / 排序 / 补漏高水位 / 游标，全用它**（非自增 id、非裸时间戳）。HLC = pt（ns）+ counter 编码成整数；bbolt key 天然单调有序，省 msg_id 计数器。生成：`new_pt=now; if new_pt>last.pt:(new_pt,0) else:(last.pt,counter+1)`；last HLC 持久化。展示转 RFC3339。

桶 schema（全 KV）：

| 桶 | key | value（JSON） | 用途 |
|---|---|---|---|
| `device` | uuid | `{platform, push_token, type, name, created_at, updated_at, last_seen_at}` | 设备注册（type=phone/watch/... 显图标）；§9 砍 key_ver（紧急重置不搞无感轮换） |
| `msgs` | HLC（big-endian） | `{hlc, from, recipient, category, title, body, ts, media_ids, target_uuid, url, ext}` | 消息历史（CP3b 砍 PushSpec，直存 Message） |
| `cursor` | 单值 `keyCurrent`（global） | `{view, focus_HLC, reported_at}` | 阅读游标（覆盖式，多设备并发最新 reported_at 胜 LWW；**非 per-uuid**——单用户域共享一游标） |
| `media` | media_id | `{path, size, mime}` | 媒体 metadata（blob 在文件系统） |
| `keys` | 单值 | `{key1, key2}` | key1/key2 扁平（§9 砍 retired/retired_exp/key_ver，紧急重置不搞无感轮换） |
| `profile` | 单值 | `{infer[], rules[]}` | 呈现 profile（不透明 blob） |

**清理：纯空间阈值 + FIFO（单一机制）—— ⚠️ TD-13 未实装（CP3c 跨审 D P2）**
- 默认上限 **1024MB**（config + CLI 可配）。写消息后检查，超 → 从 HLC 最小（最老）删整条（metadata + blob）→ 回阈值。
- **当前 store 零实装**（grep `FIFO|prune|Compact` 无命中）——`max_bytes` 形同虚设，main.go 启动告警 `[WARN] FIFO eviction not implemented; max_bytes advisory`。Phase 2 实装（公网部署前必须，否则 bark 写开放灌满磁盘崩）。
- **无 TTL、无数量 retention、无分层**（文本几乎不占空间，瓶颈是多媒体 blob；空间够全留，满 FIFO 清最老）。
- bbolt 删留空洞 → 定期 `bbolt.Compact()`（启动检查 + 手动 `--compact`）。
- **已读状态砍**（通知中转是瞬时告知型）——无 `read:` 桶。

并发 MVCC（多读 + 单写）；Hotify 单用户零竞争。崩溃安全 ACID。

## 鉴权：key1（域内）+ key2（跨户）

**两层正交凭证（"域内 vs 跨户"，非"读 vs 写"）**：

- **key1 = 域内凭证**：本 server 设备调读端点（register/messages/media/cursor/stream/present_profile）带 key1。一 server 一用户，设备共享一个 key1。
- **key2 = 跨户凭证**：跨户对端推消息（`/share/{key2}`）+ 读 profile（`/profile`）带 key2。全局一个 key2。
- B 拿 share URL（带 key2）只能**推消息给 A**，注册不成 A 设备（没 key1）、拉不了 A 历史（读端点要 key1）。

**key1 空起始 first-set**：key1 起始为空；首设备 `/register` 不带 key1 → server first-set（设 + 下发）；之后所有 register/读端点必须带 key1。防抢注靠 first-set wins + 部署层兜底（抢注=可见 DoS + 无泄密 + SSH 删 config 重置）。

**register uuid 语义（token 刷新 vs 顶号，CP3c 跨审 C 文档化）**：register **不验 uuid 来源**——同 uuid 第二次 register 走 patch 语义覆盖第一次的 token（RegisterDevice 非空字段覆盖/空字段保留）。**契约：这是"token 刷新"合法**（设备重装/换机用同 uuid 续历史），**非"顶号漏洞"**——单用户可信域（App 自生成 UUIDv4，uuid 泄露概率低 + 泄露也只能刷自己 token、读端点仍要 key1 准入不窃历史）。跨户/不可信域要防顶号须加 uuid 来源验证（未来多户扩展再议）。**理论不出现**：可信域 + UUIDv4 碰撞概率忽略。

**key 轮换 = 紧急重置（不搞无感轮换）**：日常 key1/key2 不变。发现泄露/要换 → **CLI 重置**（清 key1/key2）→ 设备重新 `/register` first-set 新 key1 + 拿新 key2（接受短暂中断、所有设备重连）。**为什么不搞无感轮换**（retired 重叠期 + WS `key_update`）：key_update 走 WS 下发 = 任何持有效 key1 的连接都收新 key1，**泄露方在重叠期连上就跟着拿新 key1，轮换对它无效**——共享密钥 + WS 下发的固有缺陷，调时长解决不了。Hotify 单用户可信域 key 泄露概率低，无感卫生轮换价值小（YAGNI），紧急重置（罕见）够用且更简单：**无 retired/过期/key_update 帧/key_ver**，key 机制只剩 first-set + 紧急重置。

**部署：全端点公网 + 自主 ACME（2026-07-21 改）**：App 远程收通知 → 全端点本就公网可达（原"反代只放 `/share`、其余锁 LAN"对通知转发器不现实，作废）；**key1/key2 app 层准入让全端点公网 ≠ 裸奔**。server **自主 ACME**（autocert）终结 TLS，不走反代；反代/LAN 锁降级可选双保险。残留：bark `/{key}` 写开放（兼容可选）+ first-set 窗口可见 DoS（§9 接受）。

## 消息路由：全广播 + 设备自决

- **全广播为主**：WS 全广播所有在线设备 + PushKit 全广播（单用户量级频控不瓶颈）。砍 server 侧"类别→设备"静态映射。
- **类别降级为呈现 hint**：server 不路由类别，类别随消息透传，**设备自决呈现**（走 profile，见下）。
- **可选定向**：消息带 `target_uuid` → 只推那台。

## 业务 category 体系

**两层 category（别混）**：
- 华为 category：`notification.category=SUBSCRIPTION`（固定，PushKit 频控）。
- 业务 category：call/sms/verify/app_notify/history（Hotify 呈现语义）。走 WS 帧 `category` / PushKit `clickAction.data`（不碰 notification.category）。

**值集**：call/sms/verify/app_notify/history + default（预定义）。

**来源三层（server 保哑）**：①发送端显式填（原生 `/api/v1/push` 一等字段；bark 皮把 call/level/group 映射成 category）；②server 只格式归一、**不内容推断**；③没带 → 端侧按 profile infer 规则从 title/body 推。

## 呈现 profile（业务策略集中下发）

解"端侧呈现每端重写太重"——业务策略抽成 profile 集中，端侧跑通用引擎 + 硬件映射。**server 仍哑**（profile 不透明 blob 存 + 转发）。

- **L1 用户 profile（语义层，集中）**：`{infer[], rules[]}`，规则写语义动作（`alert:"strong"`，非 ring/vibrate）。
- **L2 设备硬件映射（端侧硬编码）**：strong → 手机响铃全屏 / 手表强震 / PC 置顶闪烁。
- **默认 profile**：Hotify 预置开箱即用（默认 infer + rules）；首次拉取无自定义 → 下发默认；高级可改。
- **API**：`GET/POST /present_profile`（key1）；改 → WS `profile_update` 广播 → 各端拉新。

## 跨户：share URL + self-copy

- **share URL**：`https://{server}/share/{key2}`（https 必须），手动粘贴。谁有 key2 谁能推。
- **self-copy**：A 发消息推两份——① → B server（投递，硬指标）② → A server（抄底，存历史 + 多设备同步，尽力）。media 各 POST 一份。
- `from` 自报（全局 key2 下无法可靠区分对端，接受）。

## Push Kit（鸿蒙引擎）

- **鉴权**：服务账号 RSA 私钥签 JWT（PS256）→ **JWT 直当 Bearer**（不换 access_token）。
- **推送**：`POST https://push-api.cloud.huawei.com/v3/{project_id}/messages:send`，header `push-type:0`，成功码 `80000000`。
- **category**：`notification.category=SUBSCRIPTION`（已过审）；**业务 category 走 `clickAction.data`**（见上）。
- **image mini**：≤192KB + https 挂 `notification.image`（http 被拒 80100003）。
- **region**：仅中国境内。自用 `testMessage=true` 绕 MARKETING 频控。
- **死 token**：80100000/80300007 删（全局闸门：本轮 ≥1 台成功才删）；notifyId 幂等；502/超时重试。
- 详见 `PUSHKIT.md` + `docs/pushkit-transport.md`。

## 多平台 adapter（初赛只鸿蒙）

鸿蒙 PushKit（day-one，搬 push.go）；iOS APNs / 安卓·Win WS 保活 后续按需。adapter 接口在出口层（统一出域 + 平台分发）。

## 商业模式（server 不参与 gating）

server 不碰 IAP（100% app 端华为 IAP Kit）。多端路由免费（gate 不住，自托管硬约束）；多端统一体验（历史 + 游标同步）= NEXT 层（app 端软 gate）。

## Phase（真实 vs TODO）

- ✅ 架构定稿（本文件 + `docs/NEXT-Server.md`）。
- ⬜ **Phase 1 MVP（垂直切片）**：bbolt + HLC + device registry + key1 first-set/校验 + bark 入口 + 一个 push 端点 + PushKit（搬 push.go）+ WS /stream + 鸿蒙 client register + 收一条 + **graceful shutdown**（SIGTERM 优雅断 WS，重启不丢消息）+ **配置启动校验**（早失败）。**端到端打通**（区别于桥）。
- ⬜ **Phase 2**：messages 分页（HLC since）+ cursor + media 分离 + image mini + profile/category + 全广播 + 死 token 清理 + **设备 DELETE 端点**（`DELETE /api/v1/devices/{uuid}`，换机/丢设备主动清）+ **备份/导出**（JSON 导出/导入 + bbolt backup 指引）。
- ⬜ **Phase NEXT**：跨户 share + self-copy + present_profile + 多平台 adapter。

## 依赖（零 CGO，保交叉编译单二进制）

- `go.etcd.io/bbolt`（BBolt，纯 Go KV）。
- Push Kit JWT：`crypto/rsa` + stdlib（PS256 自签）。
- HTTP：`net/http`（Go 1.22 ServeMux）；WS：`gorilla/websocket`（或 stdlib）。
- → `GOOS/GOARCH` 交叉编译单静态二进制，对标 bark-server 分发。
