# CP5 WS /stream 设计存档（Plan agent 设计 2026-07-22）

> CP5 开发依据。Plan agent 两轮设计 + 鸿蒙 @ohos.net.webSocket API 查证（对着 ArkTS 客户端能力设计）。

## 依赖
**gorilla/websocket**（纯 Go 零 CGO，保交叉编译单二进制；否决 stdlib 手写 RFC6455 + nhooyrio）。go.mod 加一行 require。

## 端点 + 握手鉴权
- `GET /api/v1/stream`（Handler mux 加一行；不走 requireKey1 中间件，鉴权在 handler 内）
- **方案 A（首帧 auth）**：upgrade（无鉴权，CheckOrigin 全放——鉴权在首帧不在 Origin）→ 设 readDeadline(authTimeout) → 读首帧 `{type:auth, uuid, key1, since}` → `store.AuthorizeRead(key1)` fail-closed → register onlineSet → 补漏 → 写 `auth_ack{ok:true}`
  - 选方案 A 因 ArkTS webSocket 可能吞 Authorization header（CSDN 报告 API15 过滤受保护头，待真机验）。退路 B（Sec-WebSocket-Protocol 子协议）/ C（?key1= query，开 allowKey1Query）
  - 首帧 auth 兼容 ArkTS 头过滤（key1 进 JSON 帧不进 header）

## onlineSet（连接管理）
`map[uuid][]*clientConn`（同 uuid 多端在线 = 切片），`sync.RWMutex`（broadcast RLock 高频 / register/unregister Lock）。`clientConn{uuid, conn *websocket.Conn, send chan []byte, done chan struct{}}`。send 缓冲满 → 关连接（防慢客户端拖死 broadcast）。

## 帧契约（TD-8 提前到 CP5）
`wsFrame` JSON **文本帧**（ArkTS `on('message')` value:string 可 `JSON.parse`）：
- `type`: `auth`（client→server 首帧）/ `auth_ack`（server→client 鉴权结果）/ `message`（server→client 新消息/补漏统一）
- message 帧：`{type:"message", hlc:"N", message:{...}}`（顶层 hlc 归因 + msg.hlc；`json:"hlc,string"` 防精度）
- **不带 requestId**（推场景非 RPC；client 的 since 是握手参数）
- 心跳用 **ArkTS 内置 ping/pong**（不自定义帧）；gorilla `SetReadDeadline(pongWait)` 检死连

## 全广播接入（不碰 fanoutPush）
`ingest` 无 target 分支加 `broadcastMessage(msg)`（1 行）；定向（TargetUUID 非空）走 fanoutPush 不变（**CP6 才按 dev.PushToken 有无分派 WS/pushkit**）。

## 补漏
首帧 auth `since > 0` → `MessagesSince(since, backfillLimit)` 升序逐帧 write（同 message 帧）；`since=0` → 最新 N（CP4 MessagesSince since=0 已修）。防竞态：**先 register 后读库 + 客户端 hlc 去重**（注册后新消息必走 broadcast，注册前必落库）。

## 优雅关闭
`ShutdownStream()`（关所有 conn done + bye 帧 + Close）。CP5 实装暴露，**CP6 main.go SIGTERM 调**（CP5 不接 signal.Notify）。

## 关闭码
4401（auth 失败不重连）/ 4402（协议错立即重连）/ 4000（shutdown 指数退避）/ 1011（internal）。

## 测试
- **L0**：onlineSet 并发（N goroutine register/unregister/broadcast 不死锁）/ 帧 marshal（hlc string 精度）/ broadcast 慢客户端关连
- **L1**：`httptest.NewServer` + gorilla `Dialer`（带 Authorization header 模拟 / 或首帧 auth）；用例：auth ok/fail/timeout/新消息推/backfill since/disconnect cleanup/duplicate uuid evict/concurrent broadcast
- **L2**：websocat（可选，bash 模拟）

## 鸿蒙 @ohos.net.webSocket API 待验（写代码前查官方文档，别凭印象）
1. `connect(url, options)` **自定义 header 支持？**（决定方案 A 首帧 vs header Bearer）—— CSDN 报告 Authorization 被吞，**待真机抓包验**
2. `on('message', (err, value: string|ArrayBuffer))` —— value 是 string？（JSON.parse）
3. **内置 ping/pong？**（`pingInterval`/`pongTimeout` 默认值？决定 gorilla pongWait 设多大）
4. 单帧限长？（msg.Ext 大时）
5. **无内置重连**（client 自己 on('close') 指数退避 + 重新 createWebSocket；别重复 createWebSocket 不 close 旧实例 → 内存泄漏）

## 文件落点
- **新建** `internal/server/stream.go`（wsFrame + 帧常量 + onlineSet + clientConn + handleStream + broadcastMessage + ShutdownStream + frame 构造 + streamUpgrader）
- **改** `internal/server/server.go`（Server 加 `online *onlineSet` + `shutdown chan struct{}`；New init；Handler mux 加 `GET /api/v1/stream`）
- **改** `internal/server/push.go`（ingest 无 target 加 `s.broadcastMessage(msg)` 1 行）
- **改** `go.mod`（gorilla/websocket）
- **新建** `internal/server/stream_test.go`（L0+L1）

## CP 边界（不夹带，coop §「CP 只做功能」）
**不做**：main.go SIGTERM（CP6 graceful shutdown）/ pushkit 全广播分派（CP6 fanoutPush 改造）/ TD-1 文件拆分 / TD-9 fanoutPush sentinel（CP6）/ `/api/v1/messages?since=` HTTP 端点（CP6，补漏走 WS 帧 CP5 够）/ TD-13 FIFO（Phase 2）。

## 日志（对齐 log.md）
`[ws]` tag + hlc 归因：`[ws] connect uuid=X since=N backlog=M` / `[ws] auth uuid=X ok=true since=N backfill=M` / `[ws] push uuid=X hlc=N framesent=1` / `[ws] disconnect uuid=X reason=Y` / `[ws] bye uuid=X reason=shutdown`。
