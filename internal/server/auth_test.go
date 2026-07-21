// CP2 L1 接线测试：/api/v1/register first-set + 读端点 requireKey1（HTTP wiring）。
// 鉴权 CAS 不变量由 store L0 覆盖（store_test 的 AuthorizeRead/ResolveRegisterKey/Keys_CorruptBucket）；
// 本文件验 HTTP 装配 + 对抗审查（Agent 2）补的缺口：400 校验 / 设备落库 / corrupt→500 / 并发首注 / legacy 不 first-set / header 优先级。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

// regResp register 响应（解析 code/key1/key2）。
type regResp struct {
	Code int    `json:"code"`
	Key1 string `json:"key1"`
	Key2 string `json:"key2"`
}

// apiRegister 调原生 /api/v1/register，返响应 + 解析体。key1=="" 则不带 Authorization（窗口 A 首设备）。
// 注意：内部用 t.Fatal（NewRequest/Do 出错），**勿在 goroutine 里调**（并发测试改内联 + t.Errorf）。
func apiRegister(t *testing.T, tsURL, key1, payload string) (*http.Response, regResp) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, tsURL+"/api/v1/register", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key1 != "" {
		req.Header.Set("Authorization", "Bearer "+key1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var parsed regResp
	_ = json.NewDecoder(resp.Body).Decode(&parsed) // 401/500 也解析（取 code 字段）
	return resp, parsed
}

// errStore 模拟 keys 桶损坏：覆写 AuthorizeRead/ResolveRegisterKey 返 err，其余方法 delegate *store.BBolt。
// CP3c 跨审 D P2：原嵌入 store.Store interface（零值 nil）触达其他方法会 panic——改嵌入 *BBolt 对齐 push_test.go 风格（errSaveStore/errGetDeviceStore）。
// 测 requireKey1/handleAPIRegister 的 err→500 映射（R1 fail-closed HTTP 契约：err 不降级成 401）。
type errStore struct{ *store.BBolt }

func (errStore) AuthorizeRead(string) (bool, error) {
	return false, errors.New("keys bucket corrupt (injected)")
}
func (errStore) ResolveRegisterKey(string) (string, bool, error) {
	return "", false, errors.New("keys bucket corrupt (injected)")
}

// newCorruptServer 起一个 store 恒返 err 的 server（测 HTTP err→500）。
// 建 bb 让 errStore 嵌入真 *BBolt（覆写两方法返 err，其余 delegate 不 panic）。
func newCorruptServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	bb, err := store.NewBBolt(filepath.Join(dir, "corrupt.db"))
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	t.Cleanup(func() { _ = bb.Close() })
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	cfg := &config.Config{Server: config.ServerConfig{Addr: ":0"}}
	srv := New(cfg, errStore{bb}, pushkit.New(pushkit.Config{}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestAPIRegister_FirstSet 空 server 首设备不带 key1 → first-set + 下发 key1/key2 + 设备落库。
func TestAPIRegister_FirstSet(t *testing.T) {
	ts, _, bb := newSmokeServer(t)
	resp, reg := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1","type":"phone","name":"my phone"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || reg.Code != 200 || reg.Key1 == "" {
		t.Fatalf("first-set: status=%d code=%d key1=%q (want 200 + non-empty key1)", resp.StatusCode, reg.Code, reg.Key1)
	}
	if reg.Key2 == "" {
		t.Errorf("first-set: key2 empty (EnsureKey2 should have run in newSmokeServer)")
	}
	dev, err := bb.GetDevice("dev1") // 设备真落库（Agent 2 #3）
	if err != nil {
		t.Fatalf("GetDevice dev1: %v", err)
	}
	if dev.Platform != "harmony" || dev.Type != "phone" || dev.Name != "my phone" || dev.PushToken != "tok1" {
		t.Errorf("device persisted wrong: %+v", dev)
	}
}

// TestAPIRegister_400_MissingFields 缺 uuid/platform/push_token → 400（Agent 2 #2）。
func TestAPIRegister_400_MissingFields(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	cases := []struct{ name, payload string }{
		{"missing uuid", `{"platform":"harmony","push_token":"tok1"}`},
		{"missing platform", `{"uuid":"dev1","push_token":"tok1"}`},
		{"missing push_token", `{"uuid":"dev1","platform":"harmony"}`},
	}
	for _, tc := range cases {
		resp, _ := apiRegister(t, ts.URL, "", tc.payload)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status=%d, want 400", tc.name, resp.StatusCode)
		}
	}
}

// TestAPIRegister_400_BadJSON 坏 JSON → 400（Agent 2 #2）。
func TestAPIRegister_400_BadJSON(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/register", strings.NewReader("{bad json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad json: status=%d, want 400", resp.StatusCode)
	}
}

// TestAPIRegister_ReRegister_CorrectKey1 同 uuid 带对 key1 再注 → 200 + key2 幂等 + token 刷新落库。
func TestAPIRegister_ReRegister_CorrectKey1(t *testing.T) {
	ts, _, bb := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	resp, reg := apiRegister(t, ts.URL, first.Key1, `{"uuid":"dev1","platform":"harmony","push_token":"tok2"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || reg.Key1 != first.Key1 {
		t.Errorf("re-register: status=%d key1=%q, want 200 + same key1 %q", resp.StatusCode, reg.Key1, first.Key1)
	}
	if reg.Key2 == "" || reg.Key2 != first.Key2 { // Agent 2 #7：re-register 也下发 key2（幂等）
		t.Errorf("re-register key2: got %q, want %q", reg.Key2, first.Key2)
	}
	dev, err := bb.GetDevice("dev1")
	if err != nil {
		t.Fatalf("GetDevice dev1: %v", err)
	}
	if dev.PushToken != "tok2" { // Agent 2 #3：token 刷新落库
		t.Errorf("token refresh not persisted: got %q, want tok2", dev.PushToken)
	}
}

// TestAPIRegister_WrongKey1_401 first-set 后，新设备带错 key1 → 401。
func TestAPIRegister_WrongKey1_401(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	if first.Key1 == "" {
		t.Fatal("first-set setup failed")
	}
	resp, _ := apiRegister(t, ts.URL, "wrong-key1", `{"uuid":"dev2","platform":"harmony","push_token":"tok2"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong key1: status=%d, want 401", resp.StatusCode)
	}
}

// TestAPIRegister_NoKey1AfterSet_401 first-set 后，新设备不带 key1 → 401（防 B 蹭全广播）。
func TestAPIRegister_NoKey1AfterSet_401(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	if first.Key1 == "" {
		t.Fatal("first-set setup failed")
	}
	resp, _ := apiRegister(t, ts.URL, "", `{"uuid":"dev2","platform":"harmony","push_token":"tok2"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no key1 after set: status=%d, want 401", resp.StatusCode)
	}
}

// TestAPIRegister_CorruptKeys_500 keys 桶损坏 → register 500（err 不降级 401，R1 HTTP 契约）。
func TestAPIRegister_CorruptKeys_500(t *testing.T) {
	ts := newCorruptServer(t)
	resp, _ := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("register corrupt keys: status=%d, want 500 (err must not degrade to 401)", resp.StatusCode)
	}
}

// TestAPIRegister_ConcurrentFirstSet HTTP 层并发首注：N 同放 → 恰一个 200（first-set wins）+ 其余 401 + 落库一致。
func TestAPIRegister_ConcurrentFirstSet(t *testing.T) {
	ts, _, bb := newSmokeServer(t)
	const goroutines = 8
	statuses := make([]int, goroutines)
	gotKeys := make([]string, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for idx := 0; idx < goroutines; idx++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/register",
				strings.NewReader(fmt.Sprintf(`{"uuid":"dev%d","platform":"harmony","push_token":"tok%d"}`, idx, idx)))
			if err != nil {
				t.Errorf("NewRequest: %v", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("register: %v", err)
				return
			}
			var reg regResp
			_ = json.NewDecoder(resp.Body).Decode(&reg)
			statuses[idx] = resp.StatusCode
			gotKeys[idx] = reg.Key1
			resp.Body.Close()
		}(idx)
	}
	close(start)
	wg.Wait()

	var winner string
	allowedCount := 0
	for idx := 0; idx < goroutines; idx++ {
		switch statuses[idx] {
		case 200:
			allowedCount++
			if winner == "" {
				winner = gotKeys[idx]
			} else if gotKeys[idx] != winner {
				t.Errorf("two different key1: %q vs %q", winner, gotKeys[idx])
			}
		case 401:
			// 滞后者窗口已关 → 401，容忍
		default:
			t.Errorf("status[%d]=%d, want 200 or 401", idx, statuses[idx])
		}
	}
	if winner == "" {
		t.Fatal("no winner (no 200)")
	}
	persisted, _ := bb.GetKeys()
	if persisted.Key1 != winner {
		t.Errorf("persisted Key1=%q, want winner %q", persisted.Key1, winner)
	}
	t.Logf("concurrent first-set: %d/%d allowed, winner=%s", allowedCount, goroutines, winner)
}

// TestRead_NoKey1_401 first-set 后 GET /messages 无 key1 → 401。
func TestRead_NoKey1_401(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	resp, err := http.Get(ts.URL + "/messages/dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("read no key1: status=%d, want 401", resp.StatusCode)
	}
}

// TestRead_WrongKey1_401 first-set 后 GET /messages 错 key1 → 401（非窗口，真比对失败）。
func TestRead_WrongKey1_401(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("read wrong key1: status=%d, want 401", resp.StatusCode)
	}
}

// TestRead_FirstSetWindow_401 未首注（key1 空）GET /messages → 401（窗口锁，§19）。
func TestRead_FirstSetWindow_401(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	resp, err := http.Get(ts.URL + "/messages/dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("read in first-set window: status=%d, want 401", resp.StatusCode)
	}
}

// TestRead_CorrectKey1_200 first-set 后 GET /messages 带对 key1 → 200。
func TestRead_CorrectKey1_200(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+first.Key1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("read correct key1: status=%d, want 200", resp.StatusCode)
	}
}

// TestRead_CorruptKeys_500 keys 桶损坏 → 读端点 500（err 不降级 401，R1 HTTP 契约）。
func TestRead_CorruptKeys_500(t *testing.T) {
	ts := newCorruptServer(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer somekey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("read corrupt keys: status=%d, want 500 (err must not degrade to 401)", resp.StatusCode)
	}
}

// TestLegacyRegister_DoesNotFirstSet 旧 /register 成功但**不**触发 first-set（与 native 的关键差异）。
// legacy 抢注不能锁死 NEXT App 合法首注（plan 三次强调）→ legacy 后 key1 仍空 + 原生仍能 first-set。
func TestLegacyRegister_DoesNotFirstSet(t *testing.T) {
	ts, _, bb := newSmokeServer(t)
	resp, err := http.Post(ts.URL+"/register", "application/json",
		strings.NewReader(`{"device_key":"legacy1","push_token":"tok","name":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("legacy register: status=%d, want 200", resp.StatusCode)
	}
	keys, _ := bb.GetKeys()
	if keys.Key1 != "" {
		t.Errorf("legacy should NOT first-set: Key1=%q, want empty (window A open)", keys.Key1)
	}
	// legacy 没污染窗口 → 原生不带 key1 仍能 first-set
	resp2, reg := apiRegister(t, ts.URL, "", `{"uuid":"dev2","platform":"harmony","push_token":"tok2"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 || reg.Key1 == "" {
		t.Errorf("native first-set after legacy: status=%d key1=%q, want 200 + non-empty", resp2.StatusCode, reg.Key1)
	}
}

// TestExtractKey1_HeaderOnly query 带 key1 但无 header → 401（allowKey1Query=false 扩展接口默认关）。
func TestExtractKey1_HeaderOnly(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1?key1="+first.Key1, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("query key1 (extension off): status=%d, want 401 (allowKey1Query=false)", resp.StatusCode)
	}
}

// TestExtractKey1_HeaderPriority header+query 同带 → header 赢（Agent 2 #5：扩展点优先级契约，护 CP5）。
func TestExtractKey1_HeaderPriority(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	_, first := apiRegister(t, ts.URL, "", `{"uuid":"dev1","platform":"harmony","push_token":"tok1"}`)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1?key1=wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+first.Key1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("header over query: status=%d, want 200 (header wins)", resp.StatusCode)
	}
}

// TestExtractKey1_NonBearerScheme 非 Bearer scheme（Basic/Digest/Token）→ extractKey1 返空不误解析（CP3c 跨审 C P1 漏测补）。
// 防 Authorization: Basic xxx 的 Basic 值被当 key1 误放行（extractKey1 只匹配 Bearer 前缀，但契约未锁）。
func TestExtractKey1_NonBearerScheme(t *testing.T) {
	cases := []string{
		"Basic abc123",        // Basic 认证不应被当 key1
		"Digest username=x",   // Digest 同
		"Token xyz",           // 非 Bearer 的其他 scheme
		"bearer abc",          // 小写 bearer——EqualFold 匹配 Bearer 前缀大小写不敏感，会提取（验证大小写不敏感契约，非 bug）
	}
	for _, auth := range cases {
		req, err := http.NewRequest(http.MethodGet, "/messages/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		got := extractKey1(req)
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			// bearer（任意大小写）应提取值
			if got == "" {
				t.Errorf("Bearer scheme %q: extractKey1 返空 (want 提取值)", auth)
			}
			continue
		}
		// 非 Bearer scheme 应返空（不误把 Basic/Digest/Token 值当 key1）
		if got != "" {
			t.Errorf("非 Bearer scheme %q: extractKey1=%q (want 空，不误解析)", auth, got)
		}
	}
}
