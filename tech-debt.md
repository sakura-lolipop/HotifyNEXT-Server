# HotifyNEXT-Server · tech-debt

> CP 只做功能不夹带重构（用户纪律，见 `coding-strong-policy-weak`/`over-engineering-tendency`）；重构/拆分/cleanup 排 Phase 2 批次做。本文件记已知债，防忘。

## 清理排期（2026-07-21 定，屎山审查后）

**原则**：会长的债（compound / error-prone）下一个 CP 开头清，别让它长；不长的静态 DRY 批到 cleanup 批次；自然发生的随流清。

| 批次 | 债 | 何时 | 理由 |
|---|---|---|---|
| **CP3 开头** | TD-3（envelope 类型化） | 先建 `apiResp` + `writeAPIError` 再写 CP3 新端点 | CP2 已 5 处临界，CP3 端点（`/api/v1/messages`/`push`/`media`）翻倍；状态码双写是漏改源——边写边欠新债 |
| **CP3 流里** | TD-4（410 双胞胎 + device helper）+ **TD-5（bark.go 9 缺口，CP3 bark 兼容本职）** | TD-4 随 `/read` 删 + device 写方法抽 `mutateDevice`；**TD-5 = CP3 bark 兼容层按 `bark.md` §4 重写（含 2 段路径 bug）** | TD-5 非 cleanup、是 CP3 本职，列此追踪 9 缺口 |
| **Phase 2 cleanup 批次** | TD-1（文件拆分）+ TD-2（saveKeys DRY） | CP3 端点稳定后 | TD-1 前置依赖 CP3（现在拆=churn）；静态 DRY 不 compound，批处理省 context-switch，对齐 legacy hotify"M3 后 cleanup"节奏 |

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
- **现状**：`handleMarkRead`/`handleReadSet` 是逐字符相同的 410 handler；`RegisterDevice`/`TouchDeviceSeen` 的 `Get→Unmarshal→改→Marshal→Put` 骨架重复。
- **修**：410 双胞胎合一（Go 1.22 一个 handler 挂 POST+GET 两 pattern）；device 写方法加 `mutateDevice(tx, uuid, func(*model.Device))` helper（CP3 加 `UpdateDeviceName`/`ClearPushToken` 等时抽）。
- **触发**：CP3（`/read` 路由删除 + 新 device 写方法时）。

### TD-5 bark.go 9 缺口（CP3 bark 兼容层本职，2026-07-21）
- **现状**：`internal/bark/bark.go`（CP1-temp 最小实现）只解析路径式 + JSON {title,body}、category 硬编 default。`bark.md` §4.6 依 bark-server 源码列 9 缺口：①缺 query/form 参数合并 ②缺字段全集（level/group/call/url/image/sound/icon/…）③category 硬编 default ④缺 4 段式 `/{key}/{title}/{subtitle}/{body}` ⑤响应缺 timestamp ⑥**bug：缺 2 段式 `/{key}/{body}`——现 `len(segs)>=3` 把 GET `/{key}/body` 的 body 当 title** ⑦缺 Content-Type 分派 ⑧缺字段名小写化 ⑨缺 URL 解码。
- **性质**：**CP3 bark 兼容层的实装范围（非 Phase 2 cleanup）**——CP3 按 `bark.md` §4 重写 bark.go 全部补齐；#6 bug 也 CP3 修（CP1-temp 不完整 bark 是预期的，不单独 hotfix）。
- **触发**：CP3（bark+原生双入口归一 PushSpec）。
- **依据**：`bark.md` §4.6（bark-server 源码出处 `route_push.go`/`apns.go`/`router.go`）。
