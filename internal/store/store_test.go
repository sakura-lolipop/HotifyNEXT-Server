// store 层 L0 单元测试（docs/coop.md：测试自己写——要 HLC 单调/first-set/ErrNotFound 契约等上下文）。
// 覆盖 taskNEXT「测试策略」CP1 必测清单：HLC（4 场景+单调序列）+ JSON string + bbolt 存取（单调+重启持久化）
// + MessagesSince（开区间/limit/空 db）+ big-endian 排序 + Message 单条 + ErrNotFound 全分支 + Device（新注/刷新/
// AllDevices/RemoveDevice/TouchDeviceSeen）+ Cursor（覆盖/未设）+ Keys（first-set wins/key2 幂等+持久化）
// + Media（SaveMedia/GetMedia/MediaIDs 多附件/mime）+ 并发 smoke + Memory 子集。
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// ── HLC 纯函数 ──

func TestNextHLC_ClockForward(t *testing.T) {
	// 时钟前进（now>lastPt）：用新 pt，counter 归零
	pt, ctr := nextHLC(100, 5, 200)
	if pt != 200 || ctr != 0 {
		t.Errorf("clock forward: got pt=%d ctr=%d, want 200/0", pt, ctr)
	}
}

func TestNextHLC_StallCounterInc(t *testing.T) {
	// 时钟停滞（now==lastPt）：pt 不动，counter+1
	pt, ctr := nextHLC(100, 5, 100)
	if pt != 100 || ctr != 6 {
		t.Errorf("stall: got pt=%d ctr=%d, want 100/6", pt, ctr)
	}
}

func TestNextHLC_NTPBackward(t *testing.T) {
	// NTP 回退（now<lastPt）：pt 保持历史值，counter+1（补漏不卡）
	pt, ctr := nextHLC(1000, 3, 500)
	if pt != 1000 || ctr != 4 {
		t.Errorf("backward: got pt=%d ctr=%d, want 1000/4", pt, ctr)
	}
}

func TestNextHLC_CounterOverflow(t *testing.T) {
	// counter 溢出（lastCtr=0xFFFF + 停滞）：pt 前进 1，counter 归零（理论不触发，65536/秒）
	pt, ctr := nextHLC(100, hlcCtrMax, 100)
	if pt != 101 || ctr != 0 {
		t.Errorf("overflow: got pt=%d ctr=%d, want 101/0", pt, ctr)
	}
}

func TestNextHLC_MonotonicSequence(t *testing.T) {
	// 连续生成 100 个（持续停滞），packHLC 严格递增
	var lastPt, lastCtr uint64 = 1000, 0
	prev := packHLC(lastPt, lastCtr)
	for i := 0; i < 100; i++ {
		lastPt, lastCtr = nextHLC(lastPt, lastCtr, 1000)
		cur := packHLC(lastPt, lastCtr)
		if cur <= prev {
			t.Fatalf("iter %d: HLC not strictly monotonic: cur=%d prev=%d", i, cur, prev)
		}
		prev = cur
	}
}

func TestNextHLC_NTPBackwardThenForwardRecovers(t *testing.T) {
	// 回退几秒（counter 递增）→ 真实时钟追上 → 切回 pt 前进分支，仍单调
	var lastPt, lastCtr uint64 = 2000, 0
	// 回退到 1500 几次
	for i := 0; i < 5; i++ {
		lastPt, lastCtr = nextHLC(lastPt, lastCtr, 1500)
	}
	// 前进到 3000
	lastPt, lastCtr = nextHLC(lastPt, lastCtr, 3000)
	if lastPt != 3000 || lastCtr != 0 {
		t.Errorf("recover: got pt=%d ctr=%d, want 3000/0", lastPt, lastCtr)
	}
}

func TestPackUnpackHLC(t *testing.T) {
	cases := []struct{ pt, ctr uint64 }{
		{0, 0}, {hlcPtMask, hlcCtrMax}, {123456789, 42}, {1 << 47, 1},
	}
	for _, c := range cases {
		hlc := packHLC(c.pt, c.ctr)
		pt, ctr := unpackHLC(hlc)
		if pt != c.pt || ctr != c.ctr {
			t.Errorf("pack/unpack pt=%d ctr=%d: got pt=%d ctr=%d (hlc=%d)", c.pt, c.ctr, pt, ctr, hlc)
		}
	}
}

// ── HLC JSON string 序列化（防客户端 Number 精度 2^53）──

func TestMessageHLC_JSONString(t *testing.T) {
	// HLC 实际值 ~1.17e23 远超 JS Number.MAX_SAFE_INTEGER；json:",string" 序列化成字符串
	m := model.Message{HLC: 1 << 62, Category: "default", Body: "x"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	hlcField, ok := raw["hlc"]
	if !ok {
		t.Fatal("hlc field missing")
	}
	if len(hlcField) == 0 || hlcField[0] != '"' {
		t.Errorf("hlc not string-encoded (would lose precision in JS): %s", hlcField)
	}
	// 往返一致
	var m2 model.Message
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatal(err)
	}
	if m2.HLC != m.HLC {
		t.Errorf("round-trip: got %d want %d", m2.HLC, m.HLC)
	}
}

// ── BBolt 测试 helper ──

func newTestBBolt(t *testing.T) *BBolt {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := NewBBolt(path)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ── bbolt 存取 ──

func TestBBolt_SaveMessage_HLCMonotonic(t *testing.T) {
	st := newTestBBolt(t)
	var prev uint64
	for i := 0; i < 10; i++ {
		hlc, err := st.SaveMessage(model.Message{Category: "default", Body: fmt.Sprintf("m%d", i)})
		if err != nil {
			t.Fatalf("SaveMessage %d: %v", i, err)
		}
		if hlc <= prev {
			t.Fatalf("HLC not monotonic: hlc=%d prev=%d", hlc, prev)
		}
		prev = hlc
	}
}

func TestBBolt_SaveMessage_RestartPreservesHLC(t *testing.T) {
	// 写 5 条 → Close → 重开同 path → 写第 6 条 → HLC[6]>HLC[5]（meta/last_hlc 持久化生效）
	path := filepath.Join(t.TempDir(), "test.db")
	st1, err := NewBBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	var fifth uint64
	for i := 0; i < 5; i++ {
		fifth, _ = st1.SaveMessage(model.Message{Category: "default"})
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := NewBBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	sixth, err := st2.SaveMessage(model.Message{Category: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if sixth <= fifth {
		t.Fatalf("HLC not preserved across restart: sixth=%d fifth=%d", sixth, fifth)
	}
}

func TestBBolt_MessagesSince_OpenInterval(t *testing.T) {
	st := newTestBBolt(t)
	var hlcs []uint64
	for i := 0; i < 5; i++ {
		hlc, _ := st.SaveMessage(model.Message{Category: "default"})
		hlcs = append(hlcs, hlc)
	}
	// Since(b) 返 [c,d,e]（开区间跳过 b 本身）
	got, err := st.MessagesSince(hlcs[1], 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Since(b): got %d msgs, want 3", len(got))
	}
	for i, m := range got {
		if m.HLC != hlcs[2+i] {
			t.Errorf("got[%d].HLC=%d, want %d", i, m.HLC, hlcs[2+i])
		}
	}
}

func TestBBolt_MessagesSince_Limit(t *testing.T) {
	st := newTestBBolt(t)
	for i := 0; i < 10; i++ {
		st.SaveMessage(model.Message{Category: "default"})
	}
	got, _ := st.MessagesSince(0, 3)
	if len(got) != 3 {
		t.Errorf("limit=3: got %d, want 3", len(got))
	}
}

func TestBBolt_MessagesSince_EmptyDB(t *testing.T) {
	st := newTestBBolt(t)
	got, err := st.MessagesSince(0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty db: got %d msgs, want 0", len(got))
	}
}

func TestBBolt_BigEndianOrdering(t *testing.T) {
	// big-endian key → bbolt Cursor 扫有序（== 数值序）
	st := newTestBBolt(t)
	var expected []uint64
	for i := 0; i < 5; i++ {
		hlc, _ := st.SaveMessage(model.Message{Category: "default"})
		expected = append(expected, hlc)
	}
	got, _ := st.MessagesSince(0, 0)
	if len(got) != 5 {
		t.Fatalf("got %d, want 5", len(got))
	}
	for i, m := range got {
		if m.HLC != expected[i] {
			t.Errorf("order[%d]: got %d, want %d", i, m.HLC, expected[i])
		}
	}
}

func TestBBolt_Message_Single(t *testing.T) {
	st := newTestBBolt(t)
	hlc, _ := st.SaveMessage(model.Message{Category: "default", Body: "hello"})
	got, err := st.Message(hlc)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if got.Body != "hello" {
		t.Errorf("Body: got %q", got.Body)
	}
	_, err = st.Message(hlc + 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("non-existent: got err=%v, want ErrNotFound", err)
	}
}

// 坏 JSON（db 损坏）→ MessagesSince 必返 error 不静默跳过（CLAUDE.md ④ 返回值纪律）。
// 这是子 agent 对抗审查揪出的 C1——原 continue 会把"扫描遇坏"伪装"该条不存在"。
func TestBBolt_MessagesSince_CorruptJSON(t *testing.T) {
	st := newTestBBolt(t)
	if _, err := st.SaveMessage(model.Message{Category: "default", Body: "good"}); err != nil {
		t.Fatal(err)
	}
	// 直接写坏 JSON 到 msgs 桶（绕过 SaveMessage，模拟磁盘故障/schema 坏）
	var badKey [8]byte
	binary.BigEndian.PutUint64(badKey[:], 1<<20) // 不与真实 HLC 冲突的 key
	if err := st.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketMsgs)).Put(badKey[:], []byte("not json"))
	}); err != nil {
		t.Fatal(err)
	}
	_, err := st.MessagesSince(0, 100)
	if err == nil {
		t.Error("corrupt JSON should return error (not silently skip and dig a hole in HLC chain)")
	}
}

// ── ErrNotFound 全分支（返回值纪律 ④）──

func TestBBolt_ErrNotFound(t *testing.T) {
	st := newTestBBolt(t)
	if _, err := st.GetDevice("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDevice: got %v, want ErrNotFound", err)
	}
	if _, err := st.GetMedia("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMedia: got %v, want ErrNotFound", err)
	}
	if _, err := st.Message(99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Message: got %v, want ErrNotFound", err)
	}
	if err := st.RemoveDevice("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveDevice: got %v, want ErrNotFound", err)
	}
	if err := st.TouchDeviceSeen("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchDeviceSeen: got %v, want ErrNotFound", err)
	}
}

// ── Device ──

func TestBBolt_RegisterDevice_NewAndRefresh(t *testing.T) {
	st := newTestBBolt(t)
	if err := st.RegisterDevice(model.Device{UUID: "u1", Platform: "harmony", PushToken: "tok1", Name: "phone"}); err != nil {
		t.Fatal(err)
	}
	d1, err := st.GetDevice("u1")
	if err != nil {
		t.Fatal(err)
	}
	if d1.PushToken != "tok1" || d1.CreatedAt.IsZero() {
		t.Errorf("new register: token=%q createdZero=%v", d1.PushToken, d1.CreatedAt.IsZero())
	}
	created := d1.CreatedAt

	time.Sleep(time.Millisecond) // 确保 UpdatedAt 推进
	if err := st.RegisterDevice(model.Device{UUID: "u1", PushToken: "tok2"}); err != nil {
		t.Fatal(err)
	}
	d2, _ := st.GetDevice("u1")
	if d2.PushToken != "tok2" {
		t.Errorf("refresh token: got %q, want tok2", d2.PushToken)
	}
	if !d2.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt changed: %v -> %v", created, d2.CreatedAt)
	}
	if !d2.UpdatedAt.After(created) {
		t.Errorf("UpdatedAt not advanced")
	}
}

func TestBBolt_AllDevices(t *testing.T) {
	st := newTestBBolt(t)
	for i := 0; i < 3; i++ {
		st.RegisterDevice(model.Device{UUID: fmt.Sprintf("u%d", i), PushToken: "t"})
	}
	devs, err := st.AllDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Errorf("AllDevices: got %d, want 3", len(devs))
	}
}

func TestBBolt_RemoveDevice(t *testing.T) {
	st := newTestBBolt(t)
	st.RegisterDevice(model.Device{UUID: "u1", PushToken: "t"})
	if err := st.RemoveDevice("u1"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if _, err := st.GetDevice("u1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after remove: got %v, want ErrNotFound", err)
	}
}

func TestBBolt_TouchDeviceSeen(t *testing.T) {
	st := newTestBBolt(t)
	st.RegisterDevice(model.Device{UUID: "u1", PushToken: "t"})
	before, _ := st.GetDevice("u1")
	if !before.LastSeenAt.IsZero() {
		t.Errorf("prereq: LastSeenAt should be zero on register")
	}
	if err := st.TouchDeviceSeen("u1"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetDevice("u1")
	if after.LastSeenAt.IsZero() {
		t.Errorf("LastSeenAt not set after touch")
	}
	// 不存在 → ErrNotFound（不静默创建）
	if err := st.TouchDeviceSeen("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("touch non-existent: got %v, want ErrNotFound", err)
	}
}

// ── Cursor ──

func TestBBolt_Cursor_Overwrite(t *testing.T) {
	st := newTestBBolt(t)
	st.SetCursor(model.Cursor{View: "list", FocusHLC: 100, ReportedAt: 1})
	st.SetCursor(model.Cursor{View: "chat", FocusHLC: 200, ReportedAt: 2})
	got, err := st.GetCursor()
	if err != nil {
		t.Fatal(err)
	}
	if got.FocusHLC != 200 || got.View != "chat" {
		t.Errorf("overwrite last-write-wins: got %+v", got)
	}
}

func TestBBolt_Cursor_NotSet(t *testing.T) {
	st := newTestBBolt(t)
	got, err := st.GetCursor()
	if err != nil {
		t.Errorf("unset cursor should not error: %v", err)
	}
	if got.FocusHLC != 0 {
		t.Errorf("unset cursor should be zero value (零游标合法)")
	}
}

// ── Keys first-set + key2 ──

func TestBBolt_SetKey1FirstSet_Wins(t *testing.T) {
	st := newTestBBolt(t)
	got, err := st.SetKey1FirstSet("k1")
	if err != nil || got != "k1" {
		t.Fatalf("first set: got %q err=%v", got, err)
	}
	got2, err := st.SetKey1FirstSet("k2")
	if err != nil || got2 != "k1" {
		t.Errorf("second set should win (first-set wins): got %q, want k1", got2)
	}
	keys, _ := st.GetKeys()
	if keys.Key1 != "k1" {
		t.Errorf("GetKeys Key1: got %q", keys.Key1)
	}
}

func TestBBolt_EnsureKey2_Idempotent(t *testing.T) {
	st := newTestBBolt(t)
	k2a, err := st.EnsureKey2()
	if err != nil || k2a == "" {
		t.Fatalf("first EnsureKey2: %q err=%v", k2a, err)
	}
	k2b, _ := st.EnsureKey2()
	if k2b != k2a {
		t.Errorf("second EnsureKey2: got %q, want %q (idempotent)", k2b, k2a)
	}
}

func TestBBolt_EnsureKey2_Persisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st1, _ := NewBBolt(path)
	k2a, _ := st1.EnsureKey2()
	st1.Close()

	st2, _ := NewBBolt(path)
	defer st2.Close()
	k2b, _ := st2.EnsureKey2()
	if k2a != k2b {
		t.Errorf("key2 not persisted across restart: %q vs %q", k2a, k2b)
	}
}

func TestBBolt_ResetKeys(t *testing.T) {
	st := newTestBBolt(t)
	st.SetKey1FirstSet("k1")
	st.EnsureKey2()
	if err := st.ResetKeys(); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.GetKeys()
	if keys.Key1 != "" || keys.Key2 != "" {
		t.Errorf("ResetKeys: got %+v, want empty", keys)
	}
}

// ── Keys 鉴权决策（CP2：AuthorizeRead/ResolveRegisterKey，§8/§19）──

// TestBBolt_AuthorizeRead 读端点 key1 准入三态：未设锁（窗口 A 读端点全拒）/ first-set 后对放行 / 错·空拒。
func TestBBolt_AuthorizeRead(t *testing.T) {
	st := newTestBBolt(t)
	// 未设（窗口 A）：读端点锁，任何 provided 都拒（§19，首注前防 share URL 静默拉历史）。
	if ok, err := st.AuthorizeRead(""); err != nil || ok {
		t.Errorf("unset+empty: ok=%v err=%v, want false/nil", ok, err)
	}
	if ok, err := st.AuthorizeRead("whatever"); err != nil || ok {
		t.Errorf("unset+random: ok=%v err=%v, want false/nil", ok, err)
	}
	// first-set 后：对→true，错→false，空→false。
	st.SetKey1FirstSet("the-key")
	if ok, err := st.AuthorizeRead("the-key"); err != nil || !ok {
		t.Errorf("set+correct: ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, err := st.AuthorizeRead("wrong"); err != nil || ok {
		t.Errorf("set+wrong: ok=%v err=%v, want false/nil", ok, err)
	}
	if ok, err := st.AuthorizeRead(""); err != nil || ok {
		t.Errorf("set+empty: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestBBolt_ResolveRegisterKey register 三态决策 + first-set：
// 窗口 A first-set / 已设+对返 existing / 已设+错·空拒 / 窗口 A 带 stale 忽略（server 不采纳客户端 secret）。
func TestBBolt_ResolveRegisterKey(t *testing.T) {
	st := newTestBBolt(t)

	// 1) 窗口 A（未设）+ 空 provided → first-set，返非空 winner，allowed=true。
	k1, allowed, err := st.ResolveRegisterKey("")
	if err != nil || !allowed || k1 == "" {
		t.Fatalf("window A empty: k1=%q allowed=%v err=%v", k1, allowed, err)
	}
	if keys, _ := st.GetKeys(); keys.Key1 != k1 {
		t.Errorf("persisted Key1=%q, want %q", keys.Key1, k1)
	}

	// 2) 已设 + 正确 provided → 返 existing，allowed=true（再注册/token 刷新路径）。
	k1b, allowed2, err := st.ResolveRegisterKey(k1)
	if err != nil || !allowed2 || k1b != k1 {
		t.Errorf("set+correct: k1=%q allowed=%v, want %q/true", k1b, allowed2, k1)
	}

	// 3) 已设 + 错 provided → ("", false, nil)（401）。
	got, gotAllowed, err := st.ResolveRegisterKey("wrong")
	if err != nil || gotAllowed || got != "" {
		t.Errorf("set+wrong: got=%q allowed=%v err=%v, want \"\"/false/nil", got, gotAllowed, err)
	}

	// 4) 已设 + 空 provided → ("", false, nil)（401，防 B 蹭全广播）。
	got, gotAllowed, err = st.ResolveRegisterKey("")
	if err != nil || gotAllowed || got != "" {
		t.Errorf("set+empty: got=%q allowed=%v err=%v, want \"\"/false/nil", got, gotAllowed, err)
	}

	// 5) 窗口 A + stale provided（新 store）→ 忽略 stale，first-set 返 winner（≠ stale）。
	stFresh := newTestBBolt(t)
	k1c, allowed3, err := stFresh.ResolveRegisterKey("stale-attempt")
	if err != nil || !allowed3 || k1c == "" || k1c == "stale-attempt" {
		t.Errorf("window A stale: k1=%q allowed=%v, want non-empty != stale / true", k1c, allowed3)
	}
}

// TestBBolt_ResolveRegisterKey_ConcurrentFirstSet 并发首注 first-set wins 不变量：
// N goroutine 同放进入窗口 A → 恰一个 winner first-set，其余（bbolt Update 事务串行化）读到已设→false；
// 所有 allowed=true 结果返同一 key1 + 落库一致（无两个不同 winner = CAS 未破）。容忍个别 false（窗口被 winner 关闭）。
func TestBBolt_ResolveRegisterKey_ConcurrentFirstSet(t *testing.T) {
	st := newTestBBolt(t)
	const goroutines = 16
	resolvedKeys := make([]string, goroutines)
	resolvedAllowed := make([]bool, goroutines)
	start := make(chan struct{}) // barrier：全部就绪后同放，最大化进入窗口 A 的并发
	var wg sync.WaitGroup
	for idx := 0; idx < goroutines; idx++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, gotOK, err := st.ResolveRegisterKey("")
			if err != nil {
				t.Errorf("ResolveRegisterKey: %v", err)
				return
			}
			resolvedKeys[i] = got
			resolvedAllowed[i] = gotOK
		}(idx)
	}
	close(start)
	wg.Wait()

	// 不变量：所有 allowed=true 返同一 key1 + 落库一致。
	var winner string
	allowedCount := 0
	for i := 0; i < goroutines; i++ {
		if !resolvedAllowed[i] {
			continue
		}
		allowedCount++
		if winner == "" {
			winner = resolvedKeys[i]
		} else if resolvedKeys[i] != winner {
			t.Errorf("first-set race: two different key1 %q vs %q (CAS broken)", winner, resolvedKeys[i])
		}
	}
	if winner == "" {
		t.Fatal("first-set race: no winner (all denied — unexpected)")
	}
	if persisted, _ := st.GetKeys(); persisted.Key1 != winner {
		t.Errorf("first-set race: persisted Key1=%q, want winner %q", persisted.Key1, winner)
	}
	t.Logf("first-set race: %d/%d allowed (first-set wins), winner=%s", allowedCount, goroutines, winner)
}

// TestBBolt_Keys_CorruptBucket keys 桶坏 JSON → 所有读 keys 的方法返 err（不吞）。
// R1 fail-closed 回归锚：err 必须冒泡到调用方（→ HTTP 500），不能被吞降级成 401（否则鉴权可能绕过）。
// 仿 TestBBolt_MessagesSince_CorruptJSON：直接写坏 JSON 到 keys 桶单值。Memory 纯 struct 无法 corrupt，跳过。
func TestBBolt_Keys_CorruptBucket(t *testing.T) {
	st := newTestBBolt(t)
	if err := st.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketKeys)).Put([]byte(keyCurrent), []byte("not json"))
	}); err != nil {
		t.Fatalf("inject corrupt: %v", err)
	}
	// 五条读 keys 路径都必须返 err（不吞、不伪装成功）——这是 R1 结构性消除依赖的不变量。
	if _, err := st.GetKeys(); err == nil {
		t.Error("GetKeys: want err on corrupt keys bucket, got nil")
	}
	if _, err := st.SetKey1FirstSet("k1"); err == nil {
		t.Error("SetKey1FirstSet: want err on corrupt keys bucket, got nil")
	}
	if _, err := st.EnsureKey2(); err == nil {
		t.Error("EnsureKey2: want err on corrupt keys bucket, got nil")
	}
	if _, err := st.AuthorizeRead("whatever"); err == nil {
		t.Error("AuthorizeRead: want err on corrupt keys bucket, got nil")
	}
	if _, _, err := st.ResolveRegisterKey(""); err == nil {
		t.Error("ResolveRegisterKey: want err on corrupt keys bucket, got nil")
	}
}

// ── Media ──

func TestBBolt_SaveGetMedia(t *testing.T) {
	st := newTestBBolt(t)
	id, err := st.SaveMedia(model.Media{Path: "blobs/abc.bin", Size: 1024, MIME: "image/png"})
	if err != nil || id == "" {
		t.Fatalf("SaveMedia: id=%q err=%v", id, err)
	}
	got, err := st.GetMedia(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != "image/png" || got.Size != 1024 || got.Path != "blobs/abc.bin" {
		t.Errorf("GetMedia: got %+v", got)
	}
}

func TestBBolt_Message_MediaIDs(t *testing.T) {
	// 文字+多图：Body 与 MediaIDs 并列存取（同时发文本+图片场景）
	st := newTestBBolt(t)
	id1, _ := st.SaveMedia(model.Media{MIME: "image/jpeg"})
	id2, _ := st.SaveMedia(model.Media{MIME: "image/png"})
	hlc, _ := st.SaveMessage(model.Message{
		Category: "default",
		Body:     "看这两张图",
		MediaIDs: []string{id1, id2},
	})
	got, _ := st.Message(hlc)
	if len(got.MediaIDs) != 2 || got.MediaIDs[0] != id1 || got.MediaIDs[1] != id2 {
		t.Errorf("MediaIDs round-trip: got %+v", got.MediaIDs)
	}
	if got.Body != "看这两张图" {
		t.Errorf("Body: got %q", got.Body)
	}
}

// ── 并发 smoke（bbolt 单写串行化 + 事务内 HLC 生成正确）──

func TestBBolt_ConcurrentSaveMessage(t *testing.T) {
	st := newTestBBolt(t)
	const goroutines = 8
	const perG = 50
	results := make([][]uint64, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			hlcs := make([]uint64, 0, perG)
			for i := 0; i < perG; i++ {
				hlc, err := st.SaveMessage(model.Message{Category: "default"})
				if err != nil {
					t.Errorf("SaveMessage: %v", err)
					return
				}
				hlcs = append(hlcs, hlc)
			}
			results[idx] = hlcs
		}(g)
	}
	wg.Wait()

	// 总数对 + HLC 全唯一（串行化 + 事务内生成保证）
	seen := make(map[uint64]bool, goroutines*perG)
	var count int
	for _, hlcs := range results {
		count += len(hlcs)
		for _, hlc := range hlcs {
			if seen[hlc] {
				t.Errorf("HLC duplicate: %d", hlc)
			}
			seen[hlc] = true
		}
	}
	if count != goroutines*perG {
		t.Errorf("total: got %d, want %d", count, goroutines*perG)
	}
}

// ── Memory 实现核心子集（同 interface 验证，持久化测试只适用 BBolt）──

func TestMemory_SaveMessage_HLCMonotonic(t *testing.T) {
	st := NewMemory()
	var prev uint64
	for i := 0; i < 10; i++ {
		hlc, err := st.SaveMessage(model.Message{Category: "default"})
		if err != nil {
			t.Fatal(err)
		}
		if hlc <= prev {
			t.Fatalf("HLC not monotonic: %d <= %d", hlc, prev)
		}
		prev = hlc
	}
}

func TestMemory_ErrNotFound(t *testing.T) {
	st := NewMemory()
	if _, err := st.GetDevice("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDevice: %v", err)
	}
	if _, err := st.Message(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Message: %v", err)
	}
}

func TestMemory_SetKey1FirstSet_Wins(t *testing.T) {
	st := NewMemory()
	got, _ := st.SetKey1FirstSet("k1")
	if got != "k1" {
		t.Fatal(got)
	}
	got2, _ := st.SetKey1FirstSet("k2")
	if got2 != "k1" {
		t.Errorf("first-set wins: got %q, want k1", got2)
	}
}

// TestMemory_AuthorizeRead Memory 实现语义同 BBolt（调试/单测覆盖）。
func TestMemory_AuthorizeRead(t *testing.T) {
	st := NewMemory()
	if ok, err := st.AuthorizeRead(""); err != nil || ok {
		t.Errorf("unset: ok=%v err=%v, want false/nil", ok, err)
	}
	st.SetKey1FirstSet("the-key")
	if ok, err := st.AuthorizeRead("the-key"); err != nil || !ok {
		t.Errorf("correct: ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, err := st.AuthorizeRead("wrong"); err != nil || ok {
		t.Errorf("wrong: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestMemory_ResolveRegisterKey Memory 实现语义同 BBolt（mu 串行化等价 CAS）。
func TestMemory_ResolveRegisterKey(t *testing.T) {
	st := NewMemory()
	k1, allowed, err := st.ResolveRegisterKey("")
	if err != nil || !allowed || k1 == "" {
		t.Fatalf("window A: k1=%q allowed=%v err=%v", k1, allowed, err)
	}
	if got, gotAllowed, err := st.ResolveRegisterKey("wrong"); err != nil || gotAllowed || got != "" {
		t.Errorf("set+wrong: got=%q allowed=%v, want \"\"/false", got, gotAllowed)
	}
	if got, gotAllowed, err := st.ResolveRegisterKey(k1); err != nil || !gotAllowed || got != k1 {
		t.Errorf("set+correct: got=%q allowed=%v, want %q/true", got, gotAllowed, k1)
	}
}

func TestMemory_MessagesSince(t *testing.T) {
	st := NewMemory()
	var hlcs []uint64
	for i := 0; i < 3; i++ {
		hlc, _ := st.SaveMessage(model.Message{Category: "default"})
		hlcs = append(hlcs, hlc)
	}
	got, _ := st.MessagesSince(hlcs[0], 0)
	if len(got) != 2 {
		t.Errorf("Memory Since: got %d, want 2", len(got))
	}
}
