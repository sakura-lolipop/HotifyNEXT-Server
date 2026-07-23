// CP5 WebSocket /api/v1/stream：在线设备实时推 + 重连补漏。
//
// 设计存档 cp5-ws-design.md（两轮 Plan agent + 鸿蒙 @ohos.net.webSocket API 查证）。
//
// 为什么 WS：pushkit 离线推只覆盖有系统推送 token 的设备（鸿蒙/华为系安卓）；
// 无 token 的设备（非 HMS 安卓/windows/linux/鸿蒙前台）靠 WS 长连收实时消息（CP5），
// 重连时按 HLC since 补漏。pushkit 离线 + WS 在线互补 = Phase 1 实时性闭环。
//
// 鉴权 = 首帧 auth（方案 A）：ArkTS @ohos.net.webSocket 不支持自定义 header
// （华为开发者博客「WebSocket 不支持自定义 Headers」+ CSDN API15 过滤受保护头，两路确认），
// 故 key1 进 JSON 首帧不进 Authorization header。鉴权决策下沉 store.AuthorizeRead（fail-closed，
// 与 requireKey1 同源——HTTP 层永不直接判 Keys{}，消除「吞 err→误判窗口」脚枪）。
//
// 帧契约（TD-8 client 契约；完整错误码文档 CP6）：
//   - client→server 首帧 wsAuthFrame{type:"auth", uuid, key1, since}
//   - server→client wsFrame{type:"auth_ack"|"message", ...}（文本帧 JSON，ArkTS on('message') JSON.parse）
//   - 关闭码：4401 auth 失败（不重连）/ 4402 协议错（立即重连）/ 4000 shutdown·evict·慢客户端（指数退避）/ 1011 internal
//
// pump 模型（gorilla 标准）：writePump 单写者（独占 conn.Write/Close）；readPump 单读者（不 Close，
// 靠 writePump defer Close 解除阻塞）；broadcast/Shutdown/evict/慢客户端 通过 quit 信号 + send chan 与 writePump 通信。
//
// WS storm 防护（并发连 cap）defer 到 CP6 公网前（TD-22）：CP5 未公网 + 单用户可信域，风险低；
// 公网部署前必加 semaphore cap 挡扫描器/重连风暴灌爆 goroutine/内存。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// —— 超时 / 缓冲 / 补漏上限（const：gorilla SetReadDeadline/SetWriteDeadline 标准用法、语义平凡，不单测超时分支）——
const (
	wsAuthTimeout   = 10 * time.Second // 注册前首帧 auth 读超时：未发 auth → 关 4401（挡不发 auth 的连，≤此值自清）
	wsPongWait      = 60 * time.Second // 等 pong 超时：客户端死（不回 pong）→ readPump 读超时 → 清理
	wsPingPeriod    = wsPongWait / 2   // 发 ping 间隔（须 < pongWait，留半周期余量收 pong 重置 deadline）
	wsWriteTimeout  = 10 * time.Second // 单帧写超时：防慢写阻塞 writePump（死连写 ≤此值失败 → pump 退）
	wsBackfillLimit = 100              // since 补漏上限（MessagesSince limit；防大 backlog 一次性灌爆客户端）
	wsSendBuffer    = 64               // clientConn.send 缓冲：满 → 慢客户端，broadcast 关该连不阻塞
)

// —— WS 关闭码（4000-4999 RFC6455 留给应用自定义；client 按码定重连策略）——
const (
	wsCloseAuthFailed = 4401 // 首帧 auth 失败（key1 不符）→ client 不重连（凭证错，重连也是错）
	wsCloseProtoError = 4402 // 协议错（首帧非 auth / 坏 JSON / 缺 uuid·key1）→ client 立即重连（瞬时）
	wsCloseShutdown   = 4000 // server 主动关（ShutdownStream / evict / 慢客户端）→ client 指数退避重连
)

// wsAuthFrame client→server 首帧。key1 进帧不进 header（ArkTS webSocket 不支持自定义 header）。
type wsAuthFrame struct {
	Type  string `json:"type"`  // 必须 "auth"（否则 4402 协议错）
	UUID  string `json:"uuid"`  // 设备 uuid（onlineSet key + 未来 LastSeen）
	Key1  string `json:"key1"`  // 域内读凭证（store.AuthorizeRead fail-closed）
	Since string `json:"since"` // 补漏起点 hlc（"0"/缺省=最新 N；具体 hlc=增量开区间，跳过 since 本身）
}

// wsFrame server→client 文本帧契约。hlc 全 string 防 JS Number 精度（HLC 实际值 > 2^53）。
// 顶层 HLC 归因：grep `hlc=N` 跨 [push]→[ws] / 客户端去重不必解 message 体。不带 requestId（推场景非 RPC）。
type wsFrame struct {
	Type    string         `json:"type"`              // auth_ack | message
	HLC     string         `json:"hlc,omitempty"`     // message 帧顶层 hlc 归因（strconv.FormatUint）
	Message *model.Message `json:"message,omitempty"` // message 帧载荷（完整 Message，含 msg.HLC string）
	OK      bool           `json:"ok,omitempty"`      // auth_ack：true=放行（失败走关闭码，此字段成功时用）
	Reason  string         `json:"reason,omitempty"`  // 预留（当前失败原因随关闭帧 FormatCloseMessage 带）
}

// streamUpgrader WS 升级器。CheckOrigin 全放——鉴权在首帧 JSON（key1），不在 Origin
// （CSRF 跨站读不到 WS 帧体；Origin 校验对首帧 auth 无增益）。
var streamUpgrader = websocket.Upgrader{
	CheckOrigin: func(request *http.Request) bool { return true },
}

// clientConn 一条在线 WS 连接 + 它两个 pump 的通信通道。
type clientConn struct {
	uuid      string
	conn      *websocket.Conn
	send      chan []byte   // broadcast 投递处（缓冲 wsSendBuffer）；满 → 慢客户端
	quit      chan struct{} // 关闭信号（cc.close 一次 close；writePump 收到发 bye+关）
	closeOnce sync.Once
}

// close 幂等发关闭信号。多源触发（readPump 死连 / broadcast 慢客户端 / ShutdownStream / evict 旧连），
// sync.Once 保证 quit 只 close 一次（重复 close chan 会 panic）。
func (cc *clientConn) close() {
	cc.closeOnce.Do(func() { close(cc.quit) })
}

// onlineSet 在线连接注册表：单 conn/uuid + register 时 evict 旧连。
// CP5 全广播用（broadcast 给所有在线）；CP6 fanoutPush 按 uuid 定向 WS 推时直接查 conns map（前向就绪）。
type onlineSet struct {
	mu    sync.RWMutex
	conns map[string]*clientConn // uuid → 单条连接（新连 register 时 evict 同 uuid 旧连）
}

func newOnlineSet() *onlineSet {
	return &onlineSet{conns: make(map[string]*clientConn)}
}

// register 注册连接；uuid 已存在 → evict 旧连（close 信号）→ map 替换为新连。
// 旧连 writePump 收 quit 发 bye + Close；其 readPump 随后报错 → unregister（身份校验不误删新连）。
func (online *onlineSet) register(cc *clientConn) {
	online.mu.Lock()
	if old := online.conns[cc.uuid]; old != nil {
		old.close()
	}
	online.conns[cc.uuid] = cc
	online.mu.Unlock()
}

// unregister 移除连接；身份校验 map[uuid]==cc 才删（防 evict 后旧连清理误删已替换的新连）。
func (online *onlineSet) unregister(cc *clientConn) {
	online.mu.Lock()
	if current := online.conns[cc.uuid]; current == cc {
		delete(online.conns, cc.uuid)
	}
	online.mu.Unlock()
}

// broadcast 给所有在线连接投递 payload（RLock 高频）；non-blocking send，缓冲满 → close 慢客户端。
// 返回成功入队连接数（[ws] push framesent 归因）。绝不阻塞（慢连不影响其他连/全广播）。
func (online *onlineSet) broadcast(payload []byte) int {
	online.mu.RLock()
	defer online.mu.RUnlock()
	sent := 0
	for _, cc := range online.conns {
		select {
		case cc.send <- payload:
			sent++
		default:
			cc.close() // 慢客户端：send 满 → 关连（writePump 自行 bye+Close；readPump 随后 unregister）
		}
	}
	return sent
}

// closeAll 关闭所有连接（ShutdownStream 调）；返回关闭数。只发 quit 信号不动 map
// （writePump 自行 bye+Close；readPump 随后 unregister，不在此持锁阻塞）。
func (online *onlineSet) closeAll() int {
	online.mu.RLock()
	defer online.mu.RUnlock()
	for _, cc := range online.conns {
		cc.close()
	}
	return len(online.conns)
}

// count 在线连接数（测试 + ShutdownStream log 用）。
func (online *onlineSet) count() int {
	online.mu.RLock()
	defer online.mu.RUnlock()
	return len(online.conns)
}

// broadcastMessage 全广播给在线 WS 客户端（ingest 无 target 调，1 行接入）。
// 序列化 message 帧一次复用给所有 conn（不每连重 marshal）。不阻塞 ingest（broadcast non-blocking）。
// hlc 归因：[ws] push hlc=N 让 grep 跨 [push] saved hlc=N → [ws] push hlc=N 闭环。
func (s *Server) broadcastMessage(msg model.Message) {
	payload, err := json.Marshal(wsFrame{
		Type:    "message",
		HLC:     strconv.FormatUint(msg.HLC, 10),
		Message: &msg,
	})
	if err != nil {
		// 不该发生（model.Message 纯 JSON 可序列化）；留痕不崩，消息已落库（ingest 主目的达成）。
		log.Printf("[ws] broadcast marshal err hlc=%d: %v", msg.HLC, err)
		return
	}
	sent := s.online.broadcast(payload)
	log.Printf("[ws] push hlc=%s framesent=%d", strconv.FormatUint(msg.HLC, 10), sent)
}

// handleStream GET /api/v1/stream：WS 长连 + 首帧 auth + 全广播实时推 + since 补漏。
// 不走 requireKey1（鉴权在首帧 JSON，不在 header）。流程：upgrade → 首帧 auth → register → auth_ack → 补漏 → pump。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 已写错误响应（非 WS 请求 / 握手协议错）；只留痕，不再碰 w。
		log.Printf("[ws] upgrade failed remote=%s: %v", r.RemoteAddr, err)
		return
	}
	defer conn.Close() // 兜底 closer：auth 失败（writePump 未起）/ pump 结束（writePump 已 defer Close，此处幂等）

	// —— 首帧 auth（注册前 readDeadline = authTimeout，挡不发 auth 的连）——
	conn.SetReadDeadline(time.Now().Add(wsAuthTimeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		rejectAuth(conn, "", wsCloseAuthFailed, "read first frame: "+err.Error())
		return
	}
	var auth wsAuthFrame
	if err := json.Unmarshal(raw, &auth); err != nil || auth.Type != "auth" || auth.UUID == "" || auth.Key1 == "" {
		rejectAuth(conn, auth.UUID, wsCloseProtoError, "bad auth frame")
		return
	}
	authorized, authErr := s.st.AuthorizeRead(auth.Key1) // fail-closed（err→1011 / false→4401 / true→放行）
	if authErr != nil {
		log.Printf("[ws] auth uuid=%s err=%v", auth.UUID, authErr)
		rejectAuth(conn, auth.UUID, websocket.CloseInternalServerError, "auth check failed")
		return
	}
	if !authorized {
		rejectAuth(conn, auth.UUID, wsCloseAuthFailed, "invalid key1")
		return
	}
	since, _ := strconv.ParseUint(auth.Since, 10, 64) // "0"/缺省/坏值 → 0（最新 N 兜底）

	cc := &clientConn{
		uuid: auth.UUID,
		conn: conn,
		send: make(chan []byte, wsSendBuffer),
		quit: make(chan struct{}),
	}
	// 先 register 后补漏：注册后新消息走 broadcast、注册前已落库（SaveMessage 在 broadcast 前）→ 无缺口；
	// 重叠的重复靠客户端 hlc 去重（记 TD-8 契约）。register 内 evict 同 uuid 旧连。
	s.online.register(cc)
	log.Printf("[ws] connect uuid=%s since=%d", auth.UUID, since)

	// auth_ack + 补漏：writePump 起来前由本 goroutine 直接写 conn（单写者无竞争；pump 在 serveConn 才起）。
	if !sendAuthAck(conn) {
		log.Printf("[ws] auth_ack write fail uuid=%s (client gone right after auth)", auth.UUID)
		s.online.unregister(cc)
		return
	}
	backfillCount, backfillErr := s.backfill(conn, since)
	if backfillErr != nil {
		log.Printf("[ws] backfill abort uuid=%s since=%d: %v", auth.UUID, since, backfillErr)
		s.online.unregister(cc)
		return
	}
	log.Printf("[ws] auth uuid=%s ok=true since=%d backfill=%d", auth.UUID, since, backfillCount)

	s.serveConn(cc)
}

// rejectAuth 写关闭帧 + log（conn 由 handleStream defer Close 关）。code 决定 client 重连策略。
func rejectAuth(conn *websocket.Conn, uuid string, code int, reason string) {
	log.Printf("[ws] auth uuid=%s ok=false code=%d reason=%s", uuid, code, reason)
	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_ = conn.WriteControl(websocket.CloseMessage, // best-effort（客户端可能已断；忽略 err）
		websocket.FormatCloseMessage(code, reason), time.Now().Add(wsWriteTimeout))
}

// sendAuthAck 写 auth_ack{ok:true}（writePump 起来前单写者直接写）。返 false=写失败（客户端已断）。
func sendAuthAck(conn *websocket.Conn) bool {
	payload, err := json.Marshal(wsFrame{Type: "auth_ack", OK: true})
	if err != nil {
		return true // 不该发生；视为成功继续（ack 丢失客户端会重连）
	}
	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return false
	}
	return true
}

// backfill 补漏：since=0 最新 N / since>0 增量开区间（MessagesSince 升序返回，store.go:319）。
// 逐帧 write（升序旧→新）。返（投递帧数, err）；err=conn 断（剩余不投，handleStream 不起 pump）。
func (s *Server) backfill(conn *websocket.Conn, since uint64) (int, error) {
	msgs, err := s.st.MessagesSince(since, wsBackfillLimit)
	if err != nil {
		log.Printf("[ws] backfill read err since=%d: %v", since, err)
		return 0, nil // 读库错：不阻塞会话（客户端下次重连再补），返 0 继续 pump
	}
	for index, msg := range msgs {
		payload, err := json.Marshal(wsFrame{
			Type:    "message",
			HLC:     strconv.FormatUint(msg.HLC, 10),
			Message: &msg,
		})
		if err != nil {
			continue // 单条 marshal 失败跳过（不该发生）；不影响其余帧
		}
		conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return index, err // conn 断：已投 index 条，剩余丢；handleStream 收 err 不起 pump
		}
	}
	return len(msgs), nil
}

// serveConn 起 writePump + 跑 readPump（inline，阻塞至 conn 关闭）。readPump 退出后等 writePump 收尾。
func (s *Server) serveConn(cc *clientConn) {
	var pumpWait sync.WaitGroup
	pumpWait.Add(1)
	go func() {
		defer pumpWait.Done()
		writePump(cc)
	}()
	s.readPump(cc) // 阻塞至 conn 关闭（HTTP handler goroutine 即 readPump，WS 会话生命周期在此）
	pumpWait.Wait() // 等 writePump 收尾（quit→bye→Close），保证 ShutdownStream 的 bye 帧发出后才整体返回
}

// readPump 单读者：处理客户端 pong（重置读超时）+ 检死连。auth 后客户端不应再发 app 帧（心跳靠服务端 ping）。
// 退出时 unregister + cc.close（信号 writePump）。不 Close conn——writePump defer Close 兜底解除本 pump 阻塞。
func (s *Server) readPump(cc *clientConn) {
	defer func() {
		s.online.unregister(cc)
		cc.close() // 信号 writePump 退出（若它还在 select 等待；幂等）
	}()
	cc.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	cc.conn.SetPongHandler(func(string) error {
		cc.conn.SetReadDeadline(time.Now().Add(wsPongWait)) // 收 pong：客户端活着，重置死连超时
		return nil
	})
	for {
		if _, _, err := cc.conn.ReadMessage(); err != nil {
			log.Printf("[ws] disconnect uuid=%s reason=%v", cc.uuid, err)
			return
		}
		// auth 后的 app 帧：CP5 客户端不发（心跳走服务端 ping）；收到忽略。未来 client→server 帧（ack 等）在此解析。
	}
}

// writePump 单写者：消费 send chan 投递帧 / 定时 ping 检活 / 收 quit 发 bye 关连。独占 conn.Close（defer）。
// 关 conn 解除 readPump 的 ReadMessage 阻塞（readPump 不 Close，靠此 pump 的 defer Close 收尾）。
func writePump(cc *clientConn) {
	defer cc.conn.Close()
	pingTicker := time.NewTicker(wsPingPeriod)
	defer pingTicker.Stop()
	for {
		select {
		case payload, ok := <-cc.send:
			if !ok {
				return // send chan 不该被关（关连走 quit 信号）；防御性退出
			}
			cc.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := cc.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return // 写失败（客户端死/网络断）→ defer Close 解除 readPump 阻塞
			}
		case <-pingTicker.C:
			cc.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := cc.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return // ping 写失败 → 客户端死
			}
		case <-cc.quit:
			// 优雅关（ShutdownStream / register evict / broadcast 慢客户端）：发 bye + 关闭码 → client 按码重连。
			cc.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			_ = cc.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(wsCloseShutdown, "server closing"),
				time.Now().Add(wsWriteTimeout))
			return
		}
	}
}

// ShutdownStream 优雅关闭所有 WS 连接（CP6 main.go SIGTERM 调；CP5 只暴露不接 signal.Notify）。
// 遍历 onlineSet 发 quit 信号 → writePump 发 bye(4000) + Close → readPump 清理 unregister。
// 不动 map（writePump/readPump 自行清理），避免持锁阻塞 broadcast/register。
func (s *Server) ShutdownStream() {
	if s.online == nil {
		return // 零值 Server 或未初始化（New 总会 init；防御）
	}
	closed := s.online.closeAll()
	log.Printf("[ws] bye reason=shutdown conns=%d", closed)
}
