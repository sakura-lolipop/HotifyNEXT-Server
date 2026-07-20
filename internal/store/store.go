// 存储层：bbolt 持久化实现 + 内存调试实现 + HLC 生成纯函数。
//
// 重写对齐 NEXT 架构（docs/NEXT-Server.md）：
//   - bbolt 7 桶（device/msgs/cursor/media/keys/meta/profile，§4）
//   - HLC 消息 key（pt48+ctr16 pack 成 uint64；bbolt big-endian 字节字典序=数值序，§7）
//   - first-set wins key1（bbolt.Update 事务串行化=天然 CAS，§9）
//   - 砍 read set（§14）
//   - 返回值 (T,error)+ErrNotFound（区分"空 vs 失败"，CLAUDE.md ④——legacy getMessages 把失败兜底成[] 的屎山反面）
package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// ErrNotFound 查询的 key 不存在。调用方 errors.Is(err, ErrNotFound) 区分"空 vs 失败"，
// 不把"不存在"伪装成"成功返零值"（CLAUDE.md ④ 返回值纪律）。
var ErrNotFound = errors.New("not found")

// ── HLC 纯函数（BBolt 和 Memory 共享同一份，测试覆盖统一）──
//
// 位布局：pt 高 48 位（ns，~287 年到 ~2257）+ counter 低 16 位（0..65535）。
// 否决 counter 低 2 位（NTP 回退几秒就溢出破坏单调）；16 位=65536/秒容错，单用户永不到。
// big-endian 编码成 8 字节后，字节字典序 == 数值序 → bbolt Cursor.First() 取最小（最老）。
const (
	hlcPtMask  uint64 = (1 << 48) - 1 // pt 低 48 位掩码（截断防溢出到 counter 区）
	hlcCtrMask uint64 = (1 << 16) - 1 // counter 低 16 位掩码
	hlcCtrMax  uint64 = 0xFFFF
)

// nextHLC 生成下一个 HLC 的 (pt, counter)（docs/NEXT-Server.md §7 算法）。
// 调用方须持锁（BBolt 在 db.Update 事务内 + hlcState.mu；Memory 在 mu）保证 lastPt/lastCtr 读改写互斥。
// nowNs 传参（不内部 time.Now）便于测试注入固定时钟验单调。
func nextHLC(lastPt, lastCtr, nowNs uint64) (newPt, newCtr uint64) {
	nowNs &= hlcPtMask // 48 位截断
	if nowNs > lastPt {
		return nowNs, 0 // 时钟前进：用新 pt，counter 归零
	}
	if lastCtr >= hlcCtrMax {
		// counter 溢出（理论 65536/秒 不触发）：pt 前进 1 让出 counter 空间 + log warn 留痕。
		// 注：此后 HLC.pt 偏离物理时钟（虚假前进），但展示用 Message.TS（物理 ns）不用 HLC.pt → 展示不失真；
		// 补漏靠 HLC 单调（pt+1 仍 > 旧 HLC）不受影响。
		log.Printf("[WARN] hlc counter overflow at pt=%d, forcing pt+1 (single-user load should never hit this)", lastPt)
		return lastPt + 1, 0
	}
	return lastPt, lastCtr + 1 // 停滞/回退：pt 不动，counter+1（保单调）
}

// packHLC 把 (pt, counter) 编码成单 uint64（pt 占高 48 位，counter 低 16 位）。
func packHLC(pt, ctr uint64) uint64 { return (pt << 16) | (ctr & hlcCtrMask) }

// unpackHLC 反解。
func unpackHLC(hlc uint64) (pt, ctr uint64) { return hlc >> 16, hlc & hlcCtrMask }

// ── Store interface ──
type Store interface {
	// 设备（uuid → Device）
	RegisterDevice(d model.Device) error
	GetDevice(uuid string) (model.Device, error) // ErrNotFound
	AllDevices() ([]model.Device, error)         // 全广播扇出
	RemoveDevice(uuid string) error              // DELETE 端点（Phase 2）
	TouchDeviceSeen(uuid string) error           // WS 连/断更新 LastSeenAt（CP7）；不存在→ErrNotFound

	// 消息（HLC key）
	SaveMessage(m model.Message) (uint64, error)                    // 返回分配的 HLC
	MessagesSince(since uint64, limit int) ([]model.Message, error) // since 之后升序（旧→新）；since=0 从最老；limit<=0 不限
	Message(hlc uint64) (model.Message, error)                      // ErrNotFound

	// 媒体（metadata；blob I/O 后续 CP）
	SaveMedia(m model.Media) (string, error) // 返回生成的 media_id（store 管 id 生成）
	GetMedia(id string) (model.Media, error) // ErrNotFound

	// 游标（覆盖式单值，§13）
	SetCursor(c model.Cursor) error
	GetCursor() (model.Cursor, error) // 未设返零值+nil（零游标合法，非 ErrNotFound）

	// Keys（first-set + 启动生成，§8/§9）
	GetKeys() (model.Keys, error)
	SetKey1FirstSet(key1 string) (string, error) // first-set wins：已设返已存的
	EnsureKey2() (string, error)                 // 启动调：未存在才生成
	ResetKeys() error                            // CLI 紧急重置（CP1 占位实装）

	Close() error
}

// ── bbolt 桶名 ──
const (
	bucketDevice  = "device"  // key=uuid, value=Device JSON
	bucketMsgs    = "msgs"    // key=HLC big-endian 8B, value=Message JSON
	bucketCursor  = "cursor"  // 单值 key=keyCurrent, value=Cursor JSON
	bucketMedia   = "media"   // key=media_id, value=Media JSON
	bucketKeys    = "keys"    // 单值 key=keyCurrent（只鉴权 key1/key2）
	bucketMeta    = "meta"    // 单值 key=keyLastHLC（HLC 高水位）+ 未来 schema_version/compact_at
	bucketProfile = "profile" // CP1 建空桶，内容后续 CP（呈现 profile，§13b）
)

const (
	keyCurrent = "current"  // keys/cursor/profile 单值桶的固定 key
	keyLastHLC = "last_hlc" // meta 桶存 HLC 高水位（big-endian 8B）
)

// ── BBolt 持久化实现 ──
type BBolt struct {
	db *bolt.DB
	// hlcState 进程内缓存 last HLC（免每条消息读 meta 桶）。
	// 和 meta/keyLastHLC 双轨：crash 后 loadHLCState 从 meta 恢复一致。
	// mu 保护 lastPt/lastCtr 读改写（SaveMessage 事务内加锁，事务串行化 + mu 双重保证）。
	hlcState struct {
		mu      sync.Mutex
		lastPt  uint64
		lastCtr uint64
	}
}

// NewBBolt 打开 db 文件 + 建 7 桶 + 恢复 HLC 高水位。
// bbolt 单进程独占文件锁——误启第二实例会卡 Timeout(1s) 后报错（main.go 提示"可能已有实例在跑"）。
func NewBBolt(path string) (*BBolt, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{bucketDevice, bucketMsgs, bucketCursor, bucketMedia, bucketKeys, bucketMeta, bucketProfile} {
			if _, e := tx.CreateBucketIfNotExists([]byte(name)); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	st := &BBolt{db: db}
	if err := st.loadHLCState(); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *BBolt) Close() error { return s.db.Close() }

// loadHLCState 启动恢复 HLC 高水位（防重启后 HLC 回退破坏单调）。
func (s *BBolt) loadHLCState() error {
	return s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketMeta)).Get([]byte(keyLastHLC))
		if v == nil {
			return nil // 首启无历史 HLC
		}
		last := binary.BigEndian.Uint64(v)
		s.hlcState.lastPt, s.hlcState.lastCtr = unpackHLC(last)
		return nil
	})
}

// ── 设备 ──

// RegisterDevice 上报/更新设备（patch 语义：非空字段覆盖，空字段保留旧值——无法主动清空字段）。
// token 刷新走同路径（同 uuid 再注更新 PushToken+UpdatedAt，CreatedAt 不变）。清空字段走 RemoveDevice + 重注。
func (s *BBolt) RegisterDevice(d model.Device) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketDevice))
		now := time.Now()
		var dev model.Device
		if v := b.Get([]byte(d.UUID)); v != nil {
			if err := json.Unmarshal(v, &dev); err != nil {
				return err
			}
		}
		if dev.CreatedAt.IsZero() {
			dev.CreatedAt = now // 首注册
		}
		dev.UUID = d.UUID
		if d.Platform != "" {
			dev.Platform = d.Platform
		}
		if d.PushToken != "" {
			dev.PushToken = d.PushToken // token 刷新
		}
		if d.Type != "" {
			dev.Type = d.Type
		}
		if d.Name != "" {
			dev.Name = d.Name
		}
		dev.UpdatedAt = now
		data, err := json.Marshal(dev)
		if err != nil {
			return err
		}
		return b.Put([]byte(dev.UUID), data)
	})
}

func (s *BBolt) GetDevice(uuid string) (model.Device, error) {
	var dev model.Device
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketDevice)).Get([]byte(uuid))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &dev)
	})
	return dev, err
}

func (s *BBolt) AllDevices() ([]model.Device, error) {
	out := []model.Device{} // 非 nil 空 slice（NEXT-client §21 屎山警示：返 [] 不返 null，客户端分得清空 vs 错）
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketDevice)).ForEach(func(_ []byte, v []byte) error {
			var dev model.Device
			if err := json.Unmarshal(v, &dev); err != nil {
				return err
			}
			out = append(out, dev)
			return nil
		})
	})
	return out, err
}

func (s *BBolt) RemoveDevice(uuid string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketDevice))
		if v := b.Get([]byte(uuid)); v == nil {
			return ErrNotFound
		}
		return b.Delete([]byte(uuid))
	})
}

// TouchDeviceSeen 更新 LastSeenAt（WS 连/断调，CP7）。设备不存在→ErrNotFound（不静默创建）。
func (s *BBolt) TouchDeviceSeen(uuid string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketDevice))
		v := b.Get([]byte(uuid))
		if v == nil {
			return ErrNotFound
		}
		var dev model.Device
		if err := json.Unmarshal(v, &dev); err != nil {
			return err
		}
		dev.LastSeenAt = time.Now()
		data, err := json.Marshal(dev)
		if err != nil {
			return err
		}
		return b.Put([]byte(uuid), data)
	})
}

// ── 消息 ──

// SaveMessage 存消息 + 返回分配的 HLC。HLC 在事务内生成（串行化互斥），
// msg 持久化与 meta/last_hlc 同事务原子（无 HLC 复用风险）。
func (s *BBolt) SaveMessage(m model.Message) (uint64, error) {
	var assignedHLC uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		// 1) 生成 HLC（事务串行化 + hlcState.mu 双重保证 lastPt/lastCtr 读改写互斥）
		s.hlcState.mu.Lock()
		defer s.hlcState.mu.Unlock()
		nowNs := uint64(time.Now().UnixNano())
		newPt, newCtr := nextHLC(s.hlcState.lastPt, s.hlcState.lastCtr, nowNs)
		s.hlcState.lastPt = newPt
		s.hlcState.lastCtr = newCtr
		assignedHLC = packHLC(newPt, newCtr)
		// 注：hlcState 内存更新在 Put 前——Put 失败则事务回滚（db 无此消息+meta/last_hlc 不动），
		// 但内存 lastPt 已前进 → 失败的 HLC 号产生"空洞"（db 没这条，后续号已前进）。
		// 接受：对客户端补漏不可见——客户端 since 基于它收到的 HLC，失败的号它从不知道，
		// 补漏拉 >since 中 db 存在的号，序列仍连续。不回滚内存 lastPt（并发回滚逻辑复杂 + 收益小）。
		m.HLC = assignedHLC
		if m.TS == 0 {
			m.TS = int64(nowNs)
		}
		var keyBuf [8]byte
		binary.BigEndian.PutUint64(keyBuf[:], assignedHLC)
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(bucketMsgs)).Put(keyBuf[:], val); err != nil {
			return err
		}
		// 2) 持久化 last HLC（meta 桶，同事务原子）
		var hlcBuf [8]byte
		binary.BigEndian.PutUint64(hlcBuf[:], assignedHLC)
		return tx.Bucket([]byte(bucketMeta)).Put([]byte(keyLastHLC), hlcBuf[:])
	})
	return assignedHLC, err
}

// MessagesSince 返回 since 之后的消息（开区间，升序旧→新）。since=0 从最老扫；limit<=0 不限。
func (s *BBolt) MessagesSince(since uint64, limit int) ([]model.Message, error) {
	out := []model.Message{} // 非 nil 空 slice（同 AllDevices：返 [] 不返 null）
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketMsgs)).Cursor()
		var seekKey [8]byte
		binary.BigEndian.PutUint64(seekKey[:], since)

		var k, v []byte
		if since == 0 {
			k, v = c.First() // 从最老扫
		} else {
			k, v = c.Seek(seekKey[:])
			if k != nil && binary.BigEndian.Uint64(k) == since {
				k, v = c.Next() // 开区间：跳过 since 本身
			}
		}
		for ; k != nil; k, v = c.Next() {
			if limit > 0 && len(out) >= limit {
				break
			}
			var msg model.Message
			if err := json.Unmarshal(v, &msg); err != nil {
				// 坏 JSON = db 损坏（SaveMessage 写入时 marshal 失败会事务回滚不落库；只有磁盘故障/schema 不兼容才会坏）。
				// 不静默 continue（CLAUDE.md ④ 返回值纪律——不把"扫描遇坏"伪装"该条不存在"，否则客户端 since 补漏
				// 会在空洞处无声挖洞 + 无信号）。返 error 让调用方决策（handleHistory 返 500，逼运维修 db）。
				return fmt.Errorf("msgs bucket corrupt at hlc=%d: %w", binary.BigEndian.Uint64(k), err)
			}
			out = append(out, msg)
		}
		return nil
	})
	return out, err
}

func (s *BBolt) Message(hlc uint64) (model.Message, error) {
	var msg model.Message
	var keyBuf [8]byte
	binary.BigEndian.PutUint64(keyBuf[:], hlc)
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketMsgs)).Get(keyBuf[:])
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &msg)
	})
	return msg, err
}

// ── 媒体 ──

// SaveMedia 存 media metadata（blob 走文件系统，后续 CP），返回 media_id（store 内生成）。
func (s *BBolt) SaveMedia(m model.Media) (string, error) {
	if m.ID == "" {
		id, err := newID()
		if err != nil {
			return "", err
		}
		m.ID = id
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketMedia)).Put([]byte(m.ID), data)
	})
	return m.ID, err
}

func (s *BBolt) GetMedia(id string) (model.Media, error) {
	var media model.Media
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketMedia)).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &media)
	})
	return media, err
}

// ── 游标（覆盖式单值，§13）──

func (s *BBolt) SetCursor(c model.Cursor) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketCursor)).Put([]byte(keyCurrent), data)
	})
}

// GetCursor 未设返零值+nil（零游标合法，App 判过期——非 ErrNotFound）。
func (s *BBolt) GetCursor() (model.Cursor, error) {
	var c model.Cursor
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketCursor)).Get([]byte(keyCurrent))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &c)
	})
	return c, err
}

// ── Keys（first-set + 启动生成，§8/§9）──

func (s *BBolt) GetKeys() (model.Keys, error) {
	var keys model.Keys
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketKeys)).Get([]byte(keyCurrent))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &keys)
	})
	return keys, err
}

// SetKey1FirstSet first-set wins：key1 已设返已存的（不动）；未设写入返写入值。
// bbolt.Update 事务串行化 = 天然 CAS（读改写原子），不需要 legacy hotify-bridge/server.go:139 RLock+Lock 两段式。
func (s *BBolt) SetKey1FirstSet(key1 string) (string, error) {
	var result string
	err := s.db.Update(func(tx *bolt.Tx) error {
		kb := tx.Bucket([]byte(bucketKeys))
		var keys model.Keys
		if v := kb.Get([]byte(keyCurrent)); v != nil {
			// 坏 JSON 不吞（CLAUDE.md ④ + 子 agent B-6：吞了会让 keys.Key1=="" 误判"未设"→ first-set wins 被绕过）
			if e := json.Unmarshal(v, &keys); e != nil {
				return fmt.Errorf("keys bucket corrupt: %w", e)
			}
		}
		if keys.Key1 != "" {
			result = keys.Key1 // 已设 → wins，不动
			return nil
		}
		keys.Key1 = key1
		data, err := json.Marshal(keys)
		if err != nil {
			return err
		}
		if err := kb.Put([]byte(keyCurrent), data); err != nil {
			return err
		}
		result = key1
		return nil
	})
	return result, err
}

// EnsureKey2 启动调：key2 未存在才生成（crypto/rand 32 字节 hex）；已存在返原值。
func (s *BBolt) EnsureKey2() (string, error) {
	var result string
	err := s.db.Update(func(tx *bolt.Tx) error {
		kb := tx.Bucket([]byte(bucketKeys))
		var keys model.Keys
		if v := kb.Get([]byte(keyCurrent)); v != nil {
			// 坏 JSON 不吞（同 SetKey1FirstSet B-6；否则 keys.Key2=="" 误判"未生成"→ key2 被重置）
			if e := json.Unmarshal(v, &keys); e != nil {
				return fmt.Errorf("keys bucket corrupt: %w", e)
			}
		}
		if keys.Key2 != "" {
			result = keys.Key2
			return nil
		}
		k2, err := newID()
		if err != nil {
			return err
		}
		keys.Key2 = k2
		data, err := json.Marshal(keys)
		if err != nil {
			return err
		}
		if err := kb.Put([]byte(keyCurrent), data); err != nil {
			return err
		}
		result = k2
		return nil
	})
	return result, err
}

// ResetKeys CLI 紧急重置（清 key1/key2，设备重 register first-set 新 key1）。
func (s *BBolt) ResetKeys() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketKeys)).Delete([]byte(keyCurrent))
	})
}

// newID 生成随机 hex id（crypto/rand 32 字节 = 256 bit 熵，够唯一；YAGNI 不拉 uuid 库）。
// 用于 media_id 和 key2 生成。
func newID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ── Memory 实现（本地调试 + 单测；重启丢，HLC 仅进程内单调）──
//
// 生产用 BBolt；Memory 仅为免文件系统依赖的调试/单测。HLC 用同一份 nextHLC/packHLC 纯函数。
type Memory struct {
	mu         sync.Mutex
	devices    map[string]model.Device
	messages   []model.Message // 按 HLC 升序 append（SaveMessage 保证单调）
	media      map[string]model.Media
	cursor     model.Cursor
	keys       model.Keys
	hlcLastPt  uint64
	hlcLastCtr uint64
}

func NewMemory() *Memory {
	return &Memory{
		devices: make(map[string]model.Device),
		media:   make(map[string]model.Media),
	}
}

func (m *Memory) Close() error { return nil }

func (m *Memory) RegisterDevice(d model.Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	dev := m.devices[d.UUID]
	if dev.CreatedAt.IsZero() {
		dev.CreatedAt = now
	}
	dev.UUID = d.UUID
	if d.Platform != "" {
		dev.Platform = d.Platform
	}
	if d.PushToken != "" {
		dev.PushToken = d.PushToken
	}
	if d.Type != "" {
		dev.Type = d.Type
	}
	if d.Name != "" {
		dev.Name = d.Name
	}
	dev.UpdatedAt = now
	m.devices[d.UUID] = dev
	return nil
}

func (m *Memory) GetDevice(uuid string) (model.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, ok := m.devices[uuid]
	if !ok {
		return model.Device{}, ErrNotFound
	}
	return dev, nil
}

func (m *Memory) AllDevices() ([]model.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out, nil
}

func (m *Memory) RemoveDevice(uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[uuid]; !ok {
		return ErrNotFound
	}
	delete(m.devices, uuid)
	return nil
}

func (m *Memory) TouchDeviceSeen(uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, ok := m.devices[uuid]
	if !ok {
		return ErrNotFound
	}
	dev.LastSeenAt = time.Now()
	m.devices[uuid] = dev
	return nil
}

func (m *Memory) SaveMessage(msg model.Message) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nowNs := uint64(time.Now().UnixNano())
	newPt, newCtr := nextHLC(m.hlcLastPt, m.hlcLastCtr, nowNs)
	m.hlcLastPt = newPt
	m.hlcLastCtr = newCtr
	assigned := packHLC(newPt, newCtr)
	msg.HLC = assigned
	if msg.TS == 0 {
		msg.TS = int64(nowNs)
	}
	m.messages = append(m.messages, msg)
	return assigned, nil
}

func (m *Memory) MessagesSince(since uint64, limit int) ([]model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []model.Message{}
	for _, msg := range m.messages { // messages 升序（HLC 单调）
		if msg.HLC <= since {
			continue // 开区间（since 不含）：跳过 HLC<=since，保留 HLC>since
		}
		out = append(out, msg)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) Message(hlc uint64) (model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages {
		if msg.HLC == hlc {
			return msg, nil
		}
	}
	return model.Message{}, ErrNotFound
}

func (m *Memory) SaveMedia(media model.Media) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if media.ID == "" {
		id, err := newID()
		if err != nil {
			return "", err
		}
		media.ID = id
	}
	m.media[media.ID] = media
	return media.ID, nil
}

func (m *Memory) GetMedia(id string) (model.Media, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	media, ok := m.media[id]
	if !ok {
		return model.Media{}, ErrNotFound
	}
	return media, nil
}

func (m *Memory) SetCursor(c model.Cursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = c
	return nil
}

func (m *Memory) GetCursor() (model.Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor, nil // 未设返零值（零游标合法）
}

func (m *Memory) GetKeys() (model.Keys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys, nil
}

func (m *Memory) SetKey1FirstSet(key1 string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys.Key1 != "" {
		return m.keys.Key1, nil
	}
	m.keys.Key1 = key1
	return key1, nil
}

func (m *Memory) EnsureKey2() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys.Key2 != "" {
		return m.keys.Key2, nil
	}
	k2, err := newID()
	if err != nil {
		return "", err
	}
	m.keys.Key2 = k2
	return k2, nil
}

func (m *Memory) ResetKeys() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = model.Keys{}
	return nil
}
