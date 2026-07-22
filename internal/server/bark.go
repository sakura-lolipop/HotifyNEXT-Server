// bark 协议入口（兼容皮）：接收发送端（SmsForwarder「Bark」通道等）消息。
// bark-server 兼容（设备 key 在路径 = 路由，无鉴权——bark.md §1.4 源码确认）。
//
//	POST/GET /{key}                            仅 key（body 走 query/form/JSON）
//	POST/GET /{key}/{body}                     2 段（key + body）
//	POST/GET /{key}/{title}/{body}             3 段（key + title + body）
//	POST/GET /{key}/{title}/{subtitle}/{body}  4 段（key + title + subtitle + body）
//	>4 段 → 404（bark.md §1.1 路径式上限 4 段，fiber 路由未注册该模式 → push_test.go:191-197 实测）
//
// 成功返 bark 风格 {code:200, message:"success", timestamp}（bark.md §1.5 CommonResp，writeBark 带 timestamp）。
//
// 主从原则（ARCHITECTURE §一句话定位）：原生 /api/v1/push = 主（category/media 一等字段，CP3b 实装）；
// bark /{key} = 从（降级入口，尽力映射）。CP3b：存推接共享 ingest；CP3c：parseBark 重写九缺口 + 字段归宿表。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/model"
	"github.com/sakura-lolipop/HotifyNEXT-Server/internal/util"
)

// bark 协议层防护常量（CP3c 对抗审查 P1/P3，写开放 design 已接受可见 DoS，这些是实现层兜底）。
const (
	maxBarkKeyLen = 128 // device_key 长度上限（防 bbolt key 膨胀；UUIDv4=36，128 远超够）
	maxBarkFields = 128 // params 字段数上限（防 1MB JSON 塞数万 key 膨胀 bbolt value；bark 字段全集~25，128 远超够）
)

// barkFieldRule bark 字段归宿规则（字段归宿声明式表 = 扩展性三层之"映射层"，bark 升级好改）。
// bark 加字段 → 改 barkFieldRules 一行；或不动（未知字段自动 Ext 兜底）。
type barkFieldRule struct {
	target string // "title"/"body"/"url"（一等 Message 字段）/ "ext"（留底）/ "drop"（真丢）
	extKey string // target=="ext" 时的 Ext key（空 = 用 bark 字段名；autoCopy 等驼峰保留原形）
}

// barkFieldRules bark 字段归宿表（bark.md §1.3 字段全集 + §2 映射 + Ext 留底纪律三类）。
//
// 三类（Ext 留底纪律，plan「扩展性三层」+ 防"变垃圾桶"+ 防"双重存储"）：
//   - 一等字段（强类型 Message）：title/body/url（Message 有字段，直映）
//   - Ext 留底（无 Message 字段 / 无干净映射目标，保守留底备查）
//   - 真·丢（端侧 profile / App 全局 / §9 §14 取舍，每条存无意义或不兼容）
//
// category 三兄弟（call/group/level）显式处理（buildBarkMessage 归一优先级代码），不在此表：
//   - call=="1" → Category="call"；group 非空 → Category=group 值（bark.md §4.4，显式优先级 call > group > default）
//   - level → Ext 留底（iOS 中断级别 critical/active/timeSensitive/passive vs 业务 category 语义正交，
//     硬映射是垃圾映射；留 Ext 待未来 Message.Level alert 字段。bark.md §4.4 标"近似"非 1:1，接受不靠硬映射补）
//   - call/group 在表里 {"drop"}（遍历 params 时不进 Ext；归一代码单独读 params["call"]/params["group"]）
//
// 字段名小写化（bark.md §1.3：V1/V2 解析所有 key 转小写）：表 key 全小写；autoCopy → "autocopy"，
// Ext key 用 rule.extKey="autoCopy" 保留 bark 原驼峰（鸿蒙未来读 Ext 按 bark 字段名查）。
var barkFieldRules = map[string]barkFieldRule{
	// 一等字段（强类型 Message）
	"title": {"title", ""},
	"body":  {"body", ""},
	"url":   {"url", ""}, // → Message.URL（CP3b 加；PushKit clickAction.data，CP4 用）

	// Ext 留底（无 Message 字段 / 无干净映射目标，保守留底）
	"subtitle": {"ext", ""},         // Message 无 Subtitle 字段（bark.md §2 ⚠️ 待验）；4 段路径 + JSON 都进 Ext
	"image":    {"ext", ""},         // ⚠️ bark.md §2 字段名待验 + media 拉取/mini 存储超 CP3c（Phase 2 media 端点）；留 Ext 待 Phase 2
	"icon":     {"ext", ""},         // 鸿蒙 N/A（§6 iOS iMessage 专属），留底备查
	"copy":     {"ext", ""},         // 鸿蒙无后台剪贴板写入（§6），留底
	"autocopy": {"ext", "autoCopy"}, // bark 字段 autoCopy（驼峰），Ext key 保留原形；鸿蒙 N/A 同 copy
	"level":    {"ext", ""},         // iOS 中断级别，不映射 category（语义正交），留 Ext 待 alert 字段

	// category 归一源（显式处理，遍历时 drop 不进 Ext；buildBarkMessage 读 params 做归一）
	"call":  {"drop", ""},
	"group": {"drop", ""},

	// 真·丢（端侧 profile / App 全局 / §9 §14 取舍）
	"sound":        {"drop", ""}, // 端侧呈现 profile（须 App 预置 rawfile，§6），不进 Message 一等字段
	"badge":        {"drop", ""}, // App 图标全局状态，每条存无意义
	"volume":       {"drop", ""}, // critical 铃声音量，端侧 profile
	"ciphertext":   {"drop", ""}, // §9 全程明文，不兼容 bark 端到端加密
	"iv":           {"drop", ""}, // §1.6 端到端加密 iv，对齐 ciphertext drop（§9 明文不兼容）
	"isarchive":    {"drop", ""}, // §14 Hotify 历史全存，字段冗余
	"ttl":          {"drop", ""}, // 同 isArchive（§14 全存，TTL 无意义）
	"action":       {"drop", ""}, // PushKit clickAction 必填，无法"无动作"（bark.md §3 ⚠️ 待验）
	"delete":       {"drop", ""}, // CP3 不做撤回（留口，bark.md §2）
	"id":           {"drop", ""}, // Hotify 用 HLC（§7），bark id 冗余
	"device_keys":  {"drop", ""}, // 批量推送 V2 专属（route_push.go:111-125），低优先级 CP3 不做
	"device_key":   {"drop", ""}, // path 段已取 key（segs[0]），body/query 里的忽略避免重复
	"device_token": {"drop", ""}, // server 内部（注册时存），非协议字段
	"ext_params":   {"drop", ""}, // bark APNs 透传 map，Hotify 无 APNs（drop；未来鸿蒙 extraData 再议）
}

// handleBark bark 兼容皮入口（CP3a 搬进 server；CP3b 存推接共享 ingest；CP3c parseBark 重写九缺口 + 字段归宿表）。
// key 当 uuid 路由（无鉴权，bark.md §1.4 源码确认）→ msg.TargetUUID。
//
// 九缺口补齐（bark.md §4.6）：①1/2/3/4 段路径分派（修 2 段 bug）②query/form/JSON 合并（path 最高）
// ③字段全集（按表解析）④category 归一（call>group>default）⑤response timestamp（writeBark CP3a 已支持）
// ⑥2 段 bug 修 ⑦Content-Type 分派（JSON V2 vs form/query V1）⑧字段名小写化（collectBarkParams）⑨URL 解码（splitBarkPath）。
//
// 实现层防护（CP3c 对抗审查 P1/P2，写开放 design 已接受可见 DoS，这些是兜底）：
//   - body limit 1MB（MaxBytesReader，对称原生 push.go:85，防 OOM）
//   - device_key 长度上限（防 bbolt key 膨胀）
//   - params 字段数上限（防 1MB JSON 塞数万 key 膨胀 bbolt value）
//   - 空 content 拒 400（title/body/subtitle 全空；跟原生 push 必填对称——bark-server 空 body 兜底是 APNs 必填的
//     hack，Hotify 无 APNs 不继承；扫描器/空请求无内容全拒，不污染历史）
func (s *Server) handleBark(w http.ResponseWriter, r *http.Request) {
	// body limit ~1MB（对称原生 push，防大 body OOM；bark 写开放下 MaxBytesReader 是零成本实现层防护）
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// 用 EscapedPath()（保留 %2F 等编码）而非 URL.Path（Go net/http 已解码 %2F→/ 会把 body 含斜杠拆成多段）。
	// splitBarkPath 切 EscapedPath 后逐段 QueryUnescape 还原：/key/t/c%2Fd → ["key","t","c/d"]（body 含斜杠）。
	segs := splitBarkPath(r.URL.EscapedPath())
	if len(segs) == 0 {
		writeBark(w, http.StatusBadRequest, "missing device_key")
		return
	}
	if len(segs) > 4 { // bark.md §1.1 路径式上限 4 段，>4 fiber 未注册 → 404
		writeBark(w, http.StatusNotFound, "too many path segments (max 4)")
		return
	}
	if len(segs[0]) > maxBarkKeyLen { // 防 bbolt key 膨胀（UUIDv4=36，128 远超够；超长 key 是扫描器/异常输入）
		writeBark(w, http.StatusBadRequest, "device_key too long")
		return
	}

	msg, empty, err := parseBark(r, segs)
	if err != nil {
		writeBark(w, http.StatusBadRequest, err.Error())
		return
	}
	if empty { // 空 content 拒（跟原生 push 必填对称；不 ingest，扫描器/空请求不污染历史）
		writeBark(w, http.StatusBadRequest, "empty message: title/body/subtitle required")
		return
	}

	hlc, err := s.ingest(msg)
	writeIngestResult(w, hlc, err, true) // bark（带 timestamp）
}

// splitBarkPath 切 path 段 + URL 解码每段（bark.md §4.1 #9 url.QueryUnescape）。
// 解码失败保留原段（保守，bark-server §4.1 无明确失败处理语义）。
// 解码在 Split 后逐段做（每段独立 param），编码的 %2F 解码成 / 但已在段内不重 Split——body 含斜杠靠 %2F。
func splitBarkPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	rawSegs := strings.Split(trimmed, "/")
	segs := make([]string, len(rawSegs))
	for idx, raw := range rawSegs {
		if decoded, err := url.QueryUnescape(raw); err == nil {
			segs[idx] = decoded
		} else {
			segs[idx] = raw // 解码失败保留原值（保守）
		}
	}
	return segs
}

// parseBark 解析 bark 请求（路径式 + JSON/query/form 合并）→ model.Message + 空 content 标志。
// key（segs[0]）→ msg.TargetUUID（无鉴权 §1.4）。
// empty=true 表示 title/body 全空（无 Message 内容）—— 调用方（handleBark）据此拒 400 不 ingest。
//
// subtitle 不参与 empty 判定（CP3c 跨审 D P1 修）：subtitle 进 Ext 不是 Message 内容（model 无 Subtitle 字段），
// 旧判定含 subtitle 导致 POST /key {"subtitle":"x"} 落库 Title="" Body="" Ext[subtitle]="x" 的空骨架，扫描器绕过空内容拒绝灌历史。
// 跟原生 push 必填 title/body 对称（Hotify 无 APNs，bark-server 副标题兜底是 APNs hack 不继承）。
func parseBark(r *http.Request, segs []string) (msg model.Message, empty bool, err error) {
	params, err := collectBarkParams(r, segs)
	if err != nil {
		return model.Message{}, false, err
	}
	msg = buildBarkMessage(segs[0], params)
	empty = msg.Title == "" && msg.Body == "" // subtitle 进 Ext 不算 Message 内容
	return msg, empty, nil
}

// collectBarkParams 合并 JSON body + query + form/multipart + path 段 → 统一 map（key 全小写）。
// 优先级（bark.md §1.2，Hotify 简化统一 V1/V2）：path（最高）> form/multipart > query > JSON body。
// path 最高（bark.md §1.2 注释 "highest priority"）：路径段 title/body/subtitle 覆盖 query/form/JSON 同名字段。
// Hotify 兼容皮是"从"（主从原则），不必逐级复刻 bark V1（query<form<multipart<path）vs V2（body<query<path）的细微差异——
// 统一 path > form > query > body 够用且好审。
func collectBarkParams(r *http.Request, segs []string) (map[string]string, error) {
	params := map[string]string{}
	jsonBody := isJSONContentType(r) // 提一次避免重复 parse header

	// 1. JSON body（Content-Type application/json；V2 风格）。EOF 空 body 合法（GET / 路径式无 body）。
	if jsonBody {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("bad json: %w", err)
		}
		for key, val := range body {
			params[strings.ToLower(key)] = anyToString(val)
		}
	}

	// 2. query args（GET 参数 + POST ?xxx）
	for key, vs := range r.URL.Query() {
		if len(vs) > 0 {
			params[strings.ToLower(key)] = vs[0]
		}
	}

	// 3. form（urlencoded）+ multipart（V1 风格，仅非 JSON 时解析——JSON body 已 consumed 不能再读）。
	// ParseForm/ParseMultipartForm err 留痕不挡（malformed urlencoded 罕见；兼容皮尽力，query/path 仍收不阻断主路径）。
	if !jsonBody {
		if err := r.ParseForm(); err != nil { // urlencoded body
			log.Printf("[bark] parse urlencoded form err: %v", err)
		} else {
			for key, vs := range r.PostForm {
				if len(vs) > 0 {
					params[strings.ToLower(key)] = vs[0]
				}
			}
		}
		if isMultipartContent(r) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				log.Printf("[bark] parse multipart form err: %v", err)
			} else if r.MultipartForm != nil {
				for key, vs := range r.MultipartForm.Value {
					if len(vs) > 0 {
						params[strings.ToLower(key)] = vs[0]
					}
				}
			}
		}
	}

	// 4. path 段（最高优先级，覆盖一切）
	applyBarkPathSegments(params, segs)

	// 字段数上限（防 1MB JSON 塞数万 key 膨胀 bbolt value——bark 字段全集~25，128 远超够，攻击才触发）
	if len(params) > maxBarkFields {
		return nil, fmt.Errorf("too many fields (max %d)", maxBarkFields)
	}

	return params, nil
}

// applyBarkPathSegments 按 bark.md §1.1 段数分派 path 字段（覆盖 params 同名字段——path 最高优先级）。
//   - 1 段 /{key}：仅 key，不加 path 字段（body 走 query/form/JSON）
//   - 2 段 /{key}/{body}：body=segs[1]（修 CP1-temp bug #6：原 len>=3 把 2 段 body 当 title）
//   - 3 段 /{key}/{title}/{body}：title=segs[1], body=segs[2]
//   - 4 段 /{key}/{title}/{subtitle}/{body}：title/subtitle/body（subtitle 进 Ext，无 Message.Subtitle 字段）
//
// >4 段：handleBark 已 404（不到这）。
// body 含斜杠用 URL 编码 %2F（splitBarkPath 解码还原），不做 segs[2:] Join——bark-server §1.1 源码无合并逻辑
// （bark.md §4.1 "多段 body 合并 segs[2:]" 是 CP1-temp bark.go:53 的误解，源码无此特性）。
func applyBarkPathSegments(params map[string]string, segs []string) {
	switch len(segs) {
	case 2:
		params["body"] = segs[1]
	case 3:
		params["title"] = segs[1]
		params["body"] = segs[2]
	case 4:
		params["title"] = segs[1]
		params["subtitle"] = segs[2]
		params["body"] = segs[3]
	}
}

// buildBarkMessage 按字段归宿表 + category 归一构造 Message（key → TargetUUID）。
// category 空（无 call/group 命中）由 ingest 兜底 default（buildBarkMessage 不填 default，留给 ingest 单点兜底）。
// 空消息兜底不在此做——空 content（title/body/subtitle 全空）由 parseBark 判 empty → handleBark 400 拒（跟原生 push 对称，
// bark-server 空 body 兜底是 APNs hack，Hotify 无 APNs 不继承）。
func buildBarkMessage(key string, params map[string]string) model.Message {
	msg := model.Message{
		TargetUUID: key,
		Ext:        map[string]string{}, // 预建，按需填；空了尾部 nil 化（json omitempty）
	}

	// 字段归宿表分派（已知字段按 rule，未知字段 Ext 兜底）
	for barkField, val := range params {
		if val == "" {
			continue
		}
		rule, known := barkFieldRules[barkField]
		if !known {
			// 未知字段 → Ext 兜底（扩展性三层之"数据层"：bark 升级加字段不丢，鸿蒙未来读 Ext）
			msg.Ext[barkField] = val
			continue
		}
		switch rule.target {
		case "title":
			msg.Title = val
		case "body":
			msg.Body = val
		case "url":
			// TD-12：收 url 进 msg.URL 前 sanitize（javascript:/file: 等拒→空→不存；harmony clickAction.data 再 sanitize 是双保险）
			if safeURL := util.SanitizeActionURL(val); safeURL != "" {
				msg.URL = safeURL
			}
		case "ext":
			extKey := rule.extKey
			if extKey == "" {
				extKey = barkField
			}
			msg.Ext[extKey] = val
		case "drop":
			// 真·丢（端侧 profile / §9 §14 取舍）
		}
	}

	// category 归一（显式优先级，不塞表——业务规则塞表反而不清）：call > group > default（空由 ingest 兜底）。
	// call=="1" 是 bark 来电语义（铃声持续 30s）；group 是 SmsForwarder 用户配的分类标签（"verify"/"sms"）→ 挪用 category。
	switch {
	case params["call"] == "1":
		msg.Category = "call"
	case strings.TrimSpace(params["group"]) != "":
		msg.Category = strings.TrimSpace(params["group"])
	}

	// Ext 空 → nil（json `ext,omitempty`：空 map 仍序列化成 {}，nil 才 omit；端侧契约干净）
	if len(msg.Ext) == 0 {
		msg.Ext = nil
	}

	return msg
}

// anyToString JSON body 字段值（any）→ string（收进 params 前；key 小写化在 collectBarkParams 做）。
// 标量直转；array/object/nil → ""（复杂值 device_keys/ext_params 按规则 drop，不进 string params）。
func anyToString(val any) string {
	switch value := val.(type) {
	case string:
		return value
	case float64: // JSON number → float64（encoding/json 标准行为）
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	case nil:
		return ""
	default:
		return "" // array/object 丢弃（device_keys 批量 / ext_params map 按规则 drop，标量 params 不收）
	}
}

// isJSONContentType Content-Type 是否 application/json（V2 分派，bark.md §1.1）。
func isJSONContentType(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json")
}

// isMultipartContent Content-Type 是否 multipart/form-data（V1 form 分支）。
func isMultipartContent(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/")
}
