// HTTP 端到端 smoke（CP1-6）：register → bark push → history 拉得到 + bbolt 文件落盘。
// 验骨架装配起得来 + 路由通 + 持久化（不验协议正确性——那是 CP3/CP4 的事）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/config"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/store"
)

// newSmokeServer 起 httptest server（真 bbolt 临时文件 + 空 pushkit 只存不推）。
// 返 *store.BBolt 让测试能 GetDevice/GetKeys 回查或注 corrupt 数据（CP2 审查后补）。
func newSmokeServer(t *testing.T) (*httptest.Server, string, *store.BBolt) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "smoke.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	t.Cleanup(func() { _ = bb.Close() })
	// 启动 EnsureKey2（对齐 main.go：server 启动生成 key2，register 响应回显用）
	if _, err := bb.EnsureKey2(); err != nil {
		t.Fatalf("EnsureKey2: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Store: config.StoreConfig{
			Type:     "bbolt",
			Path:     dbPath,
			BlobDir:  filepath.Join(dir, "blobs"),
			MaxBytes: 1 << 30,
		},
	}
	srv := New(cfg, bb, pushkit.New(pushkit.Config{}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, dbPath, bb
}

// TestSmoke_RegisterPushHistory 验 原生 register(first-set key1)→bark push→history(带 key1) 全链路通 + bbolt 落盘。
func TestSmoke_RegisterPushHistory(t *testing.T) {
	ts, dbPath, _ := newSmokeServer(t)

	// 1) 原生 register（首设备不带 key1 → first-set + 下发 key1/key2）
	resp, err := http.Post(ts.URL+"/api/v1/register", "application/json",
		strings.NewReader(`{"uuid":"dev1","platform":"harmony","push_token":"tok1","name":"phone"}`))
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
		t.Fatalf("register: code=%d key1=%q (want 200 + first-set key1)", reg.Code, reg.Key1)
	}

	// 2) bark push（路径式 /{key}/标题/内容）——pushkit stub push 失败，但消息落库
	// CP3a：bark 响应带 timestamp（bark.md §1.5，writeBark），验 code/message/timestamp 三字段。
	resp, err = http.Post(ts.URL+"/dev1/标题/内容测试", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pushResp struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pushResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pushResp.Code != 200 {
		t.Fatalf("push code=%d msg=%q (want 200)", pushResp.Code, pushResp.Message)
	}
	if !strings.Contains(pushResp.Message, "saved") && pushResp.Message != "success" {
		t.Errorf("push msg unexpected: %q", pushResp.Message)
	}
	if pushResp.Timestamp == 0 {
		t.Error("bark resp missing timestamp (bark.md §1.5, writeBark 应填)")
	}

	// 3) history 拉得到（/messages 套了 requireKey1，带 key1 头）
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/messages/dev1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+reg.Key1)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var hist struct {
		Code     int `json:"code"`
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hist.Code != 200 || len(hist.Messages) == 0 {
		t.Fatalf("history: code=%d msgs=%d", hist.Code, len(hist.Messages))
	}
	if hist.Messages[0].Body != "内容测试" {
		t.Errorf("msg body: got %q, want '内容测试'", hist.Messages[0].Body)
	}

	// 4) bbolt 文件落盘（size > 0）
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("db file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Error("db file empty (bbolt not persisted)")
	}
}

// TestSmoke_ReadSetDeprecated 验 §14 砍 read set → 410 Gone（防旧 App 落 bark 兜底灌空消息）。
// TD-4：/read 路由保留返 410（合一 handler，不删），给旧 App 明确废弃信号。
func TestSmoke_ReadSetDeprecated(t *testing.T) {
	ts, _, _ := newSmokeServer(t)
	resp, err := http.Get(ts.URL + "/read/dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("read set status: got %d, want 410 Gone", resp.StatusCode)
	}
}
