# HotifyServer

Hotify 的自建后端（**Go**），对标 bark-server。**开源/可分发**——用户自托管要部署它。

## 三层
- **入口**：bark 协议（`POST /<device_key> {title,body}` 或 `/<device_key>/<title>/<body>`）。SmsForwarder 选「Bark」通道直用，零摩擦。
- **鉴权**：device_key(UUID) 本身，**无额外 token**（bark 模型；UUID 即鉴权）。
- **推送**：华为 Push Kit v3，**云函数中转**（私钥锁云函数 hotifypushkit.netlify.app，Server POST 云函数；云函数内 `/v3/{project_id}/messages:send` + header `push-type:0`，成功码 `80000000`）。

## 职责
- `device_key → push_token` 映射（多设备：一人多 device_key，可选 group key 扇出）。
- `/register`：客户端上报 `{device_key, push_token}`（开放注册，同 bark）。
- 消息历史存储（供客户端历史拉取）。
- 阅读进度同步（per-user 已读态；客户端上报 + 拉取）。
- Push Kit 转发（云函数中转 + 逐 token 单推 + 死 token 清理 + 断线回补）。

## 状态
Go 实现**待开始**。架构/决策/时序详见 `../hotify/task.md`「后端架构（单后端，抛弃 Bridge 版）」。
legacy Python 桥（`../hotify/gotify_pushkit_bridge.py`）过渡期保留、自用不断；本 Server 就绪验证后下线。

## 命名/定位
"小鸿蒙"叙事：HotifyServer = 自主内核（Go 自建）+ 生态兼容面（bark 入口），脱离 Gotify 的纯血后端。对标华为从安卓到纯血 HarmonyOS 的蜕变。
