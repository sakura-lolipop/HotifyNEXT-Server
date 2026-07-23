# HotifyNEXT-Server · tech-debt

> CP 只做功能不夹带重构（用户纪律，见 `coding-strong-policy-weak`/`over-engineering-tendency`）；重构/拆分/cleanup 排 Phase 2 批次做。本文件记已知债，防忘。

## 清理排期（2026-07-21 定，屎山审查后）

**原则**：会长的债（compound / error-prone）下一个 CP 开头清，别让它长；不长的静态 DRY 批到 cleanup 批次；自然发生的随流清。

| 批次 | 债 | 何时 | 理由 |
|---|---|---|---|
| **CP3 开头** | TD-3（envelope 类型化） | 先建 `apiResp` + `writeAPIError` 再写 CP3 新端点 | CP2 已 5 处临界，CP3 端点（`/api/v1/messages`/`push`/`media`）翻倍；状态码双写是漏改源——边写边欠新债 |
| **CP3 流里** | TD-4（410 双胞胎 + device helper）+ **TD-5（bark.go 9 缺口，CP3 bark 兼容本职）** | TD-4 随 `/read` 删 + device 写方法抽 `mutateDevice`；**TD-5 = CP3 bark 兼容层按 `bark.md` §4 重写（含 2 段路径 bug）** | TD-5 非 cleanup、是 CP3 本职，列此追踪 9 缺口 |
| **Phase 2 cleanup 批次** | TD-1（文件拆分）+ TD-2（saveKeys DRY）+ TD-10（SaveMessage 签名）+ TD-11（pk→pusher 字段名）+ TD-13（FIFO 空间阈值实装） | CP3 端点稳定后 | TD-1 前置依赖 CP3（现在拆=churn）；静态 DRY/命名不 compound，批处理省 context-switch；TD-13 跨审 D P2（max_bytes 声明零实装，公网磁盘崩风险） |
| **MVP 后工程化批次** | TD-6（CI + golangci-lint）+ TD-7（版本注入） | CP6（Phase 1 MVP 收尾）后 | 迭代期 churn 快 CI 噪声大反碍事；MVP 稳定后上自动门防回归 + 分发排错要版本号（2026-07-21 架构评估记） |
| **CP4 批次** | TD-12（msg.URL 协议白名单） | CP4（pushkit 搬，接 clickAction 前） | CP3c 两边（bark+native）裸收 url 未校验；CP4 pushkit 喂 clickAction.data 前必须加协议白名单（http/https/app scheme）防 javascript:/file: 跳转（XSS/钓鱼/本地文件泄露，CP3c 对抗审 B P2） |
| **CP6 批次** | TD-8（client 契约文档）+ TD-9（fanoutPush sentinel） | CP6（部署就绪） | client 接 API 要错误码契约文档；fanoutPush 全广播扇出前补 sentinel 防五分支语义糊（CP3b 屎山审 P2-3） |
| **CP4 审查延后** | TD-14（同步 push 阻塞/异步化）+ TD-15（5xx 重试范围）+ TD-16（harmony 小项）+ TD-17（补测） | 各自触发条件（见下） | 都不 compound（无人在其上建上层假设）；TD-14/15 撞事故/实测卡顿再动；TD-16/17 摸 harmony.go 时随流。**P1（notifyId 幂等）CP4 即时修不入库**——注释自称已做实际空实现，CP5/CP6 要建在其上，属"假地基"必须现在清 |

**不清在 CP2**：CP2 已验证完成（`go test ./...` + `go vet` 全绿），混入重构违反"不混改"纪律；TD-1 现在 do 不了（CP3 端点重排依赖）；TD-3 在 CP3 开头做时上下文一样热（CP3 就是写端点、碰 envelope 的时候），现在做无 warmth 优势反冒"改坏已验证代码"险。

**CP3 启动 checklist**：① 先翻本文件 → ② 清 TD-3（建 envelope 类型）→ ③ 写端点过程中随流清 TD-4。

## CP4 后 tech-debt 现状（2026-07-22 落盘）

**CP4 已清**：TD-3（envelope，CP3）/ TD-4（mutateDevice+ClearPushToken，CP4）/ TD-5（bark 9 缺口，CP3）/ TD-12（sanitize，CP4）/ TD-15（5xx retry 放宽，CP4）/ TD-16（harmony 小项 P3-1/P3-2，CP4）/ TD-17（补测，CP4）/ TD-19（handleHistory HLC MessagesSince since=0，CP4）。

**待清**（按触发 + 性质）：

| TD | 触发 | 性质 |
|---|---|---|
| TD-1 文件拆分 | Phase 2 cleanup | 静态 DRY，不炸 |
| TD-2 saveKeys DRY | Phase 2（跟 TD-20 truncate/orVal 同批） | 静态 DRY |
| TD-6 CI + golangci-lint | CP6（MVP 后） | 回归门 |
| TD-7 版本注入 | CP6 后 / 分发 | 排错 |
| TD-8 client 契约文档 | CP5 WS 帧契约已沉淀 stream.go 注释，错误码完整文档仍 CP6 | client 契约 |
| TD-9 fanoutPush sentinel | **CP6（全广播扇出前必须）** | **会炸**（五分支语义糊） |
| TD-10 SaveMessage 签名 | CP5/CP6 refactor | 跨 CP 签名 |
| TD-11 pk→pusher | Phase 2（跨审已改 pusher） | 命名 ✅? |
| TD-13 FIFO 空间阈值 | **Phase 2（公网部署前必须）** | **会炸**（bark 写开放灌磁盘崩） |
| TD-14 异步推送 | CP6（公网前评估，默认不异步） | 会炸（引并发坑，屎山 agent 说现在清反造屎山） |
| TD-18 cloud_function_urls 自动拉 | CP5/CP6（zero-config pushkit） | 便利 |
| TD-20 harmony 屎山（doPost 返回值/magic 16/truncate-orVal） | Phase 2（摸 harmony.go 随流） | 静态 DRY/小修 |

**会炸的（公网前必清，触发明确）**：TD-13 FIFO（磁盘）/ TD-9 fanoutPush sentinel（CP6 全广播）/ TD-14 异步（CP6 评估）。
**静态 DRY/小修（不炸，Phase 2 批）**：TD-1/2/10/11/20。
**工程化（CP6 后）**：TD-6 CI / TD-7 版本。

## 债

### TD-1 store.go / server.go 文件拆分（CP2 后记，2026-07-21）
- **现状**：
  - `internal/store/store.go` ~800 行——HLC 纯函数 + Store interface + BBolt 全域（device/msgs/media/cursor/keys）+ Memory 全域 + CP2 鉴权方法（AuthorizeRead/ResolveRegisterKey/loadKeys/key1Matches）全混在一个文件。
  - `internal/server/server.go` ~200 行——所有 handler（legacy register / native register / history / read-deprecated）+ 路由装配 + helper（writeJSON/logReq/mask）。
- **为什么不现在拆**：CP2 不夹带重构（用户忌讳混改）；server.go 端点 CP3 要重排（`/messages/{key}`→`/api/v1/messages` + 加 `/api/v1/push`），现在拆 CP3 又拆 = churn。
- **拆法建议**（Phase 2 cleanup）：
  - store：`hlc.go`（HLC 纯函数，已是自洽单元）+ 按实现 `bbolt.go`/`memory.go`，或按域 `device.go`/`msgs.go`/`media.go`/`cursor.go`/`keys.go`（keys 含鉴权决策，安全内聚，单独成文件好审）。
  - server：`register.go`/`history.go`/`push.go`/`media.go`（按端点组，CP3 native 端点稳定后）。
- **触发**：Phase 2 cleanup 批次，或任一文件破 1000 行 / 一类方法翻不到时。

### TD-2 keys 写入重复 + saveKeys helper（屎山审查 #6，2026-07-21）
- **现状**：`SetKey1FirstSet`/`EnsureKey2`/`ResolveRegisterKey` 三处重复 `loadKeys → 改 KeyN → Marshal → Put` 骨架；错误变量命名不一致（前两者 `err` shadow，`ResolveRegisterKey` 用 `marshalErr`/`putErr`）。
- **修**：抽 `saveKeys(tx, keys model.Keys) error`（Marshal + Put 两行），各处变 `return saveKeys(tx, keys)`；命名统一（短作用域用 `err` shadow 更地道）。
- **触发**：Phase 2 cleanup（CP1+CP2 已测代码，不动功能；~10 行）。

### TD-3 JSON envelope 类型化（屎山审查 #2/#3/#9，2026-07-21）—— **CP3 必做**
- **现状**：`{code, message}` envelope 用 inline `map[string]any` 散落 5+ 处（auth.go/server.go），状态码双写（`http.StatusXxx` + `"code": N`，改一处易漏另一处）；`bark.go` 与 `server.go` 各一份同构 `writeJSON` + `resp`/envelope struct。
- **为什么 CP3 必做**：CP2 ~5 处已临界，CP3 端点重排（`/api/v1/messages`/`push`/`media`）后翻倍，越晚抽成本越高；状态码双写是漏改高发点。
- **修**：定义 `apiResp struct{Code int; Message string}` + `writeAPIError(w, status, msg)` helper；bark 的 `writeJSON`/`resp` 合并进 server（CP3 bark 可能搬进 server 包时自然合并）。
- **触发**：CP3 端点重排时一并做。

### TD-4 410 双胞胎 handler + device patch-update helper（屎山审查 #1/#4，2026-07-21）
- **410 合一 ✅（CP3a 完成）**：`handleMarkRead`/`handleReadSet` 双胞胎合一成 `handleReadDeprecated`（Go 1.22 `/read/{key}` 匹配全方法）。**保留路由返 410 不删**——CP3a 审查 P3 发现删了会让旧 App /read 落 bark 兜底（read 当 device_key）灌空消息；410 给明确废弃信号。
- **mutateDevice helper ⬜（延 Phase 2）**：`RegisterDevice`/`TouchDeviceSeen` 的 `Get→Unmarshal→改→Marshal→Put` 骨架重复，待加 `UpdateDeviceName`/`ClearPushToken` 等新 device 写方法时抽 `mutateDevice(tx, uuid, func(*model.Device))`；CP3 未加新 device 写方法 → 延 Phase 2 cleanup。

### TD-5 bark.go 9 缺口（CP3 bark 兼容层本职，2026-07-21）
- **现状**：`internal/bark/bark.go`（CP1-temp 最小实现）只解析路径式 + JSON {title,body}、category 硬编 default。`bark.md` §4.6 依 bark-server 源码列 9 缺口：①缺 query/form 参数合并 ②缺字段全集（level/group/call/url/image/sound/icon/…）③category 硬编 default ④缺 4 段式 `/{key}/{title}/{subtitle}/{body}` ⑤响应缺 timestamp ⑥**bug：缺 2 段式 `/{key}/{body}`——现 `len(segs)>=3` 把 GET `/{key}/body` 的 body 当 title** ⑦缺 Content-Type 分派 ⑧缺字段名小写化 ⑨缺 URL 解码。
- **性质**：**CP3 bark 兼容层的实装范围（非 Phase 2 cleanup）**——CP3 按 `bark.md` §4 重写 bark.go 全部补齐；#6 bug 也 CP3 修（CP1-temp 不完整 bark 是预期的，不单独 hotfix）。
- **触发**：CP3（bark+原生双入口归一 PushSpec）。
- **依据**：`bark.md` §4.6（bark-server 源码出处 `route_push.go`/`apns.go`/`router.go`）。

### TD-6 CI + golangci-lint（2026-07-21 架构评估记）
- **现状**：`go test ./...` + `go vet` 手动跑；无 `.github/workflows`；无 linter 配置。
- **为什么 MVP 后才加**：迭代期代码 churn 快，CI 门噪声大反碍事；MVP 稳定后上自动门防回归才划算。
- **怎么修**：GitHub Actions（`go test` + `go vet` + build 交叉编译矩阵 GOOS/GOARCH）+ golangci-lint（启用 errcheck/govet/staticcheck/unused）。
- **errcheck 定位**：**防漏第二道保险**，不是根除手段。返回值不吞的根除靠写代码纪律（CLAUDE.md ④，CP1/CP2/CP3 实现时已守——见 plan「CP3 实现硬纪律」）——人难免漏一个 `_ =`，errcheck 自动抓兜底。别把"CI 抓吞错"当借口放松写代码时的纪律。
- **触发**：CP6（Phase 1 MVP 收尾）后第一批。

### TD-7 版本注入（2026-07-21 架构评估记）
- **现状**：无 version/commit；bark-server `/info` 返 `{version,build,arch,commit}`（route_misc.go），Hotify 无。
- **怎么修**：`go build -ldflags "-X main.version=... -X main.commit=$(git rev-parse --short HEAD)"` + 加 `/info` 端点对齐 bark-server。分发 release / 排错（用户报 bug 知道跑哪个版本）用。
- **触发**：分发起（CI 发 release 时）/ CP6 后。

### TD-8 client 契约文档（错误码表，2026-07-21 CP3b 功能审 P2-2 + 用户提）
- **现状**：错误响应散在代码（writeAPIError/writeBark + message 常量 msgSaveFailed/msgPushFailed/msgSuccess），无 client 文档。client 解析响应排查问题缺契约。
- **关键坑**：**200≠推送成功**——`code:200 + message="saved but push failed: ..."` 表消息已落库但推送失败，client 只看 code 会误判（push.go 注释已标）。
- **怎么修**：CP6 写完整 client 契约（所有端点 + 错误码表 400/401/404/405/410/415/500 + message + 字段 + 请求格式），放 NEXT-Server.md §2 扩。
- **触发**：CP6（部署就绪，client 接 API 时）。

### TD-9 fanoutPush sentinel error（2026-07-21 CP3b 屎山审 P2-3）
- **现状**：fanoutPush 五分支返 nil/err 靠注释区分语义（no-target/device-not-found/空-token 留痕 nil vs 内部错 err），无 sentinel。handler 无法区分"合理留痕"vs"真出错"。
- **怎么修**：CP6 全广播扇出前，fanoutPush 返 sentinel（ErrDeviceNotFound/ErrNoTarget）或结构体，让 handler/运维区分。CP6 加全广播分支会让 switch 从 4→5+ case，不补会糊。
- **触发**：CP6（全广播扇出前必须）。

### TD-10 SaveMessage 签名（2026-07-21 CP3b 屎山审 P3-1）
- **现状**：`SaveMessage(msg) (uint64, error)` 返 hlc 不返 msg，调用方手动 `msg.HLC = hlc` 回填（push.go ingest，值传递根因——store 改副本）。
- **怎么修**：`SaveMessage(msg *model.Message) error` 或 `(model.Message, error)` 返刷新后的 msg。
- **触发**：CP5/CP6 refactor（跨 CP store 签名变更）。

### TD-11 pk→pusher 字段名（2026-07-21 CP3b 屎山审 P3-6）
- **现状**：`Server.pk` 字段名偏紧（2 字母），`pusher` 更可读（8 处引用）。
- **触发**：Phase 2 cleanup（改名机械，批处理）。

### TD-12 msg.URL 协议白名单（2026-07-21 CP3c 对抗审 B P2）
- **现状**：`msg.URL`（bark `url` + 原生 `url` 两边都收）**无协议白名单 / 长度上限 / host 校验**。CP4 计划把 `msg.URL` 喂给 PushKit `clickAction.data`——鸿蒙端点通知跳转。
- **风险**：`javascript:` 协议（webview 场景 XSS）/ `file://` `content://`（本地文件泄露）/ `http://attacker.com`（钓鱼）。鸿蒙 PushKit clickAction 的 URL 处理语义 CP4 待验。
- **为什么 CP3c 不修**：CP3c 是解析层（bark/native 收 URL 进 Message.URL），URL 校验是推送层（CP4 pushkit 喂 clickAction 时）。clickAction 语义未定（CP4 验），现在加白名单可能错（万一鸿蒙支持自定义 app scheme）。归 CP4 验证后定。
- **怎么修**：CP4 pushkit 接 `msg.URL` 前加协议白名单（http/https/自定义 app scheme 才放行，长度 ≤ 2048），bark 和 native 共用同一 sanitize helper（放 response.go 或新建 sanitize.go）。
- **触发**：CP4（pushkit 搬 legacy `hotify-bridge/go/push.go`，接 clickAction 时）。

### TD-13 FIFO 空间阈值清理实装（2026-07-21 CP3c 跨审 D P2）
- **现状**：config 校验 `max_bytes > 0` + ARCHITECTURE 声明"超阈值 → FIFO 删 HLC 最老 → 回阈值 + 周期 bbolt.Compact()"，**store 全文零实装**（grep `FIFO|prune|Compact` 无命中）。SaveMessage 只 Put 不 Delete——`max_bytes` 形同虚设。
- **风险**：公网部署 bark `/{key}` 写开放（design 接受可见 DoS）下，扫描器/恶意脚本无限灌 bbolt msgs 桶 → 磁盘满 → 服务端整体崩溃（不只 bark，native /api/v1/* 也挂）。单 CP 都"接受可见 DoS"，**跨 CP 才看到"无 QPS 限流 + 无 FIFO = 磁盘必然崩"**（bark 写开放 + max_bytes 无 enforcement + FIFO 零实装三者闭环）。
- **现状兜底**：main.go 启动告警 `[WARN] FIFO eviction not implemented; max_bytes advisory`（CP3c 加，让运维知晓 + 单用户可信域泄露概率低接受）。
- **怎么修**：SaveMessage 后检查 msgs 桶大小超 `max_bytes` → `Cursor.First()` 删最老 HLC → 回阈值；周期跑 `bbolt.Compact()` 回收空间。可选 per-IP token bucket 限流（`golang.org/x/time/rate`，零 CGO 已是项目约束）防 bark 写开放灌库。
- **触发**：Phase 2 cleanup（CP6 MVP 后；公网部署前必须，否则磁盘崩）。

### TD-14 push 异步化（避免云函数劣化阻塞 HTTP）（2026-07-22 CP4 对抗审 P2-2）
- **现状**：`ingest → fanoutPush → pusher.Send → harmonySend` 全程同步。最坏（黑洞网络，每请求挂满 15s 超时）：单 URL 3×15s+2×1s≈47s，多 URL 翻倍。消息已先 SaveMessage 落库（不丢），但 HTTP 响应被拖住，单用户连发连续卡死。
- **CP4 不做**：异步引 goroutine 生命周期 + 关机 in-flight push 丢失 + 乱序，比「卡 47s」更可能是新屎山源（对抗审二次屎山判据：现在清=自己造屎山）。单用户量级 + sync 是更简设计（过度工程倾向纪律）。
- **怎么修（CP6 公网部署前评估）**：`SaveMessage` 后 `go s.fanoutPush(...)`（消息已落库，push best-effort，handler 立即 200）；或先调小 `harmonyHTTPTimeout=5s`（1 行）缓解。**默认不上异步**，真在生产被卡过再上。
- **触发**：CP6（公网部署前评估，若实测卡顿）。

### TD-15 5xx 重试范围 ✅（CP4 已修 2026-07-22，用户"小修方便直接修"覆盖原延后）—— doPost 502/503/504 → retry（CDN/网关瞬时），401/400/default(500) → system_error
- **现状**：`harmony.go doPost` 非 200 分支——`502 → harmonyRetry`，`401/400/default(含 500/503/504) → harmonySystemError`（终态，不重试不 fallback）。但 docs §8.2 写「HTTP 500/其他 5xx: → SystemError（**重试**）」。代码只重试 502。
- **为什么不 CP4 即修**：Netlify 云函数自身 500（private 未配/JSON parse 错）重试无用——这部分代码是合理的；真正 gap 是 CDN/网关瞬时 503/504 不重试。不 compound（无人在上层建假设），且 docs §8.2 与 §5.3 自身表述不一致需先统一再改码。
- **怎么修**：重试条件放宽为「5xx 除 401/400 外均 retry」（401/400 是调用方配错，重试无益，留 system_error）；或最保守把 `default` 中 `>= 500` 归 retry。同步统一 docs §8.2/§5.3 表述。
- **触发**：撞真 503/504 投递失败事故时，或 Phase 2 docs 统一批次。

### TD-16 harmony.go 小项 ✅ CP4 已修 P3-1/P3-2（dead 折码 + Ext ts 过滤+数量上限；P3-4 RetryLimit 命名保持未改，可选）
- **死码日志（P3-1）**：`doPost` dead 分支 `diagMsg = result.Msg` 没折 pushKitCode（system_error 分支折了），`postToCloudFunction` L159 注释自称「doPost 已把 code 融进 diagMsg（delivered/dead/system_error）」对 dead 是假的。日志/错误看不出 80100000（illegal token）vs 80300007（全无效）。修：dead 也 `fmt.Sprintf("code=%s msg=%s", pushKitCode, result.Msg)`。
- **Ext 碰撞/size（P3-2）**：`harmonySend` dataObj 先放 ts 再 merge `msg.Ext`，`Ext["ts"]` 会覆盖 server 设的 ts；Ext 无 size 上限。**不影响点击跳转安全**——Ext 进 payload.data 顶层（delivery.md §5 不进 click），sanitize 后的 url 在独立 clickAction.data（安全）。修：merge 前跳过保留键 "ts"；可选给 dataBytes 设上限（超 3500B drop Ext）。
- **命名（P3-4）**：`harmonyRetryLimit=3` 实为「总尝试 3 次」（1 初试 + 2 重试），非「重试 3 次」。docs §8.3「重试 3 次」字面 = 3 重试（4 总）。代码 + 测试一致取 3 总。可重命名 `harmonyMaxAttempts`。
- **触发**：下次摸 harmony.go 随流清（都是局部小改，互不影响，不 compound）。

### TD-17 harmony_test 补测 ✅ CP4 已全补（notifyId 稳定/401/400/subscribeLabel/no media_ids/all exhausted/dead-on-fallback/Ext ts；16 case 全绿）
- **现状**：`harmony_test.go` 漏 ① HTTP 401/400 → system_error 两分支零覆盖（只测了 500）② media_ids 空时不塞 clickAction.data（代码 `if len>0` 对的，无回归保护）③ SubscribeLabel「订阅:」前缀（含 title 空→改 body 分支，零覆盖）④ 多 URL 全 exhausted（主+备都败→keep token）⑤ data 顶层/Ext 构造（received 捕获了整 body 但只断言 ClickAction.Data）⑥ 死码在 fallback URL 触发（主 502 用尽→备 80300007→ErrDeadToken）。
- **注**：notifyId 幂等回归测试随 P1（notifyId 即时修）一并补，不在此 TD。此处是余下随流补的。
- **触发**：下次摸 harmony_test.go 随流，或 CP4.5 安卓 adapter 复用 harmony fallback/重试骨架前（复用前先补齐主路径回归）。

### TD-18 zero-config pushkit：自动拉 cloud_function_urls（2026-07-22 CP4 联调记）
- **现状**：CP4 `cloud_function_urls` 空 → 调试模式（只存不推，pushkit=false）。要真推必须手填 config.json `cloud_function_urls`（联调就是手填 hotifypushkit.netlify.app）。
- **legacy 对比**：Python 桥 `gotify_pushkit_bridge.py _fetch_cf_urls_from_txt`——`cloud_function_urls` 空时 fetch `cloud_function_urls.txt`（GitHub raw `raw.githubusercontent.com/sakura-lolipop/hotify-bridge/main/` + ghproxy.com 国内加速 → 本地缓存），自动拉 Hotify 托管 urls。**zero-config pushkit**（不配也能真推，用 Hotify 托管云函数）。
- **怎么修**：Server 启动 `cloud_function_urls` 空 → fetch Hotify 托管 `cloud_function_urls.txt`（同 legacy：ghproxy 加速 + 缓存 fallback），自托管用户 config.json override 自己的云函数 URL。zero-config 真推 + 自托管 override 兼容（Hotify 托管做默认/兜底，自托管 override）。
- **触发**：CP5/CP6（zero-config pushkit 启用，方便自托管首装 + 复用 legacy 机制；CP4 联调手填 config 绕过）。

### TD-19 ✅ handleHistory 最老 50 bug 修（CP4，用 HLC MessagesSince since=0）
- **已修（2026-07-22）**：CP1 临时 handleHistory `MessagesSince(0,50)` since=0 从最老扫返最老 50（注释"最近 50"与实现相反，DB>50 读不回新消息，对抗测两 agent 钉）。
- **修法（用 HLC 不新方法）**：`MessagesSince` since=0 改最新 N（Cursor.Last 倒序 N + 反转升序，client 默认收最新/无游标→最新 N）；since>0 增量不变（补漏）。handleHistory `MessagesSince(0,50)` 自动得最新 50。
- **曾走弯路**：试 MessagesLatest(N) 新方法（屎山：DRY 重复 MessagesSince + 绕过 HLC 游标统一），用户纠正（「为啥不用 hlc」「YAGNI」）回退（ee5ea6e revert 040773c）改用 HLC MessagesSince since=0。
- **完整 since=HLC 游标分页**（client 带 since 拉增量）仍 Phase 2（/api/v1/messages?since=）；CP4 修「默认最新 N」够用。

### TD-20 harmony.go 屎山小项（2026-07-22 CP4 屎山扫描 P2，下次摸 harmony.go 随流清）
- **P2-1 doPost 死返回值**：`doPost` 返 `(status, pushKitCode, diagMsg)` 三元，但 `postToCloudFunction` 拿 code 后 `_ = pushKitCode` 丢弃（code 已揉进 diagMsg）。签名过承诺误导 CP4.5 安卓作者。修：doPost 降 `(status, msg)`。
- **P2-2 magic 16**（Ext 字段上限，harmony.go:109）未常量化（跟 Push Kit 4KB 换算关系丢失）。修：`const maxExtFields = 16` + 注释换算。
- **P2-3 truncate/orVal DRY**：harmony.go 5 helper 跟 legacy `hotify-bridge/go/push.go` 逐字重复；`truncate`（rune 截断）+ `orVal`（空串兜底）是通用 string 工具锁 pushkit unexported，未来 server/store 用得再抄。修：提 util（跟 Mask 同理，TD-2 批）。`subscribeLabelEnabled`/`anyToCodeStr`/`readSnippet` push 专属留 pushkit。
- **P3**：散落 magic（160 诊断 snippet / 1<<20 body）/ harmonySend ~78 行 borderline（clickData/dataObj 可抽 helper，CP4.5 安卓复用时）/ config.go:53 注释 stale（"留 CP4" 没兑现，改"pushkit 无启动校验空 URLs=禁用合法"）/ android.go apns.go stub `_ = msg`（改 `_ model.Message` 签名更干净）/ anyToCodeStr 单字母 `v`/`num`/ harmony.go:216 吞 io.ReadAll err（拼进 diagMsg）。
- **触发**：下次摸 harmony.go（CP4.5 安卓 adapter 复用 fallback/retry 骨架前）随流清；P2-3 跟 TD-2 批。

## 按需清单（触发条件强，不单独成 TD，免死债）

- **`cmd/` 布局** —— 加 reset-key CLI / 多二进制时升级（当前单 main.go 在根够用）。
- **env var 配置（12-factor）** —— 上 serverless/云函数时加（当前 config.json 单机自托管够）。
- **OpenAPI/proto 契约** —— 跨端 / 对外开放时硬契约（当前单客户端自用、NEXT-Server.md §2 软文档维持够）。
