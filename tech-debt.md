# HotifyNEXT-Server · tech-debt

> CP 只做功能不夹带重构（用户纪律，见 `coding-strong-policy-weak`/`over-engineering-tendency`）；重构/拆分/cleanup 排 Phase 2 批次做。本文件记已知债，防忘。

## 清理排期（2026-07-21 定，屎山审查后）

**原则**：会长的债（compound / error-prone）下一个 CP 开头清，别让它长；不长的静态 DRY 批到 cleanup 批次；自然发生的随流清。

| 批次 | 债 | 何时 | 理由 |
|---|---|---|---|
| **CP3 开头** | TD-3（envelope 类型化） | 先建 `apiResp` + `writeAPIError` 再写 CP3 新端点 | CP2 已 5 处临界，CP3 端点（`/api/v1/messages`/`push`/`media`）翻倍；状态码双写是漏改源——边写边欠新债 |
| **CP3 流里** | TD-4（410 双胞胎 + device helper）+ **TD-5（bark.go 9 缺口，CP3 bark 兼容本职）** | TD-4 随 `/read` 删 + device 写方法抽 `mutateDevice`；**TD-5 = CP3 bark 兼容层按 `bark.md` §4 重写（含 2 段路径 bug）** | TD-5 非 cleanup、是 CP3 本职，列此追踪 9 缺口 |
| **Phase 2 cleanup 批次** | TD-1（文件拆分）+ TD-2（saveKeys DRY）+ TD-10（SaveMessage 签名）+ TD-11（pk→pusher 字段名） | CP3 端点稳定后 | TD-1 前置依赖 CP3（现在拆=churn）；静态 DRY/命名不 compound，批处理省 context-switch |
| **MVP 后工程化批次** | TD-6（CI + golangci-lint）+ TD-7（版本注入） | CP6（Phase 1 MVP 收尾）后 | 迭代期 churn 快 CI 噪声大反碍事；MVP 稳定后上自动门防回归 + 分发排错要版本号（2026-07-21 架构评估记） |
| **CP4 批次** | TD-12（msg.URL 协议白名单） | CP4（pushkit 搬，接 clickAction 前） | CP3c 两边（bark+native）裸收 url 未校验；CP4 pushkit 喂 clickAction.data 前必须加协议白名单（http/https/app scheme）防 javascript:/file: 跳转（XSS/钓鱼/本地文件泄露，CP3c 对抗审 B P2） |
| **CP6 批次** | TD-8（client 契约文档）+ TD-9（fanoutPush sentinel） | CP6（部署就绪） | client 接 API 要错误码契约文档；fanoutPush 全广播扇出前补 sentinel 防五分支语义糊（CP3b 屎山审 P2-3） |

**不清在 CP2**：CP2 已验证完成（`go test ./...` + `go vet` 全绿），混入重构违反"不混改"纪律；TD-1 现在 do 不了（CP3 端点重排依赖）；TD-3 在 CP3 开头做时上下文一样热（CP3 就是写端点、碰 envelope 的时候），现在做无 warmth 优势反冒"改坏已验证代码"险。

**CP3 启动 checklist**：① 先翻本文件 → ② 清 TD-3（建 envelope 类型）→ ③ 写端点过程中随流清 TD-4。

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

## 按需清单（触发条件强，不单独成 TD，免死债）

- **`cmd/` 布局** —— 加 reset-key CLI / 多二进制时升级（当前单 main.go 在根够用）。
- **env var 配置（12-factor）** —— 上 serverless/云函数时加（当前 config.json 单机自托管够）。
- **OpenAPI/proto 契约** —— 跨端 / 对外开放时硬契约（当前单客户端自用、NEXT-Server.md §2 软文档维持够）。
