// Memory 实现（本地调试 + 单测；重启丢，HLC 仅进程内单调）。
// 从 store.go 拆出（TD-1，整块平移零风险）。
//
// 生产用 BBolt（store.go）；Memory 仅为免文件系统依赖的调试/单测。HLC 用同一份 nextHLC/packHLC 纯函数（hlc.go）。
package store

import (
	"sync"
	"time"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

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

// ClearPushToken（Memory 实现，语义同 BBolt；TD-4 CP4 死 token 闸门）。调试/单测用。
func (m *Memory) ClearPushToken(uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, ok := m.devices[uuid]
	if !ok {
		return ErrNotFound
	}
	dev.PushToken = ""
	dev.UpdatedAt = time.Now()
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

// MessagesLatest（Memory 实现，语义同 BBolt；TD-19）。messages 升序（HLC 单调），取末尾 limit = 最新 limit。
func (m *Memory) MessagesLatest(limit int) ([]model.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	out := []model.Message{}
	start := 0
	if len(m.messages) > limit {
		start = len(m.messages) - limit
	}
	out = append(out, m.messages[start:]...)
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

// AuthorizeRead（Memory 实现，语义同 BBolt；调试/单测用）。
func (m *Memory) AuthorizeRead(provided string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys.Key1 == "" {
		return false, nil // 窗口未设：锁
	}
	return key1Matches(provided, m.keys.Key1), nil
}

// ResolveRegisterKey（Memory 实现，语义同 BBolt；mu 串行化等价 bbolt 事务 CAS）。
func (m *Memory) ResolveRegisterKey(provided string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys.Key1 == "" {
		newKey1, err := newID()
		if err != nil {
			return "", false, err
		}
		m.keys.Key1 = newKey1
		return newKey1, true, nil
	}
	if key1Matches(provided, m.keys.Key1) {
		return m.keys.Key1, true, nil
	}
	return "", false, nil
}
