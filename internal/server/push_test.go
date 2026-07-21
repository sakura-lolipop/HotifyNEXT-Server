// /api/v1/push 原生推送端点测试（CP3b）。
// 验 key1 准入 + JSON→Message→ingest + push 失败不挡落库 + device not found + body limit + 并发 HLC 单调。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

// failPusher mock Pusher 永远返失败（测"push 失败不挡落库"语义）。
type failPusher struct{}

func (failPusher) Send(model.Message, model.Device) error {
	return errors.New("push injected failure")
}

// countingPusher mock Pusher 计数 Send 调用 + 记录收到的 msg/dev（CP3c 跨审 C P1 漏测补：
// 验 fanoutPush 五分支哪些调 Send 哪些不调——no-target/empty-token 分支应 calls==0）。
// 非并发用（calls++ 非 thread-safe；并发测用 failPusher）。
type countingPusher struct {
	calls   int
	lastMsg model.Message
	lastDev model.Device
}

func (c *countingPusher) Send(msg model.Message, dev model.Device) error {
	c.calls++
	c.lastMsg = msg
	c.lastDev = dev
	return nil
}

// errSaveStore 嵌入 *store.BBolt 覆写 SaveMessage 返 err（测 ingest 存失败 → 500，漏测 #3）。
// 其余方法用 *store.BBolt 的（register 真 first-set key1，push 的 SaveMessage 注入 err）。
type errSaveStore struct{ *store.BBolt }

func (errSaveStore) SaveMessage(model.Message) (uint64, error) {
	return 0, errors.New("save injected failure")
}

// errGetDeviceStore 嵌入 *store.BBolt 覆写 GetDevice 返非 ErrNotFound err（测 fanoutPush 内部错分支，漏测 #2）。
type errGetDeviceStore struct{ *store.BBolt }

func (errGetDeviceStore) GetDevice(string) (model.Device, error) {
	return model.Device{}, errors.New("getdevice injected failure")
}

// newPushServerWithStore 起 server 用指定 store（mock store 注入 err 用）。
func newPushServerWithStore(t *testing.T, pk Pusher, st store.Store) *httptest.Server {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{Addr: ":0"}}
	srv := New(cfg, st, pk)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// newPushServer 起 server 指定 pusher（默认空 pushkit stub 只存不推；测 push 失败传 failPusher）。
// 返 *store.BBolt 让测试 MessagesSince 回查落库。
func newPushServer(t *testing.T, pk Pusher) (*httptest.Server, *store.BBolt) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "push.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	t.Cleanup(func() { _ = bb.Close() })
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Store: config.StoreConfig{
			Type: "bbolt", Path: dbPath,
			BlobDir:  filepath.Join(dir, "blobs"),
			MaxBytes: 1 << 30,
		},
	}
	srv := New(cfg, bb, pk)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, bb
}

// registerFirstSet 注册首设备 first-set key1（返 key1，push 端点准入用）。
// 注意：内部用 t.Fatal，勿在 goroutine 里调（并发测试内联 http，见 TestAPIPush_Concurrent）。
func registerFirstSet(t *testing.T, ts *httptest.Server, uuid string) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/v1/register", "application/json",
		strings.NewReader(`{"uuid":"`+uuid+`","platform":"harmony","push_token":"tok-`+uuid+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Code int    `json:"code"`
		Key1 string `json:"key1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if reg.Code != 200 || reg.Key1 == "" {
		t.Fatalf("register: code=%d key1=%q", reg.Code, reg.Key1)
	}
	return reg.Key1
}

// apiPush POST /api/v1/push 带 key1（key1="" 表无 Authorization 头，测 401）。
// 注意：内部用 t.Fatal，勿在 goroutine 里调。
func apiPush(t *testing.T, tsURL, key1, payload string) (*http.Response, apiResp) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, tsURL+"/api/v1/push", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if key1 != "" {
		req.Header.Set("Authorization", "Bearer "+key1)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r apiResp
	json.NewDecoder(resp.Body).Decode(&r)
	return resp, r
}

// TestAPIPush_Success happy path：push + key1 → 200 + 落库回查（title/body/url + category 兜底 default）。
func TestAPIPush_Success(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")

	resp, r := apiPush(t, ts.URL, key1, `{"title":"t1","body":"b1","url":"https://example.com/x"}`)
	defer resp.Body.Close()
	if r.Code != 200 || r.Message != "success" {
		t.Fatalf("push: code=%d msg=%q (want 200 success)", r.Code, r.Message)
	}

	msgs, err := bb.MessagesSince(0, 10)
	if err != nil {
		t.Fatalf("MessagesSince: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs count: got %d, want 1", len(msgs))
	}
	if msgs[0].Title != "t1" || msgs[0].Body != "b1" || msgs[0].URL != "https://example.com/x" {
		t.Errorf("msg fields: title=%q body=%q url=%q", msgs[0].Title, msgs[0].Body, msgs[0].URL)
	}
	if msgs[0].Category != "default" {
		t.Errorf("category 兜底: got %q, want default", msgs[0].Category)
	}
}

// TestAPIPush_AuthFailure 错/无 key1 → 401（first-set 后窗口 A 关）。
func TestAPIPush_AuthFailure(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	registerFirstSet(t, ts, "dev1") // first-set 关窗口 A

	resp, r := apiPush(t, ts.URL, "", `{"title":"t"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || r.Code != 401 {
		t.Errorf("no key1: status=%d code=%d (want 401)", resp.StatusCode, r.Code)
	}

	resp, r = apiPush(t, ts.URL, "wrong-key1", `{"title":"t"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || r.Code != 401 {
		t.Errorf("wrong key1: status=%d code=%d (want 401)", resp.StatusCode, r.Code)
	}
}

// TestAPIPush_400 坏 JSON / 缺必填（title/body/media_ids 全空）→ 400。
func TestAPIPush_400(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")

	resp, r := apiPush(t, ts.URL, key1, `{not-json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || r.Code != 400 {
		t.Errorf("bad json: status=%d code=%d (want 400)", resp.StatusCode, r.Code)
	}

	resp, r = apiPush(t, ts.URL, key1, `{"category":"call"}`) // title/body/media_ids 全空
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || r.Code != 400 {
		t.Errorf("missing required: status=%d code=%d (want 400)", resp.StatusCode, r.Code)
	}
}

// TestAPIPush_PushFailNotBlock failPusher：push 失败但消息落库（200 + "saved but push failed"）。
// 证 ingest 语义：存成功后推失败不挡（消息已落库是主目的）。
func TestAPIPush_PushFailNotBlock(t *testing.T) {
	ts, bb := newPushServer(t, failPusher{})
	key1 := registerFirstSet(t, ts, "dev1")

	resp, r := apiPush(t, ts.URL, key1, `{"title":"t","body":"b","target_uuid":"dev1"}`)
	defer resp.Body.Close()
	if r.Code != 200 {
		t.Fatalf("push fail: code=%d msg=%q (want 200, 消息已落库)", r.Code, r.Message)
	}
	if !strings.Contains(r.Message, msgPushFailed) {
		t.Errorf("push fail msg: %q (want 'saved but push failed')", r.Message)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Errorf("msgs after push fail: %d (want 1, push 失败不挡落库)", len(msgs))
	}
}

// TestAPIPush_DeviceNotFound target_uuid 查不到 → 落历史不推 + 200 success（fanoutPush ErrNotFound 留痕不报错）。
func TestAPIPush_DeviceNotFound(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")

	resp, r := apiPush(t, ts.URL, key1, `{"title":"t","body":"b","target_uuid":"ghost"}`)
	defer resp.Body.Close()
	if r.Code != 200 || r.Message != "success" {
		t.Fatalf("device not found: code=%d msg=%q (want 200 success, 落历史不推)", r.Code, r.Message)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Errorf("msgs: %d (want 1, device not found 也落历史)", len(msgs))
	}
}

// TestAPIPush_BodyLimit body 超 ~1MB → 400（MaxBytesReader 防 OOM/恶意灌）。
func TestAPIPush_BodyLimit(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")

	huge := strings.Repeat("x", 1<<20+100) // 1MB + 100B
	body := `{"title":"` + huge + `"}`
	resp, r := apiPush(t, ts.URL, key1, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || r.Code != 400 {
		t.Errorf("body limit: status=%d code=%d (want 400, 超 1MB)", resp.StatusCode, r.Code)
	}
}

// TestAPIPath_NotFound GET /api/v1/push（失配 method）→ 404，不落 bark 兜底污染 msgs（CP3b 功能审 #15）。
// 验 Go 1.22 ServeMux：POST /api/v1/push 精确 pattern 对 GET 失配 → /api/ 子树兜底 404（不落 / bark 建 TargetUUID="api" 空消息）。
func TestAPIPath_NotFound(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/api/v1/push")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/v1/push: status=%d (want 404, 不落 bark 兜底)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("msgs polluted: %d (want 0, GET /api/v1/push 不该建消息)", len(msgs))
	}
}
// goroutine 内不用 apiPush（其内部 t.Fatal 不 goroutine-safe），内联 http + t.Errorf。
func TestAPIPush_Concurrent(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")

	const goroutines, perG = 5, 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perG; i++ {
				req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/push", strings.NewReader(`{"body":"c"}`))
				if err != nil {
					t.Errorf("NewRequest: %v", err)
					continue
				}
				req.Header.Set("Authorization", "Bearer "+key1)
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("push err: %v", err)
					continue
				}
				resp.Body.Close()
			}
		}()
	}
	close(start)
	wg.Wait()

	want := goroutines * perG
	msgs, err := bb.MessagesSince(0, want+10)
	if err != nil {
		t.Fatalf("MessagesSince: %v", err)
	}
	if len(msgs) != want {
		t.Fatalf("msgs count: got %d, want %d", len(msgs), want)
	}
	seen := make(map[uint64]bool, len(msgs))
	for i, m := range msgs {
		if seen[m.HLC] {
			t.Fatalf("dup HLC %d at %d", m.HLC, i)
		}
		seen[m.HLC] = true
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].HLC <= msgs[i-1].HLC {
			t.Fatalf("HLC not strictly monotonic at %d: %d <= %d", i, msgs[i].HLC, msgs[i-1].HLC)
		}
	}
}

// TestAPIPush_MediaIDs media_ids 多附件落库回查（漏测 #1：media 一等字段 JSON tag + 落库）。
func TestAPIPush_MediaIDs(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")
	resp, r := apiPush(t, ts.URL, key1, `{"body":"b","media_ids":["m1","m2","m3"]}`)
	defer resp.Body.Close()
	if r.Code != 200 {
		t.Fatalf("code=%d msg=%q", r.Code, r.Message)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 || len(msgs[0].MediaIDs) != 3 || msgs[0].MediaIDs[0] != "m1" {
		t.Errorf("media_ids 落库: %+v", msgs[0].MediaIDs)
	}
}

// TestAPIPush_MediaIDsSatisfiesRequired media_ids 单独满足必填（漏测 #10：title/body 空 + media_ids 非空 → 200）。
func TestAPIPush_MediaIDsSatisfiesRequired(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	key1 := registerFirstSet(t, ts, "dev1")
	resp, r := apiPush(t, ts.URL, key1, `{"media_ids":["x"]}`) // title/body 空，media_ids 单独
	defer resp.Body.Close()
	if r.Code != 200 {
		t.Errorf("media_ids 单独应满足必填: code=%d msg=%q", r.Code, r.Message)
	}
}

// TestAPIPush_RealPushkitFail pushkit stub 真 Send 失败路径（漏测 #4：ProjectID 非空 → Send 返 not implemented）。
// 非 failPusher mock，走 fanoutPush default→*pushkit.Client.Send 真分支。
func TestAPIPush_RealPushkitFail(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{ProjectID: "fake"}))
	key1 := registerFirstSet(t, ts, "dev1")
	resp, r := apiPush(t, ts.URL, key1, `{"body":"b","target_uuid":"dev1"}`)
	defer resp.Body.Close()
	// dev1 注册（tok-dev1 非空）→ fanoutPush default → pushkit.Send（ProjectID=fake）→ not implemented err
	if r.Code != 200 || !strings.Contains(r.Message, msgPushFailed) {
		t.Errorf("real pushkit fail: code=%d msg=%q (want 200 + push failed)", r.Code, r.Message)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Errorf("msgs: %d (want 1, 推失败不挡落库)", len(msgs))
	}
}

// TestBark_DeviceNotFound bark /unknown-key/... → device not found 留痕 + 200 + 落库（漏测 #5/#6）。
// 验 bark 接 ingest 后 device not found 行为（fanoutPush ErrNotFound 分支，bark 响应带 timestamp）。
func TestBark_DeviceNotFound(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Post(ts.URL+"/unknown-key/标题/内容", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var r barkResp // bark 响应带 timestamp
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if r.Code != 200 {
		t.Errorf("bark device not found: code=%d (want 200)", r.Code)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 || msgs[0].TargetUUID != "unknown-key" || msgs[0].Body != "内容" {
		t.Errorf("bark msg 落库: %+v", msgs)
	}
}

// TestAPIPush_SaveFail ingest 存失败 → 500（漏测 #3：ingest 语义核心分支，零覆盖）。
// errSaveStore：register 真（first-set key1），push 的 SaveMessage 注入 err。
func TestAPIPush_SaveFail(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "save.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	defer bb.Close()
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	ts := newPushServerWithStore(t, pushkit.New(pushkit.Config{}), errSaveStore{bb})
	key1 := registerFirstSet(t, ts, "dev1")
	resp, r := apiPush(t, ts.URL, key1, `{"body":"b"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || r.Code != 500 {
		t.Errorf("save fail: status=%d code=%d (want 500)", resp.StatusCode, r.Code)
	}
	if !strings.Contains(r.Message, msgSaveFailed) {
		t.Errorf("save fail msg: %q (want prefix %q)", r.Message, msgSaveFailed)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("msgs: %d (want 0, 存失败消息没落库)", len(msgs))
	}
}

// TestAPIPush_GetDeviceErr fanoutPush GetDevice 内部错（非 ErrNotFound）→ 推失败 200（漏测 #2）。
// errGetDeviceStore：SaveMessage 真（落库），GetDevice 注入 err。
func TestAPIPush_GetDeviceErr(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "getdev.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	defer bb.Close()
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	ts := newPushServerWithStore(t, pushkit.New(pushkit.Config{}), errGetDeviceStore{bb})
	key1 := registerFirstSet(t, ts, "dev1")
	resp, r := apiPush(t, ts.URL, key1, `{"body":"b","target_uuid":"dev1"}`)
	defer resp.Body.Close()
	if r.Code != 200 || !strings.Contains(r.Message, msgPushFailed) {
		t.Errorf("getdevice err: code=%d msg=%q (want 200 + push failed)", r.Code, r.Message)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Errorf("msgs: %d (want 1, 存成功落库)", len(msgs))
	}
}

// TestFanoutPush_NoTarget target_uuid 空 → fanoutPush no-target 分支（log + return nil，不调 Send）。
// CP3c 跨审 C P1：fanoutPush 五分支零直接覆盖，用计数 Pusher 验 Send 未调用（no-target 隐式覆盖不可靠）。
func TestFanoutPush_NoTarget(t *testing.T) {
	pk := &countingPusher{}
	ts, bb := newPushServer(t, pk)
	key1 := registerFirstSet(t, ts, "dev1") // first-set 关窗口 A（push 无 target_uuid 仍落库不推）

	resp, r := apiPush(t, ts.URL, key1, `{"body":"b"}`) // 无 target_uuid → no-target 分支
	defer resp.Body.Close()
	if r.Code != 200 {
		t.Errorf("no-target: code=%d msg=%q (want 200)", r.Code, r.Message)
	}
	if pk.calls != 0 {
		t.Errorf("no-target 不该调 Send: calls=%d (want 0)", pk.calls)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Errorf("no-target 应落库（消息已存，只是不推）: %d (want 1)", len(msgs))
	}
}

// TestFanoutPush_EmptyToken target_uuid 设备存在但 token 空 → fanoutPush empty-token 分支（留痕不 Send）。
// register 校验 token 非空，但 legacy /register 或未来 DELETE/ResetKeys 可能造空 token 设备——这条分支是防静默 success 假绿的留痕防线。
// 直注空 token 设备绕过 register 校验（构造分支触发条件）。
func TestFanoutPush_EmptyToken(t *testing.T) {
	pk := &countingPusher{}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	defer bb.Close()
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	// 直注空 token 设备（绕过 register 的 token 非空校验，构造 empty-token 分支触发条件）
	if err := bb.RegisterDevice(model.Device{UUID: "emptydev", Platform: "harmony", PushToken: ""}); err != nil {
		t.Fatalf("RegisterDevice emptydev: %v", err)
	}
	ts := newPushServerWithStore(t, pk, bb)
	key1 := registerFirstSet(t, ts, "firstset") // first-set key1（push 准入用，与 emptydev 无关）

	resp, r := apiPush(t, ts.URL, key1, `{"body":"b","target_uuid":"emptydev"}`)
	defer resp.Body.Close()
	if r.Code != 200 {
		t.Errorf("empty token: code=%d msg=%q (want 200, 消息落库不推)", r.Code, r.Message)
	}
	if pk.calls != 0 {
		t.Errorf("empty token 不该调 Send: calls=%d (want 0, 留痕不推)", pk.calls)
	}
}

// TestIngest_TimestampBackfilled ingest 后 fanoutPush 收到的 msg.TS>0（CP3c 跨审 D P1：msg.TS 跨层回填）。
// 验 push.go ingest 预填 TS（防 CP4 PushKit showBeginTime/归并 key 全 0）+ HLC 回填。
func TestIngest_TimestampBackfilled(t *testing.T) {
	pk := &countingPusher{}
	ts, _ := newPushServer(t, pk)
	key1 := registerFirstSet(t, ts, "dev1")

	resp, r := apiPush(t, ts.URL, key1, `{"body":"b","target_uuid":"dev1"}`)
	defer resp.Body.Close()
	if r.Code != 200 || pk.calls != 1 {
		t.Fatalf("push: code=%d msg=%q calls=%d (want 200, Send 调 1 次)", r.Code, r.Message, pk.calls)
	}
	if pk.lastMsg.TS == 0 {
		t.Errorf("msg.TS 应回填非 0: got 0 (CP4 PushKit showBeginTime 会全 0/全归并)")
	}
	if pk.lastMsg.HLC == 0 {
		t.Errorf("msg.HLC 应回填非 0: got 0")
	}
}
