// harmony.go L0 单测（CP4）：mock 云函数（httptest）返各 Push Kit code，验 harmonySend 各分支。
// 覆盖：delivered(80000000) / dead token(80100000+80300007→ErrDeadToken) / system_error(500+其他code) /
// retry(502) 重试次数 + URL fallback / clickAction.data 字段(ts/category/url/media_ids) /
// URL sanitize(javascript: 拒) / 调试模式(CloudFunctionURLs 空跳过)。
package pushkit

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
)

// mockCloudFunc 起 mock 云函数，handler 处理响应 + 捕获每次收到的 body 到 received（验 clickAction.data 等）。
func mockCloudFunc(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]cloudFuncRequestBody) {
	t.Helper()
	received := make([]cloudFuncRequestBody, 0)
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body cloudFuncRequestBody
		_ = json.Unmarshal(raw, &body) // 捕获调用方构造的 notification（云函数透传契约）
		received = append(received, body)
		if handler != nil {
			handler(w, r)
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(ts.Close)
	return ts, &received
}

// replyCloudCode 模拟云函数透传 Push Kit 响应（HTTP 200 + body={code,msg}）。
func replyCloudCode(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "test"})
}

// testHarmonyDev 测试用鸿蒙设备（platform=harmony → Send 走 harmonySend）。
func testHarmonyDev() model.Device {
	return model.Device{UUID: "dev1", Platform: "harmony", PushToken: "tok-dev1"}
}

func TestHarmonySend_Delivered(t *testing.T) {
	ts, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	if err := client.Send(model.Message{Category: "call", Title: "t", Body: "b"}, testHarmonyDev()); err != nil {
		t.Errorf("delivered: got err=%v, want nil", err)
	}
}

func TestHarmonySend_DeadToken(t *testing.T) {
	for _, code := range []string{"80100000", "80300007"} {
		ts, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
			replyCloudCode(w, code)
		})
		client := New(Config{CloudFunctionURLs: []string{ts.URL}})
		err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
		if !errors.Is(err, ErrDeadToken) {
			t.Errorf("code=%s: got err=%v, want ErrDeadToken", code, err)
		}
	}
}

func TestHarmonySend_SystemError_NonDeadCode(t *testing.T) {
	// 非死码（鉴权 802xxxxx）→ system_error，保留 token（非 ErrDeadToken）
	ts, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80200001")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
	if err == nil || errors.Is(err, ErrDeadToken) {
		t.Errorf("system_error code: got err=%v, want non-nil non-ErrDeadToken", err)
	}
}

func TestHarmonySend_SystemError_HTTP500(t *testing.T) {
	ts, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500 → system_error（保留 token）
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
	if err == nil || errors.Is(err, ErrDeadToken) {
		t.Errorf("HTTP 500: got err=%v, want non-nil non-ErrDeadToken", err)
	}
}

func TestHarmonySend_RetryCount(t *testing.T) {
	// 502 → retry harmonyRetryLimit 次（用尽）→ exhausted err。interval=0 避免等 3s。
	origInterval := harmonyRetryInterval
	harmonyRetryInterval = 0
	t.Cleanup(func() { harmonyRetryInterval = origInterval })

	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502 → retry
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
	if err == nil {
		t.Error("all-502: want err (exhausted), got nil")
	}
	if got := len(*received); got != harmonyRetryLimit {
		t.Errorf("retry count: got %d POSTs, want %d (harmonyRetryLimit)", got, harmonyRetryLimit)
	}
}

func TestHarmonySend_FallbackToSecondURL(t *testing.T) {
	// 主 URL 全 502（retry 用尽）→ fallback 备 URL 返 80000000 → nil。interval=0 避免等。
	origInterval := harmonyRetryInterval
	harmonyRetryInterval = 0
	t.Cleanup(func() { harmonyRetryInterval = origInterval })

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	backup, backupHits := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{primary.URL, backup.URL}})
	if err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev()); err != nil {
		t.Errorf("fallback: got err=%v, want nil (backup delivered)", err)
	}
	if len(*backupHits) != 1 {
		t.Errorf("backup hits: %d, want 1 (fallback to backup)", len(*backupHits))
	}
}

func TestHarmonySend_ClickActionData(t *testing.T) {
	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	msg := model.Message{
		Category: "call", Title: "t", Body: "b", TS: 12345,
		URL:      "https://example.com/x",
		MediaIDs: []string{"m1", "m2"},
	}
	if err := client.Send(msg, testHarmonyDev()); err != nil {
		t.Fatal(err)
	}
	if len(*received) != 1 {
		t.Fatalf("received: %d, want 1", len(*received))
	}
	notification := (*received)[0].Notification
	if notification.Category != huaweiCategory {
		t.Errorf("category: %q, want %q", notification.Category, huaweiCategory)
	}
	if notification.ClickAction.ActionType != 0 {
		t.Errorf("actionType: %d, want 0（1 要 action/uri → 80100003）", notification.ClickAction.ActionType)
	}
	data := notification.ClickAction.Data
	if data["ts"] != "12345" || data["category"] != "call" || data["url"] != "https://example.com/x" {
		t.Errorf("clickAction.data ts/category/url: got %+v", data)
	}
	if data["media_ids"] != "m1,m2" {
		t.Errorf("media_ids: %q, want %q（数组逗号拼接，端侧 split）", data["media_ids"], "m1,m2")
	}
}

func TestHarmonySend_URLSanitizeRejectsJavascript(t *testing.T) {
	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	// javascript: 被 SanitizeActionURL 拒（TD-12）→ clickAction.data 不含 url
	if err := client.Send(model.Message{Category: "default", Body: "b", URL: "javascript:alert(1)"}, testHarmonyDev()); err != nil {
		t.Fatal(err)
	}
	data := (*received)[0].Notification.ClickAction.Data
	if url, ok := data["url"]; ok {
		t.Errorf("javascript: URL 应被 sanitize 拒，got url=%q", url)
	}
}

func TestHarmonySend_DisabledSkipsPush(t *testing.T) {
	// CloudFunctionURLs 空 → Send 在分派前静默跳过（不进 harmonySend，不 POST，返 nil）
	client := New(Config{CloudFunctionURLs: nil})
	if err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev()); err != nil {
		t.Errorf("disabled: got err=%v, want nil (skip)", err)
	}
}

// TestHarmonySend_NotifyIDStableAcrossRetry P1-1 地基保护：retry/fallback 全程 NotifyID 稳定不变
// （同 msg.HLC → 同 NotifyID → Push Kit 原生覆盖防重复通知）。at-least-once 竞态下重发不产重复通知。
func TestHarmonySend_NotifyIDStableAcrossRetry(t *testing.T) {
	origInterval := harmonyRetryInterval
	harmonyRetryInterval = 0 // 即时重试避免等 3s
	t.Cleanup(func() { harmonyRetryInterval = origInterval })

	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502 → retry harmonyRetryLimit 次
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	msg := model.Message{HLC: 0x123456789a, Category: "default", Body: "b"}
	expectedNotifyID := int(msg.HLC & 0x7fffffff)
	_ = client.Send(msg, testHarmonyDev())
	if len(*received) != harmonyRetryLimit {
		t.Fatalf("retry: %d POSTs, want %d", len(*received), harmonyRetryLimit)
	}
	for idx, req := range *received {
		if req.Notification.NotifyID != expectedNotifyID {
			t.Errorf("retry %d: NotifyID=%d, want %d (稳定不变→Push Kit 覆盖防重)", idx, req.Notification.NotifyID, expectedNotifyID)
		}
	}
}

// TestHarmonySend_HTTP401_400SystemError 401（AUTH_TOKEN 配错）/400（请求格式错）→ system_error（保留 token，非死）。
func TestHarmonySend_HTTP401_400SystemError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
		ts, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		client := New(Config{CloudFunctionURLs: []string{ts.URL}})
		err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
		if err == nil || errors.Is(err, ErrDeadToken) {
			t.Errorf("HTTP %d: got err=%v, want system_error (non-nil non-ErrDeadToken, 保留 token)", status, err)
		}
	}
}

// TestHarmonySend_SubscribeLabelPrefix SubscribeLabel 默认 true → title 加"订阅:"前缀（华为 SUBSCRIPTION 类目要求标题带订阅字样）。
func TestHarmonySend_SubscribeLabelPrefix(t *testing.T) {
	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}}) // SubscribeLabel 空→New 默认 "true"
	if err := client.Send(model.Message{Category: "default", Title: "短信", Body: "b"}, testHarmonyDev()); err != nil {
		t.Fatal(err)
	}
	if title := (*received)[0].Notification.Title; title != "订阅:短信" {
		t.Errorf("SubscribeLabel 前缀: title=%q, want %q", title, "订阅:短信")
	}
}

// TestHarmonySend_NoMediaIDsOmitted MediaIDs=nil → clickAction.data 不含 media_ids 键（omitempty 语义）。
func TestHarmonySend_NoMediaIDsOmitted(t *testing.T) {
	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	if err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev()); err != nil {
		t.Fatal(err)
	}
	data := (*received)[0].Notification.ClickAction.Data
	if _, ok := data["media_ids"]; ok {
		t.Errorf("MediaIDs=nil 不应塞 media_ids 键，got %+v", data)
	}
}

// TestHarmonySend_AllURLsExhausted 主+备都 502 → 全 fallback 用尽 → err（保留 token，非死）。
func TestHarmonySend_AllURLsExhausted(t *testing.T) {
	origInterval := harmonyRetryInterval
	harmonyRetryInterval = 0
	t.Cleanup(func() { harmonyRetryInterval = origInterval })

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(backup.Close)
	client := New(Config{CloudFunctionURLs: []string{primary.URL, backup.URL}})
	err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
	if err == nil || errors.Is(err, ErrDeadToken) {
		t.Errorf("all URLs exhausted: got err=%v, want non-nil non-ErrDeadToken (保留 token)", err)
	}
}

// TestHarmonySend_DeadOnFallback 主 URL 502 用尽 → fallback 备 URL 返 80300007 → ErrDeadToken。
func TestHarmonySend_DeadOnFallback(t *testing.T) {
	origInterval := harmonyRetryInterval
	harmonyRetryInterval = 0
	t.Cleanup(func() { harmonyRetryInterval = origInterval })

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	backup, _ := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80300007") // 备 URL 返死码
	})
	client := New(Config{CloudFunctionURLs: []string{primary.URL, backup.URL}})
	err := client.Send(model.Message{Category: "default", Body: "b"}, testHarmonyDev())
	if !errors.Is(err, ErrDeadToken) {
		t.Errorf("dead on fallback: got err=%v, want ErrDeadToken", err)
	}
}

// TestHarmonySend_ExtTSCollisionFiltered Ext["ts"] 不覆盖 server ts（P3-2 防碰撞）+ 其他 Ext 透传。
func TestHarmonySend_ExtTSCollisionFiltered(t *testing.T) {
	ts, received := mockCloudFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		replyCloudCode(w, "80000000")
	})
	client := New(Config{CloudFunctionURLs: []string{ts.URL}})
	msg := model.Message{Category: "default", Body: "b", TS: 999, Ext: map[string]string{"ts": "evil", "custom": "val"}}
	if err := client.Send(msg, testHarmonyDev()); err != nil {
		t.Fatal(err)
	}
	var dataObj map[string]string
	if err := json.Unmarshal([]byte((*received)[0].Data), &dataObj); err != nil {
		t.Fatal(err)
	}
	if dataObj["ts"] != "999" {
		t.Errorf("Ext ts 碰撞: data.ts=%q, want 999 (server ts 不被 Ext 覆盖)", dataObj["ts"])
	}
	if dataObj["custom"] != "val" {
		t.Errorf("Ext 透传: data.custom=%q, want val", dataObj["custom"])
	}
}
