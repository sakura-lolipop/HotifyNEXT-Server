// HTTP 端到端 smoke（CP1-6）：register → bark push → history 拉得到 + bbolt 文件落盘。
// 验骨架装配起得来 + 路由通 + 持久化（不验协议正确性——那是 CP3/CP4 的事）。
package server

import (
	"encoding/json"
	"io"
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
func newSmokeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "smoke.db")
	bb, err := store.NewBBolt(dbPath)
	if err != nil {
		t.Fatalf("NewBBolt: %v", err)
	}
	t.Cleanup(func() { _ = bb.Close() })
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
	return ts, dbPath
}

// TestSmoke_RegisterPushHistory 验 register→bark push→history 全链路通 + bbolt 文件落盘。
func TestSmoke_RegisterPushHistory(t *testing.T) {
	ts, dbPath := newSmokeServer(t)

	// 1) register（旧字段名 device_key，CP1 临时映射 uuid）
	resp, err := http.Post(ts.URL+"/register", "application/json",
		strings.NewReader(`{"device_key":"dev1","push_token":"tok1","name":"phone"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2) bark push（路径式 /{key}/标题/内容）——pushkit stub 会 push 失败，但消息已落库
	resp, err = http.Post(ts.URL+"/dev1/标题/内容测试", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// push 失败不挡 200（返 "saved but push failed" 或 "success"）
	if !strings.Contains(string(body), "saved") && !strings.Contains(string(body), "success") {
		t.Fatalf("push resp unexpected: %s", body)
	}

	// 3) history 拉得到（CP1 临时全局取，忽略 {key}）
	resp, err = http.Get(ts.URL + "/messages/dev1")
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

// TestSmoke_ReadSetDeprecated 验 §14 砍 read set → 返 410 Gone（防旧 App 404 噪声）。
func TestSmoke_ReadSetDeprecated(t *testing.T) {
	ts, _ := newSmokeServer(t)
	resp, err := http.Get(ts.URL + "/read/dev1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("read set status: got %d, want 410 Gone", resp.StatusCode)
	}
}
