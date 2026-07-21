# bark 协议权威 Spec（HotifyNEXT-Server 兼容皮参考）

> 来源：bark-server 源码（`C:/Users/littl/bark/bark/bark-server/`，本地已克隆的 Finb/bark-server v2），**不查文档站**。
> 每条结论标源码文件:行号。不确定标 ⚠️ 待验。
> 用途：CP3 `bark /{device_key}` 兼容端点实装的权威依据，对应 `internal/bark/bark.go` 升级。

---

## 1. bark 协议权威 Spec

### 1.1 HTTP 端点全集（路由注册）

源码 `route_push.go:19-39`（`init()` 注册）：

**V2 REST 端点**（`route_push.go:21-23`）：
- `POST /push` —— JSON 体带 `device_key`（或 `device_keys` 批量），主推端点。

**V1 兼容端点（路径式）**（`route_push.go:26-38`，`registerRouteWithWeight("push_compat", 1, ...)`）：
| 方法 | 路径 | 含义 |
|---|---|---|
| GET / POST | `/{device_key}` | 仅 key（body 走 query/form/JSON） |
| GET / POST | `/{device_key}/{body}` | key + body |
| GET / POST | `/{device_key}/{title}/{body}` | key + title + body |
| GET / POST | `/{device_key}/{title}/{subtitle}/{body}` | key + title + subtitle + body |

**路径式上限 4 段**：`/{key}/title/subtitle/body/extra` → **404**（`push_test.go:191-197` 实测验证；fiber 路由未注册该模式）。

**Content-Type 分派**（`route_push.go:45-55`，`routeDoPush`）：
- `application/json...` → `routeDoPushV2`（JSON 体解析 + query + path 三层合并）
- 其他（form/multipart/纯 query/路径式）→ `routeDoPushV1`

**GET 也支持**：所有路径式端点 GET/POST 双注册（`route_push.go:27-37`）。

### 1.2 参数合并优先级（关键）

**V1**（`route_push.go:57-91`，`routeDoPushV1`），低 → 高：
1. query args（`c.Request().URI().QueryArgs()`，`route_push.go:64`）
2. post args（form-urlencoded，`route_push.go:66`）
3. multipart form values（`route_push.go:68-75`）
4. **url path params（最高优先级）**（`route_push.go:77-83`，注释明写 "highest priority"）

> 含义：路径段里的 `title`/`body` 会覆盖 query/form 里的同名字段。所有 key 被小写化（`route_push.go:61`）。

**V2**（`route_push.go:92-109`，`routeDoPushV2`）：
1. JSON body（`c.BodyParser(&params)`，`route_push.go:95`）
2. query args（覆盖 body，`route_push.go:99-101`）
3. **url path params（最高，覆盖一切）**（`route_push.go:103-109`）

> 即 V2 POST /push 时若 URL 里也带 `/{key}/...`，路径值赢。但 `/push` 路径本身无 `:device_key` 捕获，实际 path params 为空——`device_key` 从 JSON body 或 query 取。

### 1.3 JSON / 参数字段全集

**顶层强类型字段**（`apns/apns.go:19-29`，`PushMessage` struct）：
| 字段 | JSON / form tag | 类型 | 语义 | 源码 |
|---|---|---|---|---|
| `id` | `id,omitempty` | string | 消息 ID（APNs `CollapseID`，去重/合并） | `apns.go:20` + `route_push.go:220-222` |
| `device_key` | `device_key,omitempty` | string | **设备路由 key**（非凭证，见 1.4） | `apns.go:22` |
| `device_token` | `-`（不暴露） | string | APNs token（server 内部，注册时存） | `apns.go:21` |
| `subtitle` | `subtitle,omitempty` | string | 通知副标题 | `apns.go:23` |
| `title` | `title,omitempty` | string | 通知标题 | `apns.go:24` |
| `body` | `body,omitempty` | string | 通知正文 | `apns.go:25` |
| `sound` | `sound,omitempty` | string | iOS 系统铃声名（自动加 `.caf` 后缀，`route_push.go:231-237`） | `apns.go:27` |
| `ext_params` | `ext_params,omitempty` | map | 扩展参数（透传给 APNs payload custom） | `apns.go:28` |

**除上述外所有字段 → 塞进 `ExtParams` 透传**（`route_push.go:238-247`，`switch` default 分支）。

**官方字段表**（`docs/API_V2.md:22-42`，bark-server 自带文档，非外部站）——除顶层外可传：

| 字段 | 类型 | 语义 | 备注 |
|---|---|---|---|
| `level` | string | `'critical'`/`'active'`/`'timeSensitive'`/`'passive'` | iOS 中断级别 |
| `volume` | string | critical 级铃声音量 | |
| `badge` | integer | App 图标角标数字 | |
| `call` | string | 必须 `1`——铃声持续 30s（来电） | |
| `autoCopy` | string | 必须 `1`——自动复制 | |
| `copy` | string | 要复制的值（配合 autoCopy） | |
| `icon` | string | icon URL（iOS 15+） | |
| `group` | string | 通知分组（→ APNs `ThreadID`，`apns.go:121-124`） | |
| `ciphertext` | string | 端到端加密密文（见 1.6） | |
| `isArchive` | string | 必须 `1`——App 端归档 | |
| `ttl` | integer | 归档消息存活秒数 | |
| `url` | string | 点通知跳转 URL | |
| `action` | string | `"none"`——点通知无动作 | |
| `device_keys` | array | 批量推送（V2 专属，`route_push.go:111-125`） | 多 key 并发 push |
| `delete` |（隐式）| `1` = 撤回（silent push），见 `apns.go:36-39`、`apns.go:110-113` | 走 `ContentAvailable` background push |

**字段小写化**：V1/V2 解析时所有 key 转小写（`route_push.go:61, 100, 128, 219`），故 `Title`/`TITLE` 等同 `title`。

**空消息兜底**（`route_push.go:254-257`）：title/body/subtitle 全空时，body 填 `"Empty Message"`（防 APNs 丢消息；对加密 push 尤其重要——body 是 APNs 必填）。

### 1.4 device_key：**仅路由，完全无鉴权**（核心结论）

**源码证据链**：
1. `push()` 函数（`route_push.go:208-276`）处理 `device_key` 仅一行查 token：
   ```go
   deviceToken, err := db.DeviceTokenByKey(msg.DeviceKey)  // route_push.go:259
   ```
   **无任何 token / secret / 签名 / HMAC 校验**。key 当 map key 查 value（token），查到就推、查不到报 400。
2. `push()` 全函数（`route_push.go:208-276`）grep 不到 `auth`/`secret`/`verify`/`hmac`/`sign` 任何字眼。
3. **可选的 Basic Auth 是部署层兜底，非协议要求**（`route_auth.go:12-34`）：
   - `if user == "" && passwd == ""` → 直接 `return`（默认关闭，`route_auth.go:13-16`）。
   - 开启后用 fiber `basicauth` middleware 全局拦（`route_auth.go:33`，`router.Use("/+", basicAuth)`），但对 `/ping`/`/register`/`/healthz` 放行（`route_auth.go:19, 24-28`）。
   - 启动 flag `--user` / `--password` 默认空字符串（`main.go:266-276`），即默认部署 **完全无鉴权**。

**结论**：bark 协议本身**无 key/secret 校验**——`device_key` 仅是路由用的 opaque string（bark App 注册时 server 生成 UUID，`route_register.go:60`，`SaveDeviceTokenByKey` 空 key 时填新 UUID）。Basic Auth 是部署管理员自选的外层保护，跟 bark 协议无关。

> HotifyNEXT §19 写端点分层定调 "bark `/{device_key}` 完全无鉴权——bark 协议本质" **与源码完全吻合**，CP3 bark 端点不加 key1。

### 1.5 响应格式

**`CommonResp` struct**（`router.go:19-24`）：
```go
type CommonResp struct {
    Code      int         `json:"code"`
    Message   string      `json:"message"`
    Data      interface{} `json:"data,omitempty"`
    Timestamp int64       `json:"timestamp"`
}
```

**Code 值集**（源码统计）：
- `200` 成功（`success()`，`router.go:86-92`）
- `400` 客户端错误（device key 空 / device token 获取失败 / 路径解析失败 / 批量超限，`route_push.go:79, 87, 96, 105, 122, 133, 140, 251, 261`）
- `500` 服务端错误（push 失败 / 注册失败，`route_push.go:273`、`route_register.go:63`）
- `410` APNs 返回 token 失效（`route_push.go:269`，触发删 token）
- HTTP 层：`418 I'm a teapot`（Basic Auth 未过，`route_auth.go:29`，非标准但 fiber 支持）

**成功响应**（`route_push.go:89`）：
```json
{"code":200,"message":"success","timestamp":1700000000}
```

**失败响应**（`route_push.go:87`）：
```json
{"code":400,"message":"failed to get device token: ...","timestamp":1700000000}
```
HTTP status 与 body.code 一致（`c.Status(code).JSON(failed(code, ...))`）。

**批量推送响应**（`route_push.go:174`，`data(result)`）：`code:200` + `data:[{code, message, device_key}, ...]`，每设备独立结果。

**特别**：customErrorHandler（`main.go:82-92`）所有未捕获错误也走 `CommonResp` 格式，code = HTTP status。

### 1.6 加密机制（端到端，App 持 key，server 只透传）

**bark-server 自身不做加密/解密**：
- 全仓库 grep `ciphertext|encrypt|RSA|AES` —— 仅命中：
  - `docs/API_V2.md:38`（字段表描述）
  - `push_test.go:201-235`（`TestCiphertext`，只测能接收 `ciphertext` 参数不报错）
  - `route_push.go:255`（注释，解释为何空 body 兜底——加密 push 也需要 body 防 APNs 丢弃）
  - `apns_certs.go:157,191`（TLS 根证书名，无关）
- **`ciphertext` 走 `ExtParams` 普通透传**（`route_push.go:238-239` default 分支），原样塞进 APNs payload custom 字段（`apns.go:127-130`，`pl.Custom(k, fmt.Sprintf("%v", v))`）。

**机制**（从字段表 + 透传逻辑推）：发送端用 App 持有的对称密钥加密 body 得 `ciphertext` + `iv`，server 收到后**不解密**，原样推给设备；App 端用本地密钥解密渲染。这是 App↔发送端的端到端约定，server 是不透明的中继。

> Hotify §9 定调"全程明文（暂定）"——**与 bark 加密机制不兼容**。CP3 bark 端点**接收 `ciphertext` 字段但不解密**（透传进 PushSpec.Body 或单独留痕），App 端无 bark 的解密 key 也无法渲染——属"bark 兼容皮能做到子集"之外，标取舍。

### 1.7 注册端点（device_key 生成）

`route_register.go:17-27`：
- `POST /register`（body：`device_key?`+`device_token`，`route_register.go:19`）
- `GET /register/:device_key`（检查 key 是否注册，`route_register.go:20`）
- `GET /register?devicetoken=...`（老兼容，`route_register.go:25`）

`device_key` 空时 server 生成新 UUID（`route_register.go:58-60`，`SaveDeviceTokenByKey` 空入参填 UUID）。`device_token` 必填、长度 ≤160（`route_register.go:54-56`）。

### 1.8 其他端点

`route_misc.go:10-44`：
- `GET /` → `"ok"`（健康检查字符串）
- `GET /ping` → `{code:200, message:"pong", timestamp:...}`
- `GET /healthz` → `"ok"`
- `GET /info` → `{version, build, arch, commit, devices}`

---

## 2. HotifyNEXT 字段映射表（bark → model.Message）

> CP3b 砍 PushSpec 中间层（bark 解析直出 Message）；CP3c 字段归宿声明式表 `barkFieldRules`（`internal/server/bark.go`）。
> 三类（Ext 留底纪律）：一等字段（强类型 Message）/ Ext 留底（无映射目标）/ 真·丢（端侧 profile + §9 §14 取舍）。

对照 `internal/model/model.go` Message + `docs/NEXT-Server.md` §6/§13b。

| bark 字段 | bark 语义 | Hotify 映射目标 | 说明 |
|---|---|---|---|
| `device_key`（路径段 0 或 body） | 设备路由 | `Message.TargetUUID`（按 key 查 Device） | bark key 当 uuid 查 `store.GetDevice(key)`（现有 `bark.go:75` 已这么做）；查不到落历史不推（§7 全广播未来扩，CP6） |
| `title` | 通知标题 | `Message.Title`（一等） | 直映 |
| `body` | 正文 | `Message.Body`（一等） | 直映；body 含斜杠用 `%2F` URL 编码（bark-server §1.1 源码无 `segs[2:]` 合并，旧 `bark.go:53` 误解已删，CP3c） |
| `subtitle` | 副标题 | ⚠️ 待验（PushSpec 无字段） | 鸿蒙 PushKit `notification` 无副标题槽；可拼进 Body 或丢弃。**PushSpec 暂不收**，CP3 标取舍 |
| `sound` | iOS 铃声 | 端侧呈现 profile（§13b L1） | **须 App 预置 rawfile 音频**（§6 bark 功能映射）；server 不下音频。映射进 `category`/profile 的 alert 策略，不进 PushSpec 一等字段 |
| `badge` | 角标 | **真丢**（CP3c） | App 图标全局状态，每条存无意义（§1.3 integer，Hotify 无处用） |
| `call`（=`1`） | 来电持续响铃 | `Message.Category="call"`（归一） | §6/§13b 最干净的类型映射；原生 category 一等字段。优先级 call > group > default；非 "1"（如 "yes"）不触发 |
| `level`（`critical/active/timeSensitive/passive`） | iOS 中断级 | **`Ext["level"]`（留底，CP3c 不映射 category）** | CP3c：中断级别 vs 业务分类语义正交，硬映射是垃圾映射；留 Ext 待未来 `Message.Level` alert 字段。bark-server 映射近似非 1:1（§6 critical 鸿蒙无真穿透），Hotify 接受不靠硬映射补。category 归一变 call > group > default（level 退出） |
| `group` | 通知分组 | `Message.Category=group值`（归一） | §6：用户在 SmsForwarder 按 `group="verify"` 配 → category=verify（挪用当分类标签）；TrimSpace；优先级低于 call |
| `url` | 点击跳转 | `Message.URL`（一等）→ PushKit `clickAction.data`（§附录 A 必填，CP4 用） | CP3c 两边（bark+native）裸收未校验，协议白名单延 CP4（TD-12） |
| `icon` | icon URL | **鸿蒙 N/A** | §6 bark 不能做：iOS iMessage 专属，鸿蒙无；§15 profile API 是 App 内补全（锁屏做不到） |
| `image`（bark v3 新增，⚠️ 待验） | 通知图 | **`Ext["image"]`（留底，待 Phase 2）** | CP3c：字段名待验 + media 拉取/mini 存储超 CP3c 范围（Phase 2 media 端点才做）；留 Ext 待 Phase 2 映射 MediaIDs |
| `copy` + `autoCopy` | 自动复制 | **鸿蒙 N/A** | §6：鸿蒙无后台剪贴板写入；丢弃 |
| `ciphertext` + `iv` | 端到端加密 | **不兼容** | §9 全程明文；CP3 接收字段但不解密、App 无 bark key 无法渲染——标取舍 |
| `action=none` | 点击无动作 | ⚠️ 待验 | PushKit clickAction 必填，无法"无动作"；CP3 标取舍 |
| `isArchive` + `ttl` | App 端归档 | 无（Hotify 历史永久，§14 不砍） | Hotify 默认全归档，bark 字段冗余 |
| `delete`（=`1`） | 撤回 | 撤回 API（§6） | CP3 暂不实装撤回；留口 |
| `id` | 去重 ID | HLC（§7，Hotify 用 HLC 不用 id） | bark 的 id 当元数据留痕即可，去重靠 HLC |
| `device_keys`（数组） | 批量 | 多次落库 + 多 push | CP3 可支持（bark V2 特性），低优先级 |
| `volume` | critical 音量 | profile alert | 同 sound，进端侧呈现策略 |

---

## 3. 兼容边界（能做 / 做不到 / 不做）

### ✅ 完全无鉴权（device_key 仅路由）—— 源码确认
- `route_push.go:259` 仅 `DeviceTokenByKey` 查 token，**无任何 secret/token/hmac/sign 校验**。
- `route_auth.go:13-16` Basic Auth 默认关闭（`user=="" && passwd==""` 直 return）；开启也只对 `/+` 全局拦，是部署层保护非协议要求。
- **CP3 bark 端点不加 key1**（§19 写端点分层已定调，与源码吻合）。

### ✅ 能映射进 Message（bark 兼容皮能做到的子集）
- `title` / `body` / `device_key`（→ TargetUUID）—— 直映，现有 `bark.go` 已做。
- `call` → `category = "call"`（§13b 业务 category 来源第 1 条）。
- `group` → `category`（挪用当分类标签，§6，TrimSpace）；`level` → `Ext` 留底**不映射 category**（CP3c，中断级别 vs 业务分类语义正交）。
- `url` → `Message.URL` → PushKit `clickAction.data`（§附录 A 必填；协议白名单延 CP4，TD-12）。
- `image` → `Ext` 留底（CP3c：字段名待验 + media 拉取超 CP3c，Phase 2 media 端点）。
- `sound` → 端侧呈现 profile（须 App 预置，不进 PushSpec 一等字段）。

### ❌ 做不到（鸿蒙/系统限制，§6 bark 不能做）
- `icon`（自定义通知图标）—— iOS iMessage 专属，鸿蒙无；§15 profile API 是 App 内补全。
- `autoCopy` + `copy`（自动复制验证码）—— 鸿蒙无后台剪贴板写入。
- markdown 渲染 —— 鸿蒙系统通知不渲染 markdown。
- 锁屏发件人头像 —— PushKit 无此机制（§15）。
- `level = critical` 真穿透 —— 鸿蒙无免打扰响铃穿透；伪 critical = 系统关键词置顶（视觉非听觉）。

### ❌ 不做（Hotify 取舍）
- bark 端到端加密（`ciphertext`/`iv`）—— §9 全程明文，不兼容；接收字段但不解密、App 无 bark key 无法渲染。
- `id` 去重 —— Hotify 用 HLC（§7），bark id 留元数据即可。
- `isArchive`/`ttl` —— Hotify 历史默认全存（§14），字段冗余。
- `action=none` —— PushKit clickAction 必填，无法"无动作"（⚠️ 待验：或可用占位 data）。

### ⚠️ 待验（CP3 实装前需二次确认）
- `subtitle` 鸿蒙呈现方案（拼进 Body / 丢弃 / PushSpec 加字段）。
- `badge` 是否留 ext 透传。
- `image` 字段确切名（API_V2.md 未列，需查 bark App iOS 源码）。
- `action=none` 在 PushKit 的等价处理。

---

## 4. CP3 实装要点（不写代码，列清单）

对照现有 `internal/bark/bark.go`（仅 title/body、category 硬编 default），CP3 补齐：

### 4.1 端点解析（严格按 bark）
- 支持 4 种路径式：`/{key}`、`/{key}/{body}`、`/{key}/{title}/{body}`、`/{key}/{title}/{subtitle}/{body}`（`route_push.go:26-38`）；超过 4 段 → 404（`push_test.go:191-197`）。
- GET / POST 双支持（`route_push.go:27-37`）。
- **Content-Type 分派**（`route_push.go:45-55`）：JSON 体走 V2 解析、其余走 V1。
- **参数合并优先级**（`route_push.go:77-83, 103-109`）：path > query > form > JSON body。低优先级不能覆盖高优先级（现有 `bark.go` 只取 path 或 JSON，未合并 query/form，**缺口**）。
- **body 含斜杠用 `%2F` URL 编码**（CP3c）：bark-server §1.1 源码**无 `segs[2:]` 合并**（旧 `bark.go:53` 的合并是误解，已删）；body 含 `/` 靠 `%2F` 编码 + `handleBark` 用 `r.URL.EscapedPath()` 保留段内 + `splitBarkPath` 逐段 `QueryUnescape` 还原。
- **字段名小写化**（`route_push.go:61`）：解析时 key 全部转小写再匹配（`Title`/`TITLE` 同 `title`）。
- URL 解码 path params（`route_push.go:184-204`，`url.QueryUnescape`）。

### 4.2 字段全集解析（进 PushSpec 前的归一 map）
- 顶层：`title`/`body`/`subtitle`/`sound`/`device_key`/`id`（`apns.go:19-29`）。
- 其余全部进 ExtParams-style map（`route_push.go:238-247`），包括 `level`/`group`/`call`/`url`/`icon`/`image`/`badge`/`ciphertext`/`copy`/`autoCopy`/`action`/`volume`/`isArchive`/`ttl`/`delete`/`device_keys`。
- **空 content 400 拒**（CP3c，跟原生 push 必填对称）：title/body/subtitle 全空 → 400 拒不落库。bark-server 空 body 兜底（`route_push.go:254-257` 填占位防 APNs 丢）是 **APNs 必填 body 的 hack**——Hotify 无 APNs 不继承；扫描器（`/favicon.ico`/`.env`/`wp-admin`）/空请求全拒，不污染历史。

### 4.3 device_key 路由 + 无鉴权
- `device_key` 当 uuid 查 Device（现有 `bark.go:75` `GetDevice(key)` 已这么做）。
- **无 key1/secret 校验**（`route_push.go:259` 仅查 token；§19 已定调）。
- 查不到设备：落历史 + 留痕不推（现有 `bark.go:83-85` 已做；CP6 扩全广播扇出）。

### 4.4 归一进 PushSpec（§6 主从：bark 是从）
- `call == "1"` → `Category = "call"`。
- `group` 非空 → `Category = group`（挪用，用户配 `"verify"`/`"sms"` 即命中业务 category；§6/§13b）。
- `level` 非空 → **不映射 category**（CP3c：中断级别 vs 业务分类语义正交，硬映射是垃圾映射）→ 留 `Ext["level"]` 待未来 `Message.Level` alert 字段。category 归一变 **call > group > default**（level 退出优先级链）。
- `url` → clickAction.data（PushKit 必填，§附录 A）。
- `image`（⚠️ 待验字段名）→ 拉 mini ≤192KB → MediaIDs 单元素。
- 其余（sound/badge/volume）→ 端侧呈现 profile 元数据，不进 PushSpec 一等字段。
- 无 category 命中 → `Category = "default"`（§13b 兜底）。

### 4.5 响应严格 bark 风格
- 成功：`{"code":200,"message":"success","timestamp":...}`（`router.go:86-92`）。
- 失败：`{"code":<400/500>,"message":"...","timestamp":...}`（`router.go:95-101`），HTTP status 与 code 一致。
- 现有 `bark.go:96-99` 的 `resp{Code, Message}` **缺 `timestamp` 字段**——**缺口**，CP3 补 `Timestamp int64`。
- 批量推送（`device_keys`）：`code:200 + data:[{code,message,device_key}]`（`route_push.go:174`）—— CP3 可选，低优先级。

### 4.6 `bark.go` 九缺口清单（**CP3c 已全补齐**，commit 6652fdd，下述为 CP1-temp 旧状历史记录）
1. **缺 query/form 参数合并**（只解析 path 或 JSON，漏 query/form- 现有 `bark.go:50-59`）。
2. **缺字段全集**——只解析 `{title, body}`，漏 `level/group/call/url/image/sound/icon/...` 全部 ExtParams 字段。
3. **category 硬编 `"default"`**——CP3 应按 `call/group/level` 归一映射（§4.4）。
4. **缺 `subtitle` 路径段**——`/{key}/{title}/{subtitle}/{body}` 4 段式未支持（现有 `bark.go:51-53` 只到 3 段）。
5. **缺响应 `timestamp` 字段**（§4.5）。
6. **缺 `/{key}/{body}` 2 段式**（现有 `bark.go:51` 直接 `len(segs) >= 3`，2 段时 body 走 JSON 解析但 GET 无 body——GET `/{key}/body` 会把 "body" 当 title；**bug**，需按段数分派）。
7. **缺 Content-Type 分派**（JSON vs form vs query，`route_push.go:45-55`）。
8. **缺字段名小写化**（`route_push.go:61`）。
9. **缺 URL 解码** path params（`route_push.go:184-204`）。

---

## 附录：源码索引

| 文件 | 关键内容 |
|---|---|
| `bark/bark-server/route_push.go:19-39` | 路由注册（V2 `/push` + V1 路径式 4 段） |
| `bark/bark-server/route_push.go:45-91` | V1 解析（query/form/multipart/path 合并优先级） |
| `bark/bark-server/route_push.go:92-176` | V2 解析（JSON body + query + path + device_keys 批量） |
| `bark/bark-server/route_push.go:208-276` | `push()` 核心逻辑（device_key 仅查 token、无鉴权、空 body 兜底） |
| `bark/bark-server/apns/apns.go:19-29` | `PushMessage` struct（顶层强类型字段） |
| `bark/bark-server/apns/apns.go:107-150` | APNs payload 构造（ExtParams 透传、group→ThreadID、delete→background） |
| `bark/bark-server/router.go:19-24, 86-111` | `CommonResp` + `success/failed/data` 响应工厂 |
| `bark/bark-server/route_auth.go:12-34` | Basic Auth（默认关闭、部署层非协议） |
| `bark/bark-server/route_register.go:17-87` | 注册端点（device_key 空→UUID） |
| `bark/bark-server/route_misc.go:10-44` | `/ping`、`/healthz`、`/info`、`/` |
| `bark/bark-server/docs/API_V2.md:22-42` | 官方字段表（title/body/level/volume/badge/call/autoCopy/copy/sound/icon/group/ciphertext/isArchive/ttl/url/action） |
| `bark/bark-server/push_test.go:71-235` | 测试用例（路径式上限 4 段、ciphertext 透传、GET/POST/JSON 全支持） |
