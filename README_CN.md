# Kiro-Go（增强版）

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将 Kiro 账号转换为 OpenAI / Anthropic 兼容的 API 服务。

[English](README.md) | 中文 | [Tiếng Việt](README_VI.md)

> 这是官方 Kiro-Go 的**增强分支**。在原版基础上增加了细粒度的 API Key 限流、
> 共享代理池 + 故障转移、模型重映射、自助用量面板，以及一整套抗 DoS/DDoS 的加固能力。
> 与原版的差异见下方 [相比原版新增了什么](#相比原版新增了什么)。

---

## 这个项目是做什么的

Kiro-Go 是一个反向代理，把一组 Kiro 账号暴露成 **OpenAI 兼容**和 **Anthropic 兼容**的 API 端点。
它管理账号池、把进来的 Claude/OpenAI 请求翻译成 Kiro 上游的 AWS 格式、把响应流式返回，
并提供一个 Web 管理面板。

请求流程：

```
客户端 → Handler.ServeHTTP（路由）→ 鉴权 → 翻译器 → 账号池选账号
       → CallKiroAPI 从 AWS 流式读取 → 翻译器把事件映射回去 → 客户端
```

纯 Go 标准库 `net/http`，无框架，仅一个依赖（`github.com/google/uuid`）。

---

## 相比原版新增了什么

| 能力 | 原版 Kiro-Go | 本增强版 |
|------|--------------|----------|
| 按 Key 限流（RPM / 并发 IP 上限 / IP 白名单 / TPM 展示） | ❌ | ✅ |
| Key 绑定账号（把某个 Key 固定到一组账号） | ❌ | ✅ |
| Key 终身累计计数（重置周期用量后仍保留总量） | ❌ | ✅ |
| 批量创建 / 删除 / 导出 API Key | ❌ | ✅ |
| 全局强制模型（Force Model）——重映射客户端请求的模型名 | ❌ | ✅ |
| 按 Key 指定模型 | ❌ | ✅ |
| 身份模型（Identity Model）——让助手自报为指定模型名 | ❌ | ✅ |
| 共享代理池 + 代理级故障转移（健康状态持久化） | ❌ | ✅ |
| 强制走代理开关（Require-proxy，防止泄露服务器真实 IP） | ❌ | ✅ |
| 抗 DoS/DDoS 加固（全局并发、按 IP RPM、按 Key 在途上限） | ❌ | ✅ |
| 自助用量面板 `/usage`（终端用户凭自己的 Key 查看） | ❌ | ✅ |
| 管理面板暴力破解限流（`/admin/api/*`） | ❌ | ✅ |
| 友好的限流提示消息（在对话里回一句而不是硬报错） | ❌ | ✅ |
| 面板 API Key 列表 5 秒实时自动刷新 | ❌ | ✅ |

> 版本变更日志见 `version.json`。安全审计结论见 `SECURITY_AUDIT.md`，
> 抗 DoS 部署指南见 `deploy/HARDENING.md`。

### 基础能力（沿袭原版）

- Anthropic `/v1/messages` 与 OpenAI `/v1/chat/completions`、`/v1/responses`
- 多账号池 + 加权轮询负载均衡
- Token 自动刷新、SSE 流式输出、Web 管理面板
- 多种登录方式：AWS Builder ID、IAM Identity Center（企业 SSO）、
  Kiro Hosted SSO（含 Microsoft Entra 等外部 IdP）、SSO Token 导入、本地缓存、凭证 JSON
- 用量统计、账号导入 / 导出、i18n（中 / 英 / 越）
- 支持配置出站代理（SOCKS5 / HTTP）
- 思考模式（Thinking Mode）

---

## 快速开始

### 方式一：Windows 一键脚本（本地开发最简单）

双击 `run.bat` 即可。它会自动：停掉旧进程 → 找空闲端口 → 编译 → 启动。

```
Admin 面板 : http://127.0.0.1:<端口>/admin
Claude API : http://127.0.0.1:<端口>/v1/messages
OpenAI API : http://127.0.0.1:<端口>/v1/chat/completions
```

### 方式二：Docker Compose（推荐用于服务器）

```bash
docker compose up -d --build
```

- `--build`：改了代码就必须带，否则会复用旧镜像缓存跑到旧版本。
- 检查：`docker compose ps`，然后 `curl http://localhost:8080/admin`。

详细的 Docker 部署（含 SSO loopback 端口机制、常见报错）见 `DEPLOYMENT.md`。

### 方式三：源码编译

```bash
go build -o kiro-go .
./kiro-go        # 默认读取 data/config.json，不存在则自动创建
```

配置文件自动创建于 `data/config.json`。默认管理密码为 `changeme` ——
**上线前务必**通过 `ADMIN_PASSWORD` 环境变量或在面板里修改。

---

## 使用方法

打开 `http://localhost:8080/admin` 登录，添加账号，然后调用 API：

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-你的key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-你的key" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好！"}]}'
```

> 当启用了 API Key 校验时，所有 API 请求都必须带 Key
>（Claude 用 `x-api-key`，OpenAI 用 `Authorization: Bearer`）。

### 端点一览

| 端点 | 说明 |
|------|------|
| `/v1/messages`、`/v1/messages/count_tokens` | Claude 兼容 |
| `/v1/chat/completions` | OpenAI 兼容 |
| `/v1/responses` | OpenAI Responses（含 30 天历史存储） |
| `/v1/models`、`/v1/stats` | 模型列表 / 统计 |
| `/v1/key/info`、`/v1/key/logs` | 自助：凭自己的 Key 查看信息 / 日志 |
| `/usage` | 自助用量面板（终端用户凭 Key 查看） |
| `/admin`、`/admin/api/*` | 管理面板（密码保护） |
| `/check`、`/health` | 公开：用量查询页 / 健康检查 |

### 思考模式

在模型名后加后缀（默认 `-thinking`），如 `claude-sonnet-4.5-thinking`。
Claude 请求里带顶层 `thinking` 配置（如 `{"type":"enabled","budget_tokens":2048}`）
也会自动开启。输出格式在面板 **设置 - 思考模式** 里配置。

### 模型重映射（解决 404 / 503）

如果客户端请求了一个上游不存在的模型名（比如把不存在的 `claude-sonnet-4.8` 写死进客户端），
用两种方式在不改客户端的情况下把它映射到真实模型：

- **全局强制模型（Force Model）**：设置 - 强制模型下拉框。覆盖**每一个**请求的模型。
- **按 Key 指定模型**：在 API Key 弹窗里给单个 Key 设置模型。

优先级：全局强制模型 > 按 Key 模型 > 客户端请求的模型。

### 外部供应商（兜底容量）

**供应商** 标签页可登记 OpenAI / Anthropic 兼容的上游，与 Kiro 账号池并行服务请求，在所有账号额度耗尽或处于冷却时继续提供服务。

供应商的**原生**端点由其协议决定。原生请求**原样转发**，只重写模型名，不丢失任何信息：

| 供应商协议 | 原生服务端点 |
|---|---|
| `anthropic` | `/v1/messages` |
| `openai` | `/v1/chat/completions`，开启 *原生服务 /v1/responses* 后还包括 `/v1/responses` |

开启 **启用协议桥接** 后，供应商还能服务它本不支持的端点，方式是翻译请求与响应：

| 客户端调用 | anthropic 供应商 | openai 供应商 |
|---|---|---|
| `/v1/messages` | 原样转发 | 翻译 |
| `/v1/chat/completions` | 翻译 | 原样转发 |
| `/v1/responses` | 翻译 | 开启 *原生服务* 则原样转发，否则翻译 |

桥接默认关闭，因为原样转发无损，而翻译只能尽力而为。它存在的实际理由：Claude Code 只调用 `/v1/messages`，而多数廉价的 OpenAI 兼容上游只实现 `/v1/chat/completions`——没有桥接，两者永远无法对接。同理，当前 OpenAI SDK 与 Codex 默认走 `/v1/responses`，而几乎没有廉价上游实现该接口。

翻译无法承载的部分：服务端会话状态（`previous_response_id` / `store`）、跨轮次重放的带签名推理内容，以及 OpenAI 的服务端内置工具（`web_search`、`code_interpreter` 等）。处理器在路由前已把历史展平进请求，因此行为规范的客户端不会丢失影响答案的内容。

仍然无法服务的端点会跳过该供应商，转而尝试下一个上游。

路由顺序由**层级（Tier）**字段决定，该字段与 Kiro 账号共用（账号详情 → 路由优先级）。层级按升序尝试，同一层级内账号优先于供应商，因此 `acc1 = 0`、`provider = 1`、`acc2 = 2` 会得到完全对应的调用链。全部保持默认的 `0` 则维持原有行为：先跑完整个 Kiro 池，供应商仅作兜底。

每个供应商拥有独立价目表，单位是**每 100 万 token 的积分**（输入 / 输出 / 缓存写入 / 缓存读取），从客户购买的同一积分余额中扣除。token 数取自供应商自己上报的 usage；OpenAI 协议的流式请求需要 `stream_options.include_usage`，客户端未设置时会自动注入（此时上游会额外返回一个只含 usage 的收尾分片）。

### 出站代理与代理池

- 单个出站代理：**设置 - 出站代理设置**，支持 SOCKS5 / HTTP，即时生效无需重启。
- **共享代理池**：多个代理轮换，健康状态持久化；某个代理挂了会被跳过（冷却后重试），
  重启也不丢失健康状态。
- **强制走代理（Require-proxy）**：开启后，任何拿不到可用代理的出站请求都会被拦截，
  避免泄露服务器真实 IP（含 token 刷新 / 外部 IdP discovery 路径）。

---

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | 管理密码（覆盖配置文件） | - |
| `LOOPBACK_HOST` | SSO loopback 绑定地址，**Docker 内必须设为 `0.0.0.0`** | `127.0.0.1` |
| `LOG_LEVEL` | 日志级别 `debug`/`info`/`warn`/`error` | `info` |
| `KIRO_MAX_BODY_BYTES` | 单请求最大 body | `10485760`（10 MiB） |
| `KIRO_MAX_CONCURRENT` | 全服务器并发请求上限 | `256` |
| `KIRO_IP_RPM` | 每 IP 每分钟请求数，超出直接拒绝 | `120` |
| `KIRO_PER_KEY_INFLIGHT` | 单个 Key 同时在 RPM 延迟中排队的请求数，超出返回 429 | `8` |
| `KIRO_TRUST_PROXY` | 从 `X-Forwarded-For`/`X-Real-IP` 读取真实 IP | `false` |

> ⚠️ `KIRO_TRUST_PROXY` 必须与实际是否有反向代理一致：直接暴露给公网时保持 `false`；
> 前面挂了 Nginx/Cloudflare 时必须设 `true`，否则所有请求都被当成来自 `127.0.0.1`。
> 详见 `deploy/HARDENING.md`。

---

## 出错了怎么排查

### 构建 / 运行

```bash
go build -o kiro-go .    # 编译
go test ./...            # 跑全部测试
go vet ./...             # 静态检查
```

- **编译失败**：先看 `go build ./...` 的报错行；确认 Go 版本 ≥ 1.21。
- **改了代码但跑的还是旧版**（Docker）：忘了带 `--build`。
  用 `docker compose up -d --build` 强制重建镜像。
- **`config.json` 被覆盖 / 丢了刚加的 Key**：确认没有多个 server 实例同时跑。
  `run.bat` 会先停旧实例正是为此。Docker 下确认没混用 `docker run` 和 `docker compose`
  产生两个容器（见 `DEPLOYMENT.md` 报错 #4）。
- **`Refusing to start: admin password is still the default on a non-loopback host`**：
  这是故意的安全保护——当密码还是 `changeme` **且** host 不是 loopback（比如 `0.0.0.0`）时拒绝启动。
  本地跑：把 `data/config.json` 里的 `"host"` 改成 `"127.0.0.1"`（可继续用 `changeme`）；
  公网 / Docker 部署：改设强密码环境变量 `ADMIN_PASSWORD`。切勿用 `0.0.0.0` + 默认密码对外暴露。

### 登录 SSO 报错

- **Start Login 返回 500 / "所有 loopback 端口都被占用"**：Docker 里 `LOOPBACK_HOST`
  设错了（比如少写一段成了 `0.0.0`）。用
  `docker compose exec kiro-go printenv LOOPBACK_HOST` 确认是 `0.0.0.0`，
  改完要 `docker compose up -d --force-recreate`（env 是构建时烤进容器的）。
- **端口冲突 `49153: address already in use`**（macOS）：那几个高位端口在 ephemeral
  范围内，已从 compose 移除，只需映射 5 个低位端口（3128–9091）。

### 请求报错

- **404 / 503 model not found**：客户端请求了上游不存在的模型名。
  用上面的[模型重映射](#模型重映射解决-404--503)把它映射到真实模型。
- **401 Bad credentials**（Microsoft Entra 账号）：确保导出 / 导入时保留了
  External IdP 元数据（issuerUrl/idpClientId/provider/scopes）。用面板里的
  **Copy JSON** 按钮导出即可保留。
- **上游 `CONTENT_LENGTH_EXCEEDS_THRESHOLD`**：请求体超过 ~2 MB。翻译器会自动截断，
  也可在设置里调 `MaxPayloadBytes`。
- **被限流（429 / 提示被拦）**：检查该 Key 的 RPM / 并发 IP / IP 白名单设置，
  以及全局 `KIRO_IP_RPM`。

### SSE 流式被截断 / 卡顿

- Go 的 `WriteTimeout` 故意设为 0，别改回去。
- 前面挂 Nginx 时必须 `proxy_buffering off;` + `proxy_read_timeout 600s;`，
  否则 token 会一顿一顿或者直接挂（见 `deploy/HARDENING.md`）。

---

## 免责声明

仅供学习与研究使用。与 Amazon、AWS 或 Kiro 无任何关联。使用者需自行遵守相关服务条款与法律法规，风险自负。

## 许可证

[MIT](LICENSE)
