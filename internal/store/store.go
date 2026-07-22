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
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// ErrNotFound 查询的 key 不存在。调用方 errors.Is(err, ErrNotFound) 区分"空 vs 失败"，
// 不把"不存在"伪装成"成功返零值"（CLAUDE.md ④ 返回值纪律）。
var ErrNotFound = errors.New("not found")

// HLC 纯函数（nextHLC/packHLC/unpackHLC + 位布局常量）拆到 hlc.go。
// Memory 实现（struct + 18 方法）拆到 memory.go。

// ── Store interface ──
type Store interface {
	// 设备（uuid → Device）
	RegisterDevice(d model.Device) error
	GetDevice(uuid string) (model.Device, error) // ErrNotFound
	AllDevices() ([]model.Device, error)         // 全广播扇出
	RemoveDevice(uuid string) error              // DELETE 端点（Phase 2）
	TouchDeviceSeen(uuid string) error           // WS 连/断更新 LastSeenAt（CP7）；不存在→ErrNotFound

	ClearPushToken(uuid string) error // 死 token 闸门（CP4：pushkit ErrDeadToken → 清 PushToken 防反复推死 token）；不存在→ErrNotFound

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

	// 鉴权决策（CP2，§8/§19）—— HTTP 层只调这两个，永不直接 GetKeys() 后判 Key1==""（那会复活 B-6 同源脚枪）。
	// 两者返 fail-closed bool：err 被吞最坏误拒（401），绝不绕过。
	AuthorizeRead(providedKey1 string) (authorized bool, err error)                // 读端点准入：true=放行 / false=拒（未设或不匹配）/ err=内部错(500)
	ResolveRegisterKey(providedKey1 string) (key1 string, allowed bool, err error) // register 三态决策 + first-set（同事务 atomic CAS）

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

// mutateDevice 读 device → fn 改 → 写回（TD-4 抽，CP4 提前：ClearPushToken 用）。
// 设备必须已存在（不存在→ErrNotFound，不静默创建）；fn 内改 *model.Device 字段。
// RegisterDevice 不用此（它 patch 语义 + 首注册创建，跟「已存在+改」语义不同）；
// TouchDeviceSeen 的 Get→改→Put 骨架同源重复，留 Phase 2 顺流迁来（CP4 不夹带改已测代码）。
func (s *BBolt) mutateDevice(tx *bolt.Tx, uuid string, fn func(*model.Device)) error {
	bucket := tx.Bucket([]byte(bucketDevice))
	raw := bucket.Get([]byte(uuid))
	if raw == nil {
		return ErrNotFound
	}
	var dev model.Device
	if err := json.Unmarshal(raw, &dev); err != nil {
		return err
	}
	fn(&dev)
	data, err := json.Marshal(dev)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(uuid), data)
}

// ClearPushToken 清空设备 PushToken（TD-4，CP4 死 token 闸门落点）。
// pushkit 返 ErrDeadToken（华为 80100000/80300007）→ fanoutPush 调此清 token，防反复推死 token
// 浪费云函数/Push Kit 配额 + 日志噪声。设备不存在→ErrNotFound（不静默创建）。
// RegisterDevice patch 跳过空串（store.go RegisterDevice 内 if d.PushToken!=""）故无法用它清 token，专用此方法。
func (s *BBolt) ClearPushToken(uuid string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return s.mutateDevice(tx, uuid, func(dev *model.Device) {
			dev.PushToken = ""
			dev.UpdatedAt = time.Now()
		})
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

// MessagesSince 返回消息（升序 旧→新），按 HLC 游标语义：
//   - since=0（无游标）→ 最新 N（client 默认收最新；Cursor.Last 倒序 N + 反转升序）。修 TD-19
//     （原 since=0 c.First 从最老扫返最老 N 的 CP1 临时 bug，DB>N 读不回新消息）。
//   - since>0 → since 之后升序（开区间跳 since；补漏增量）。
//
// limit<=0 不限。坏 JSON 返 err 不吞（CLAUDE.md ④——不把"扫描遇坏"伪装"该条不存在"）。
func (s *BBolt) MessagesSince(since uint64, limit int) ([]model.Message, error) {
	out := []model.Message{} // 非 nil 空 slice（返 [] 不返 null）
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketMsgs)).Cursor()
		// unmarshal + corrupt err 不吞（坏 JSON 返 err 不静默跳）
		unmarshal := func(key, val []byte) (model.Message, error) {
			var msg model.Message
			if err := json.Unmarshal(val, &msg); err != nil {
				return model.Message{}, fmt.Errorf("msgs bucket corrupt at hlc=%d: %w", binary.BigEndian.Uint64(key), err)
			}
			return msg, nil
		}
		if since == 0 {
			// since=0（无游标）→ 最新 N：Cursor.Last 倒序取 N（新→旧）+ 反转升序（旧→新）
			reversed := []model.Message{}
			for key, val := c.Last(); key != nil; key, val = c.Prev() {
				if limit > 0 && len(reversed) >= limit {
					break
				}
				msg, err := unmarshal(key, val)
				if err != nil {
					return err
				}
				reversed = append(reversed, msg)
			}
			for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
				reversed[left], reversed[right] = reversed[right], reversed[left] // 反转升序（旧→新）
			}
			out = reversed
			return nil
		}
		// since>0：since 之后升序（补漏增量；开区间跳 since 本身）
		var seekKey [8]byte
		binary.BigEndian.PutUint64(seekKey[:], since)
		key, val := c.Seek(seekKey[:])
		if key != nil && binary.BigEndian.Uint64(key) == since {
			key, val = c.Next() // 开区间：跳过 since 本身
		}
		for ; key != nil; key, val = c.Next() {
			if limit > 0 && len(out) >= limit {
				break
			}
			msg, err := unmarshal(key, val)
			if err != nil {
				return err
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

// ── Keys（first-set + 启动生成，§8/§9；CP2 加鉴权决策）──

// loadKeys 读 keys 桶单值（CP2 抽出：GetKeys/SetKey1FirstSet/EnsureKey2/AuthorizeRead/ResolveRegisterKey 共用，
// 单点 unmarshal + err 不吞——同源 B-6 防线集中一处，避免散落副本各自漏检）。
// 未设返零值 model.Keys{} + nil（合法，调用方按 Key1/Key2=="" 判未设）；坏 JSON 返零值 + err（调用方必检）。
func loadKeys(tx *bolt.Tx) (model.Keys, error) {
	var keys model.Keys
	v := tx.Bucket([]byte(bucketKeys)).Get([]byte(keyCurrent))
	if v == nil {
		return keys, nil
	}
	if err := json.Unmarshal(v, &keys); err != nil {
		return model.Keys{}, fmt.Errorf("keys bucket corrupt: %w", err)
	}
	return keys, nil
}

// saveKeys Marshal + Put keys 桶单值（TD-2 DRY：SetKey1FirstSet/EnsureKey2/ResolveRegisterKey 三处
// loadKeys→改 KeyN→Marshal→Put 重复，抽此 helper；与 loadKeys 对称的写侧单点）。
func saveKeys(tx *bolt.Tx, keys model.Keys) error {
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(bucketKeys)).Put([]byte(keyCurrent), data)
}

// key1Matches constant-time 比对 provided 与 current（secret 比对卫生，防时序侧信道）。
// key1 固定 64-hex；长度不等直接 false（不泄露内容）。
func key1Matches(provided, current string) bool {
	if current == "" {
		return false // current 必非空：防 subtle.ConstantTimeCompare(空,空)==1 返 true 的 quirk（调用方虽先判未设，收进函数防 CP5 新调用方踩坑→鉴权绕过）
	}
	return len(provided) == len(current) && subtle.ConstantTimeCompare([]byte(provided), []byte(current)) == 1
}

func (s *BBolt) GetKeys() (model.Keys, error) {
	var keys model.Keys
	err := s.db.View(func(tx *bolt.Tx) error {
		var loadErr error
		keys, loadErr = loadKeys(tx)
		return loadErr
	})
	return keys, err
}

// SetKey1FirstSet first-set wins：key1 已设返已存的（不动）；未设写入返写入值。
// bbolt.Update 事务串行化 = 天然 CAS（读改写原子），不需要 legacy hotify-bridge/server.go:139 RLock+Lock 两段式。
func (s *BBolt) SetKey1FirstSet(key1 string) (string, error) {
	var result string
	err := s.db.Update(func(tx *bolt.Tx) error {
		// loadKeys 坏 JSON 不吞（B-6：吞了 keys.Key1=="" 误判"未设"→ first-set wins 被绕过）。
		keys, loadErr := loadKeys(tx)
		if loadErr != nil {
			return loadErr
		}
		if keys.Key1 != "" {
			result = keys.Key1 // 已设 → wins，不动
			return nil
		}
		keys.Key1 = key1
		if err := saveKeys(tx, keys); err != nil {
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
		// loadKeys 坏 JSON 不吞（同 SetKey1FirstSet B-6；否则 keys.Key2=="" 误判"未生成"→ key2 被重置）。
		keys, loadErr := loadKeys(tx)
		if loadErr != nil {
			return loadErr
		}
		if keys.Key2 != "" {
			result = keys.Key2
			return nil
		}
		newKey2, err := newID()
		if err != nil {
			return err
		}
		keys.Key2 = newKey2
		if err := saveKeys(tx, keys); err != nil {
			return err
		}
		result = newKey2
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

// AuthorizeRead 读端点 key1 准入（CP2，§8/§19）。HTTP 读端点（messages/media/cursor/stream）经 requireKey1 调此。
//   - true  = key1 已设 且 provided 匹配 → 放行
//   - false = key1 未设（窗口锁，首注前读端点全拒）或 provided 不匹配/空 → 401
//   - err   = keys 桶损坏（loadKeys 不吞）→ 500
//
// fail-closed：返 bool 零值=false=拒，HTTP 层即使吞 err 也只误拒不绕过（R1 结构性消除）。
func (s *BBolt) AuthorizeRead(provided string) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		keys, loadErr := loadKeys(tx)
		if loadErr != nil {
			return loadErr
		}
		if keys.Key1 == "" {
			ok = false // 窗口未设：读端点锁（§19，防 share URL 静默拉历史），首注后自动解锁
			return nil
		}
		ok = key1Matches(provided, keys.Key1)
		return nil
	})
	return ok, err
}

// ResolveRegisterKey register 的 key1 三态决策 + first-set（CP2，§8/§9）。handleAPIRegister 调此。
// 单 Update 事务内 read-check + first-set（atomic CAS，无 TOCTOU；并发首注 first-set wins 只一个赢）。
//   - key1 未设（窗口 A）→ first-set(newID) 返 winner，allowed=true（provided 被忽略——server 始终自生成，客户端不选 secret）
//   - key1 已设 + provided 匹配 → 返 existing，allowed=true（再注册/token 刷新）
//   - key1 已设 + provided 不匹配/空 → ("", false)（401，防 B 蹭全广播）
//   - keys 桶损坏 → (..., err)（500）
//
// fail-closed：allowed 零值=false=拒。newID 在 store 内（不导出 NewID），与 EnsureKey2 同源对称（R2 消除）。
func (s *BBolt) ResolveRegisterKey(provided string) (string, bool, error) {
	var authKey1 string
	var allowed bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		keys, loadErr := loadKeys(tx)
		if loadErr != nil {
			return loadErr
		}
		if keys.Key1 == "" {
			// 窗口 A：first-set（server 生成，与 EnsureKey2 同源用 newID；不导出 NewID）。
			newKey1, genErr := newID()
			if genErr != nil {
				return genErr
			}
			keys.Key1 = newKey1
			if err := saveKeys(tx, keys); err != nil {
				return err
			}
			authKey1 = newKey1
			allowed = true
			return nil
		}
		// 已设：验 provided（provided 空/错都 false→401）。
		if key1Matches(provided, keys.Key1) {
			authKey1 = keys.Key1
			allowed = true
		}
		return nil
	})
	return authKey1, allowed, err
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
