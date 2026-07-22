# HotifyNEXT-Server 日志形式

> CP4（2026-07-22）定。日志策略经对抗 agent 审查（吞状态/归因/噪音平衡）+ bark-server 样式 agent 对比。
> 工程蓝图（是什么 + 怎么打）；决策（为什么这么打，不上结构化 json/级别）见末尾。

## 格式

`[tag] key=val` 半结构化，stdlib `log`（**不上 logrus/slog/json log**——单用户低 QPS + 自托管，grep 过滤 > 结构化解析；决策见下）。

```
[http] POST /api/v1/push 200 127.0.0.1 2.6s
[push] saved hlc=9174991379730268160 target=ddff9c84-... category=call
[pushkit] ✓ harmony ddff9c84-... hlc=9174991379730268160 code=80000000 (url=https://hotifypushkit.netlify.app/api/push)
[server] 500 save failed: msgs bucket corrupt at hlc=...
```

## 输出

`log.SetOutput(io.MultiWriter(os.Stdout, logFile))`（main.go 启动设）：
- **stdout**——shell 实时（前台 go run）/ systemd journal（生产 systemd 部署）
- **hotify.log**——持久保存（cwd `HotifyNEXT-Server/hotify.log`，`*.log` 已 .gitignore；公网排障翻历史）
- 之前 `go run` background 只写临时 task output 文件（不 shell 实时 + 不持久）。io.MultiWriter 一次兜住两份。
- 路径固定 `hotify.log`（config.log_file 留 Phase 2 如需多日志/路径可配）。

## 日志清单（按 tag）

### `[http]`（logReq 中间件，每请求一条 access log）
```
[http] METHOD path status remoteAddr duration
```
- **status + remoteAddr**（P1，2026-07-22）：`statusRecorder` 包 ResponseWriter 捕获响应码。之前只 `method path duration` **吞状态**（200/400/500 混）。一改兜住所有 handler 的 400/401/404/415/500 可观测。
- 公网排障：status（成败）+ remoteAddr（谁在打/扫描/抢注）。

### `[push]`（ingest 存库 + fanoutPush 推送）
- `[push] saved hlc=N target=X category=Y`——消息存 msgs 桶（SaveMessage 后，定向/全广播都打）。**HLC 归因锚**（P2-2）。
- `[push] device not found target=X (不落库，从根杀编 key 灌库向量)`——定向 target 不存在（P2-1，灌库向量可见性）。
- `[push] device X empty push token, saved but not pushed`——空 token（fanoutPush 挡）。
- `[push] device X dead token, PushToken cleared`——死 token 清理（pushkit ErrDeadToken → ClearPushToken）。
- `[push] device X dead token but ClearPushToken failed: ... (token kept)`——清理失败留痕。

### `[pushkit]`（harmonySend 推送结果，五态全覆盖）
- `[pushkit] ✓ harmony uuid hlc=N code=80000000 (url=X)`——delivered
- `[pushkit] ✗ harmony uuid hlc=N dead token (url=X) msg=X`——死 token（返 ErrDeadToken → fanoutPush 清 token）
- `[pushkit] ⚠ harmony uuid hlc=N system_error (url=X) msg=X`——系统错（保留 token）
- `[pushkit] ↻ harmony retry N/3 (url=X) msg=X`——重试中
- `[pushkit] ✗ harmony uuid hlc=N all cloud function URLs exhausted, keep token`——全 URL 用尽
- 四行终态都带 `hlc=N`（P2-3，跨 [push]/[pushkit] 两层 grep 归因）。

### `[register]`（设备注册）
`[register] device=X platform=Y token=***`（token `util.Mask` 脱敏）+ `[register-legacy]`（旧 /register 无 key1）。

### `[server]` / `[bark]`（500 err）
`[server] 500 msg` / `[bark] 500 msg`（writeAPIError/writeBark status>=500；P2-4）。间歇磁盘问题（bbolt 坏/GetDevice 错/存失败）第一次就在日志抓到，不靠 client 回贴响应体。

### `[WARN]`（启动告警）
- `[WARN] store=memory (debug only, restart loses all data)`
- `[WARN] FIFO eviction not implemented (TD-13, Phase 2); max_bytes advisory`
- `[pushkit] cloud_function_urls 为空 → Send 静默跳过（调试模式，只存不推）`

## 归因字段（grep 友好）

| 字段 | tag | 用途 |
|---|---|---|
| `hlc` | [push] saved → [pushkit] 终态 | 跨层归因（P2-2/P2-3 补，grep `hlc=N` 命中存库+推送） |
| `uuid` | [register]/[push]/[pushkit] | 设备追踪 |
| `status` | [http] | 成败（P1） |
| `code` | [pushkit] | Push Kit code（80000000/80100000/80300007） |
| `remoteAddr` | [http] | 公网扫描/抢注排查（P1） |
| `token` | [register] | `util.Mask` 脱敏（首4…末4），不泄露 |

## 级别

**不上 logrus/slog**（stdlib `log` + `[WARN]`/`[tag]` 文本区分够）。单用户低 QPS + 自托管，grep 过滤 > 结构化级别。（bark-server 样式 agent 对比后定，待补充）

## 噪音平衡

- **access log（[http]）每请求**：公网扫描器刷量，但 access log 职责——在**日志收集层过滤/采样**，不在源端省。加 status/IP 只加字段不增行。
- **[push] saved 每消息**：只对真实进库消息触发（scanner 空 content/device-not-found 在此之前 400 挡），频度=真实消息率，Hotify 单用户低噪。
- **device-not-found**（P2-1）：bark 写开放 + 公网扫描器刷 `POST /随机key` 会高频，但这恰是攻击信号你想看；部署后若刷屏，日志收集层采样，不砍 log。
- **不打的**（够的别加，防过度日志）：
  - `[push] success`——`[pushkit] ✓` 已记，别双打。
  - store 层零 log——err 冒泡上层（ingest/fanoutPush/500 log），store 不自打。

## 决策记录

- **P1 logReq 加 status/IP**（2026-07-22）：全仓最大吞状态黑洞——200/400/500 混，公网排障看不出成败/扫描。`statusRecorder` 包 ResponseWriter 一次兜住所有 handler。
- **P2-1 device-not-found log**：从根杀的「编 key 灌库」向量（CP3c 跨审修正），被拒时零 log 排障不知扫描器在试啥 key。
- **P2-2 ingest saved hlc**：定向消息存库成功后整条链路无 HLC 留痕（[pushkit] ✓ 只带 uuid 不带 hlc）+ 调试模式（cloud_function_urls 空）定向消息完全无 log。补 `[push] saved hlc=N` 覆盖。
- **P2-3 [pushkit] 四行补 hlc**：跨 [push]/[pushkit] 两层 grep `hlc=N` 归因链闭合。
- **P2-4 500 err 落 log**：err.Error() 之前只拼进响应体给 client，server 侧无 log；间歇磁盘问题第一次就在日志抓到。
- **不上结构化 json log / 级别**：单用户自托管 stdlib grep 够；slog/logrus 引依赖 + 改全仓 log 调用，YAGNI。（bark-server 样式 agent 对比后定）

## 待补充

- bark-server 日志样式对比 agent（格式/级别/字段/库）——验证「Hotify 该不该对齐」。
- 日志验证 agent（起 Server + curl 验 P1/P2 输出 + 找缺口）——补充遗漏点。
