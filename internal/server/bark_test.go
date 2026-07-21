// bark 兼容皮 parseBark 重写测试（CP3c）。
// 覆盖 bark.md §4.6 九缺口 + 2 段 bug 回归 + Ext 留底纪律 + category 归一优先级
// + CP3c 对抗审查补测（body limit / Ext 上限 / 空 content 拒 / 扫描器不污染 / iv drop / call 非触发 / group Trim / autocopy query）。
//
// 两层：
//   - parseBark 纯函数单测（构造 http.Request + segs → 验 Message 字段，快、不依赖 store）
//   - handleBark 集成测（httptest + 落库回查：URL 解码 / timestamp / 路径 404 / body limit / 不落库验证 / 端到端 sanity）
package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/pushkit"
)

// newBarkRequest 构造 bark 请求（method + url + 可选 body + contentType）。
// segs 由调用方传（模拟 handleBark 的 splitBarkPath 输出——单测绕过路由直接测 parseBark）。
func newBarkRequest(method, urlPath, body, contentType string) *http.Request {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(method, urlPath, bodyReader)
	} else {
		req = httptest.NewRequest(method, urlPath, nil)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// --- 缺口 #4 + #6：1/2/3/4 段路径分派 + 2 段 bug 回归 ---

// TestBark_Path2Segments_BodyOnly 2 段 /{key}/{body} → body=segs[1], title 空（修 CP1-temp bug #6）。
// CP1-temp `len(segs)>=3` 把 2 段 GET /{key}/body 当 JSON 解析（GET 无 body）→ title/body 全空。
func TestBark_Path2Segments_BodyOnly(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey", "", "")
	msg, empty, err := parseBark(req, []string{"mykey", "hello-body"})
	if err != nil {
		t.Fatalf("parseBark: %v", err)
	}
	if msg.Title != "" {
		t.Errorf("2 段 title 应空: got %q（2 段 bug：原把 body 当 title）", msg.Title)
	}
	if msg.Body != "hello-body" {
		t.Errorf("2 段 body: got %q, want hello-body", msg.Body)
	}
	if msg.TargetUUID != "mykey" {
		t.Errorf("TargetUUID: got %q, want mykey", msg.TargetUUID)
	}
	if empty {
		t.Errorf("2 段有 body 不该 empty")
	}
}

// TestBark_Path3Segments 3 段 /{key}/{title}/{body} → title=segs[1], body=segs[2]。
func TestBark_Path3Segments(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey", "", "")
	msg, _, _ := parseBark(req, []string{"mykey", "the-title", "the-body"})
	if msg.Title != "the-title" || msg.Body != "the-body" {
		t.Errorf("3 段: title=%q body=%q (want the-title/the-body)", msg.Title, msg.Body)
	}
}

// TestBark_Path4Segments_Subtitle 4 段 /{key}/{title}/{subtitle}/{body} → title/body + Ext[subtitle]。
// subtitle 无 Message 字段，进 Ext 留底（无映射目标类）。
func TestBark_Path4Segments_Subtitle(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey", "", "")
	msg, _, _ := parseBark(req, []string{"mykey", "t", "sub", "b"})
	if msg.Title != "t" || msg.Body != "b" {
		t.Errorf("4 段 title/body: %q/%q", msg.Title, msg.Body)
	}
	if msg.Ext["subtitle"] != "sub" {
		t.Errorf("4 段 subtitle 进 Ext: got %v, want Ext[subtitle]=sub", msg.Ext)
	}
}

// TestBark_PathOver4_NotFound >4 段 → 404（bark.md §1.1 上限 4 段）+ 不落库。
func TestBark_PathOver4_NotFound(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/key/a/b/c/d") // 5 段
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf(">4 段 status=%d (want 404)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf(">4 段不该落库: %d (want 0)", len(msgs))
	}
}

// --- 缺口 #1：query/form/JSON 合并优先级 ---

// TestBark_QueryMerge query 参数 → title/body（1 段路径 + query）。
func TestBark_QueryMerge(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey?title=qt&body=qb", "", "")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Title != "qt" || msg.Body != "qb" {
		t.Errorf("query 合并: title=%q body=%q (want qt/qb)", msg.Title, msg.Body)
	}
}

// TestBark_PathOverridesQuery path 段覆盖 query 同名字段（path 最高优先级，bark.md §1.2）。
func TestBark_PathOverridesQuery(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey?title=qt&body=qb", "", "")
	msg, _, _ := parseBark(req, []string{"mykey", "pt", "pb"})
	if msg.Title != "pt" {
		t.Errorf("path 覆盖 query title: got %q, want pt", msg.Title)
	}
	if msg.Body != "pb" {
		t.Errorf("path 覆盖 query body: got %q, want pb", msg.Body)
	}
}

// TestBark_FormMerge form-urlencoded body → title/body（V1 风格）。
func TestBark_FormMerge(t *testing.T) {
	form := url.Values{"title": {"ft"}, "body": {"fb"}}
	req := newBarkRequest(http.MethodPost, "/mykey", form.Encode(), "application/x-www-form-urlencoded")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Title != "ft" || msg.Body != "fb" {
		t.Errorf("form 合并: title=%q body=%q (want ft/fb)", msg.Title, msg.Body)
	}
}

// TestBark_JSONBody JSON V2 body → title/body（Content-Type 分派缺口 #7）。
func TestBark_JSONBody(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"title":"jt","body":"jb"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Title != "jt" || msg.Body != "jb" {
		t.Errorf("JSON body: title=%q body=%q (want jt/jb)", msg.Title, msg.Body)
	}
}

// --- category 归一（call > group > default）---

// TestBark_Category_Call call=="1" → category="call"。
func TestBark_Category_Call(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"call":"1","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "call" {
		t.Errorf("call: category=%q, want call", msg.Category)
	}
}

// TestBark_Category_Group group 非空 → category=group 值（SmsForwarder 配 "verify" → category=verify）。
func TestBark_Category_Group(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"group":"verify","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "verify" {
		t.Errorf("group: category=%q, want verify", msg.Category)
	}
}

// TestBark_Category_CallOverGroup call + group 都设 → category=call（优先级 call > group）。
func TestBark_Category_CallOverGroup(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"call":"1","group":"verify","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "call" {
		t.Errorf("call>group 优先级: category=%q, want call", msg.Category)
	}
}

// TestBark_CallWrongValue_DoesNotTrigger call 非 "1"（"yes"/"true"）不触发来电 → 走 group（CP3c 漏测补）。
// bark.md §1.3 call 必须 "1"，发送端误传 "yes" 不该当来电。
func TestBark_CallWrongValue_DoesNotTrigger(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"call":"yes","group":"verify","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "verify" {
		t.Errorf("call=yes 不触发来电，应走 group: category=%q, want verify", msg.Category)
	}
}

// TestBark_GroupWhitespace_Trimmed group 带空格 → TrimSpace（CP3c 漏测补）。
func TestBark_GroupWhitespace_Trimmed(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"group":"  verify  ","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "verify" {
		t.Errorf("group TrimSpace: category=%q, want verify", msg.Category)
	}
}

// TestBark_Category_NoHit_Empty 无 call/group → category 空（ingest 兜底 default）。
func TestBark_Category_NoHit_Empty(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Category != "" {
		t.Errorf("无命中: category=%q, want 空（ingest 兜底 default）", msg.Category)
	}
}

// TestBark_Category_NoHit_Default_Integrated 集成：无 call/group 落库 → category=default（ingest 兜底）。
func TestBark_Category_NoHit_Default_Integrated(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Post(ts.URL+"/mykey", "application/json", strings.NewReader(`{"body":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 || msgs[0].Category != "default" {
		t.Errorf("ingest 兜底 default: %+v", msgs)
	}
}

// --- 缺口 #8：字段名小写化 ---

// TestBark_Field_Lowercase JSON "Title"（驼峰）→ title（小写化后查表命中）。
func TestBark_Field_Lowercase(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"Title":"UP","Body":"low"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Title != "UP" || msg.Body != "low" {
		t.Errorf("小写化: title=%q body=%q (want UP/low)", msg.Title, msg.Body)
	}
}

// --- 缺口 #9：URL 解码（含斜杠 body）+ body 不合并 ---

// TestBark_URLDecode_BodyWithSlash body 含 %2F → URL 解码成 /（splitBarkPath 逐段解码）。
// bark-server §1.1 源码无 body 合并，body 含斜杠靠 %2F 编码（非 segs[2:] join）。
func TestBark_URLDecode_BodyWithSlash(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/mykey/t/c%2Fd") // 3 段 /key/title/c%2Fd → body="c/d"
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 {
		t.Fatalf("落库: %d (want 1)", len(msgs))
	}
	if msgs[0].Title != "t" || msgs[0].Body != "c/d" {
		t.Errorf("URL 解码 body 含斜杠: title=%q body=%q (want t / c/d)", msgs[0].Title, msgs[0].Body)
	}
}

// TestBark_URLDecode_Space path 段含 %20 → 解码成空格。
func TestBark_URLDecode_Space(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/mykey/hello%20world")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 1 || msgs[0].Body != "hello world" {
		t.Errorf("URL 解码空格: body=%q (want 'hello world')", msgs[0].Body)
	}
}

// --- 缺口 #5：response timestamp ---

// TestBark_ResponseTimestamp bark 响应带 timestamp（bark.md §1.5 CommonResp）。
func TestBark_ResponseTimestamp(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Post(ts.URL+"/mykey", "application/json", strings.NewReader(`{"body":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r barkResp
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Timestamp == 0 {
		t.Errorf("bark 响应缺 timestamp: %+v (want timestamp>0)", r)
	}
	if r.Code != 200 {
		t.Errorf("code=%d (want 200)", r.Code)
	}
}

// --- Ext 留底纪律（icon/copy + 未知字段兜底 + level + autoCopy query 小写）---

// TestBark_Ext_IconCopy icon/copy 无 Message 字段 → Ext 留底（无映射目标类）。
func TestBark_Ext_IconCopy(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"icon":"https://i","copy":"1234","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext["icon"] != "https://i" {
		t.Errorf("Ext[icon]: got %q, want https://i", msg.Ext["icon"])
	}
	if msg.Ext["copy"] != "1234" {
		t.Errorf("Ext[copy]: got %q, want 1234", msg.Ext["copy"])
	}
}

// TestBark_Ext_AutoCopyCamelCase JSON autoCopy（驼峰）→ Ext key 保留 "autoCopy"（小写化查表，Ext key 用原形）。
func TestBark_Ext_AutoCopyCamelCase(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"autoCopy":"1","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext["autoCopy"] != "1" {
		t.Errorf("Ext[autoCopy] 保留驼峰: got %v, want autoCopy=1", msg.Ext)
	}
}

// TestBark_Ext_AutoCopyQuery_Lowercase query ?autocopy=1（小写）→ Ext["autoCopy"]（驼峰保留）（CP3c 漏测补）。
// 验小写化后查表命中，Ext key 仍驼峰。
func TestBark_Ext_AutoCopyQuery_Lowercase(t *testing.T) {
	req := newBarkRequest(http.MethodGet, "/mykey?autocopy=1&body=b", "", "")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext["autoCopy"] != "1" {
		t.Errorf("query 小写 autocopy → Ext[autoCopy] 驼峰: got %v", msg.Ext)
	}
}

// TestBark_Ext_Unknown 未知字段 → Ext 兜底（扩展性三层之数据层：bark 升级加字段不丢）。
func TestBark_Ext_Unknown(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"newBarkField":"future","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext["newbarkfield"] != "future" {
		t.Errorf("未知字段 Ext 兜底: got %v, want newbarkfield=future", msg.Ext)
	}
}

// TestBark_Level_ToExt level 不映射 category（语义正交）→ Ext 留底 + category 空（ingest default）。
func TestBark_Level_ToExt(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"level":"critical","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext["level"] != "critical" {
		t.Errorf("level 进 Ext: got %v, want level=critical", msg.Ext)
	}
	if msg.Category != "" {
		t.Errorf("level 不映射 category: category=%q, want 空（ingest default）", msg.Category)
	}
}

// --- 一等字段 url 落库 ---

// TestBark_URL_Landing bark url → Message.URL（PushKit clickAction.data，CP4 用）。
func TestBark_URL_Landing(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"url":"https://example.com/x","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.URL != "https://example.com/x" {
		t.Errorf("url 落库: got %q", msg.URL)
	}
}

// --- 空 Ext → nil（json omitempty）+ 空 content 拒（跟原生 push 对称）---

// TestBark_EmptyContent_Rejected 空内容（title/body/subtitle 全空）→ empty=true（handleBark 400 拒，跟原生 push 必填对称）。
// bark-server 空 body 兜底是 APNs 必填的 hack，Hotify 无 APNs 不继承。
func TestBark_EmptyContent_Rejected(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{}`, "application/json")
	_, empty, err := parseBark(req, []string{"mykey"})
	if err != nil {
		t.Fatalf("parseBark: %v", err)
	}
	if !empty {
		t.Errorf("空 content 应 empty=true（→ handleBark 400 拒）")
	}
}

// TestBark_ExtNil_WhenEmpty 无 Ext 字段 → msg.Ext=nil（json omitempty 不序列化 ext:{}）。
func TestBark_ExtNil_WhenEmpty(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if msg.Ext != nil {
		t.Errorf("空 Ext 应 nil（json omitempty）: got %v", msg.Ext)
	}
}

// --- 真·丢字段（sound/badge/ciphertext/iv 不进 Message 也不进 Ext）---

// TestBark_DropFields sound/badge/ciphertext 等 → 不进 Message 也不进 Ext（端侧 profile / §9 §14）。
func TestBark_DropFields(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey",
		`{"sound":"bell","badge":"3","ciphertext":"enc","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	for _, dropped := range []string{"sound", "badge", "ciphertext"} {
		if _, ok := msg.Ext[dropped]; ok {
			t.Errorf("真丢字段 %s 不该进 Ext: %v", dropped, msg.Ext)
		}
	}
}

// TestBark_IVField_Dropped iv（端到端加密）→ drop 对齐 ciphertext（CP3c 漏测补，§1.6）。
func TestBark_IVField_Dropped(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{"iv":"enc-iv","ciphertext":"enc","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"mykey"})
	if _, ok := msg.Ext["iv"]; ok {
		t.Errorf("iv 应 drop 对齐 ciphertext: got Ext %v", msg.Ext)
	}
}

// --- 缺 device_key → 400 + 不落库 ---

// TestBark_MissingKey 路径空（根 /）→ 400 missing device_key + 不落库。
func TestBark_MissingKey(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 key: status=%d (want 400)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("缺 key 不该落库: %d (want 0)", len(msgs))
	}
}

// --- 坏 JSON → 400 + 不落库（纪律④：JSON decode err 处理）---

// TestBark_BadJSON 坏 JSON body（Content-Type application/json）→ parseBark 返 err。
func TestBark_BadJSON(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/mykey", `{not-json`, "application/json")
	_, _, err := parseBark(req, []string{"mykey"})
	if err == nil {
		t.Fatal("坏 JSON 应返 err（→ 400）")
	}
	if !strings.Contains(err.Error(), "bad json") {
		t.Errorf("坏 JSON err: %q (want 'bad json')", err.Error())
	}
}

// TestBark_BadJSON_DoesNotIngest 集成：坏 JSON → 400 + 不落库（CP3c 漏测补，验证 ingest 未触发）。
func TestBark_BadJSON_DoesNotIngest(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Post(ts.URL+"/mykey", "application/json", strings.NewReader(`{not-json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("坏 JSON status=%d (want 400)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("坏 JSON 不该落库: %d (want 0)", len(msgs))
	}
}

// TestBark_MultipartMerge multipart/form-data → title/body（V1 风格 form 分支）。
func TestBark_MultipartMerge(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("title", "mt")
	mw.WriteField("body", "mb")
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := newBarkRequest(http.MethodPost, "/mykey", buf.String(), mw.FormDataContentType())
	msg, _, err := parseBark(req, []string{"mykey"})
	if err != nil {
		t.Fatalf("parseBark multipart: %v", err)
	}
	if msg.Title != "mt" || msg.Body != "mb" {
		t.Errorf("multipart 合并: title=%q body=%q (want mt/mb)", msg.Title, msg.Body)
	}
}

// --- device_key 在 body 被忽略（path 段已取 key）---

// TestBark_DeviceKeyInBody_Dropped body 里 device_key 字段 → drop（path segs[0] 是唯一 key）。
func TestBark_DeviceKeyInBody_Dropped(t *testing.T) {
	req := newBarkRequest(http.MethodPost, "/pathkey", `{"device_key":"bodykey","body":"b"}`, "application/json")
	msg, _, _ := parseBark(req, []string{"pathkey"})
	if msg.TargetUUID != "pathkey" {
		t.Errorf("TargetUUID 应取 path: got %q, want pathkey", msg.TargetUUID)
	}
}

// --- CP3c 对抗审查 P1/P2 补测（body limit / Ext 上限 / 扫描器不污染 / key 过长）---

// TestBark_BodyLimit body 超 ~1MB → 400（MaxBytesReader 防 OOM，对称原生 push）。
func TestBark_BodyLimit(t *testing.T) {
	ts, _ := newPushServer(t, pushkit.New(pushkit.Config{}))
	huge := strings.Repeat("x", 1<<20+100) // 1MB + 100B
	body := `{"body":"` + huge + `"}`
	resp, err := http.Post(ts.URL+"/mykey", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("body limit: status=%d (want 400, 超 1MB)", resp.StatusCode)
	}
}

// TestBark_TooManyFields 字段数超上限 → 400（防 1MB JSON 塞数万 key 膨胀 bbolt value）。
func TestBark_TooManyFields(t *testing.T) {
	// 构造 > maxBarkFields(128) 个未知字段
	var fields []string
	for i := 0; i < maxBarkFields+10; i++ {
		fields = append(fields, `"kf`+itoa(i)+`":"v"`)
	}
	body := `{"body":"b",` + strings.Join(fields, ",") + `}`
	req := newBarkRequest(http.MethodPost, "/mykey", body, "application/json")
	_, _, err := parseBark(req, []string{"mykey"})
	if err == nil {
		t.Errorf("字段超上限应返 err（→ 400）")
	}
}

// itoa 简单 itoa（避免引入 strconv 到测试，保持依赖最小）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}

// TestBark_Favicon_NotIngest catch-all 扫描器（GET /favicon.ico）→ 空 content 400 拒 + 不落库（CP3c 跨层审 P2 #3）。
// 防扫描器（favicon.ico/.env/wp-admin）落 catch-all 建 TargetUUID="favicon.ico" 空消息污染历史。
func TestBark_Favicon_NotIngest(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("favicon.ico 空 content: status=%d (want 400)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("favicon.ico 不该落库: %d (want 0, 防扫描器污染历史)", len(msgs))
	}
}

// TestBark_KeyTooLong device_key 超 maxBarkKeyLen → 400（防 bbolt key 膨胀，CP3c 跨层审 P3）。
func TestBark_KeyTooLong(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	longKey := strings.Repeat("k", maxBarkKeyLen+10)
	resp, err := http.Get(ts.URL + "/" + longKey + "/body")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("key 过长: status=%d (want 400)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("key 过长不该落库: %d (want 0)", len(msgs))
	}
}

// TestBark_InternalPath_NotPollute POST /messages/abc（失配 method）→ 404 不落库（server.go /messages/ 子树）。
func TestBark_InternalPath_NotPollute(t *testing.T) {
	ts, bb := newPushServer(t, pushkit.New(pushkit.Config{}))
	resp, err := http.Post(ts.URL+"/messages/abc", "application/json", strings.NewReader(`{"body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /messages/abc 失配 method: status=%d (want 404)", resp.StatusCode)
	}
	msgs, _ := bb.MessagesSince(0, 10)
	if len(msgs) != 0 {
		t.Errorf("POST /messages/abc 不该落库: %d (want 0, /messages/ 子树 404)", len(msgs))
	}
}
