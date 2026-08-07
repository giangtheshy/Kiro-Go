# Kiro-Go (bản nâng cấp)

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Biến tài khoản Kiro thành API tương thích OpenAI / Anthropic.

[English](README.md) | [中文](README_CN.md) | Tiếng Việt

> Đây là **bản fork nâng cấp** của Kiro-Go gốc. So với bản gốc, bản này bổ sung
> giới hạn theo từng API Key, pool proxy dùng chung + failover, remap model,
> trang xem usage tự phục vụ, và cả bộ chống DoS/DDoS.
> Khác biệt xem mục [Có gì hơn bản gốc](#có-gì-hơn-bản-gốc).

---

## Dự án này làm gì

Kiro-Go là một reverse proxy: nó phơi một nhóm tài khoản Kiro ra thành các endpoint
**tương thích OpenAI** và **tương thích Anthropic**. Nó quản lý pool tài khoản, dịch
request Claude/OpenAI sang định dạng AWS thượng nguồn của Kiro, stream response trả về,
và cung cấp một trang quản trị web.

Luồng request:

```
client → Handler.ServeHTTP (định tuyến) → xác thực → translator → pool chọn tài khoản
       → CallKiroAPI stream từ AWS → translator map event trả lại → client
```

Thuần Go stdlib `net/http`, không framework, chỉ một dependency (`github.com/google/uuid`).

---

## Có gì hơn bản gốc

| Tính năng | Kiro-Go gốc | Bản nâng cấp này |
|-----------|-------------|------------------|
| Giới hạn theo từng Key (RPM / trần IP đồng thời / IP allowlist / TPM hiển thị) | ❌ | ✅ |
| Gán Key vào tài khoản cố định (bound accounts) | ❌ | ✅ |
| Bộ đếm tổng trọn đời cho Key (reset chu kỳ vẫn giữ tổng) | ❌ | ✅ |
| Tạo / xóa / export API Key hàng loạt | ❌ | ✅ |
| Force Model toàn cục — remap tên model client gửi lên | ❌ | ✅ |
| Chỉ định model theo từng Key | ❌ | ✅ |
| Identity Model — cho assistant tự xưng là model tên gì | ❌ | ✅ |
| Pool proxy dùng chung + failover mức proxy (lưu trạng thái sức khỏe) | ❌ | ✅ |
| Công tắc Require-proxy (chặn lộ IP thật của server) | ❌ | ✅ |
| Chống DoS/DDoS (concurrency toàn cục, RPM theo IP, trần in-flight theo Key) | ❌ | ✅ |
| Trang usage tự phục vụ `/usage` (người dùng cuối tự xem bằng Key của mình) | ❌ | ✅ |
| Chống brute-force mật khẩu admin (`/admin/api/*`) | ❌ | ✅ |
| Thông báo giới hạn thân thiện (trả lời trong chat thay vì lỗi cứng) | ❌ | ✅ |
| Danh sách API Key tự động làm mới real-time mỗi 5 giây | ❌ | ✅ |

> Nhật ký thay đổi phiên bản xem `version.json`. Kết quả kiểm tra bảo mật xem
> `SECURITY_AUDIT.md`. Hướng dẫn triển khai chống DoS xem `deploy/HARDENING.md`.

### Tính năng nền tảng (kế thừa bản gốc)

- Anthropic `/v1/messages` và OpenAI `/v1/chat/completions`, `/v1/responses`
- Pool nhiều tài khoản + cân bằng tải weighted round-robin
- Tự refresh token, stream SSE, trang quản trị web
- Nhiều cách đăng nhập: AWS Builder ID, IAM Identity Center (SSO doanh nghiệp),
  Kiro Hosted SSO (gồm IdP ngoài như Microsoft Entra), import SSO Token, cache cục bộ, credentials JSON
- Thống kê usage, import / export tài khoản, đa ngôn ngữ (Trung / Anh / Việt)
- Hỗ trợ cấu hình proxy outbound (SOCKS5 / HTTP)
- Chế độ suy nghĩ (Thinking Mode)

---

## Bắt đầu nhanh

### Cách 1: Script một chạm trên Windows (đơn giản nhất cho dev local)

Nhấp đúp `run.bat`. Nó tự động: dừng tiến trình cũ → tìm port trống → build → chạy.

```
Admin panel : http://127.0.0.1:<port>/admin
Claude API  : http://127.0.0.1:<port>/v1/messages
OpenAI API  : http://127.0.0.1:<port>/v1/chat/completions
```

### Cách 2: Docker Compose (khuyến nghị cho server)

```bash
docker compose up -d --build
```

- `--build`: **bắt buộc khi code thay đổi**, nếu không compose dùng lại image cache cũ và chạy nhầm version cũ.
- Kiểm tra: `docker compose ps`, rồi `curl http://localhost:8080/admin`.

Chi tiết triển khai Docker (gồm cơ chế port loopback của SSO, các lỗi thường gặp) xem `DEPLOYMENT.md`.

### Cách 3: Build từ source

```bash
go build -o kiro-go .
./kiro-go        # mặc định đọc data/config.json, tự tạo nếu chưa có
```

Config tự tạo tại `data/config.json`. Mật khẩu admin mặc định là `changeme` —
**bắt buộc đổi trước khi lên production** qua biến `ADMIN_PASSWORD` hoặc đổi trong panel.

---

## Cách dùng

Mở `http://localhost:8080/admin` đăng nhập, thêm tài khoản, rồi gọi API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-key-cua-ban" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Xin chào!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-key-cua-ban" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Xin chào!"}]}'
```

> Khi đã bật kiểm tra API Key, mọi request API đều phải kèm Key
> (Claude dùng `x-api-key`, OpenAI dùng `Authorization: Bearer`).

### Danh sách endpoint

| Endpoint | Mô tả |
|----------|-------|
| `/v1/messages`, `/v1/messages/count_tokens` | Tương thích Claude |
| `/v1/chat/completions` | Tương thích OpenAI |
| `/v1/responses` | OpenAI Responses (có lưu lịch sử 30 ngày) |
| `/v1/models`, `/v1/stats` | Danh sách model / thống kê |
| `/v1/key/info`, `/v1/key/logs` | Tự phục vụ: xem info / log bằng Key của chính mình |
| `/usage` | Trang usage tự phục vụ (người dùng cuối xem bằng Key) |
| `/admin`, `/admin/api/*` | Trang quản trị (bảo vệ bằng mật khẩu) |
| `/check`, `/health` | Công khai: trang tra usage / health check |

### Chế độ suy nghĩ (Thinking Mode)

Thêm hậu tố (mặc định `-thinking`) vào tên model, ví dụ `claude-sonnet-4.5-thinking`.
Request Claude có block `thinking` ở cấp cao nhất (vd `{"type":"enabled","budget_tokens":2048}`)
cũng tự bật. Định dạng output cấu hình trong panel **Settings - Thinking Mode**.

### Remap model (xử lý lỗi 404 / 503)

Nếu client gửi lên một tên model không tồn tại ở thượng nguồn (ví dụ hard-code
`claude-sonnet-4.8` vốn không có thật), có hai cách remap sang model thật mà không cần sửa client:

- **Force Model (toàn cục)**: dropdown Settings - Force Model. Ghi đè **mọi** request.
- **Model theo từng Key**: đặt model cho từng Key trong modal API Key.

Thứ tự ưu tiên: Force Model toàn cục > Model theo Key > model client gửi lên.

### Nhà cung cấp ngoài (dung lượng dự phòng)

Tab **Providers** cho phép khai báo các upstream tương thích OpenAI hoặc Anthropic để phục vụ request song song với pool Kiro, giữ dịch vụ chạy tiếp khi mọi account đã hết quota hoặc đang cooldown.

Request được **chuyển tiếp nguyên văn** — chỉ tên model bị ghi đè — nên mỗi nhà cung cấp chỉ phục vụ đúng endpoint theo giao thức của nó:

| Giao thức | Phục vụ |
|---|---|
| `anthropic` | `/v1/messages` |
| `openai` | `/v1/chat/completions`, thêm `/v1/responses` khi bật *Phục vụ cả /v1/responses* |

Endpoint không khớp sẽ bỏ qua nhà cung cấp đó và rơi xuống upstream kế tiếp. **Nếu khách của bạn dùng Claude Code (gọi vào `/v1/messages`) thì nhà cung cấp dự phòng bắt buộc phải là endpoint tương thích Anthropic** — loại tương thích OpenAI sẽ không bao giờ được chọn cho luồng đó.

Thứ tự định tuyến lấy từ trường **Bậc (Tier)**, dùng chung với account Kiro (chi tiết account → Thứ tự ưu tiên định tuyến). Bậc được thử theo thứ tự tăng dần, trong cùng một bậc thì account đi trước nhà cung cấp; nên `acc1 = 0`, `provider = 1`, `acc2 = 2` sẽ cho đúng chuỗi đó. Để mặc định `0` cho tất cả thì hành vi giữ nguyên như trước: thử hết pool Kiro rồi mới tới nhà cung cấp.

Mỗi nhà cung cấp có bảng giá riêng tính bằng **credits trên 1M token** (input / output / cache write / cache read), trừ vào đúng số credits khách đã mua. Số token đọc từ báo cáo usage của chính nhà cung cấp; riêng luồng streaming giao thức OpenAI cần `stream_options.include_usage`, hệ thống sẽ tự thêm khi client không đặt (khi đó upstream trả thêm một chunk cuối chỉ chứa usage).

### Proxy outbound & pool proxy

- Proxy outbound đơn: **Settings - Outbound Proxy**, hỗ trợ SOCKS5 / HTTP, có hiệu lực ngay không cần restart.
- **Pool proxy dùng chung**: nhiều proxy xoay vòng, trạng thái sức khỏe được lưu; proxy chết
  sẽ bị bỏ qua (thử lại sau thời gian chờ), không mất trạng thái kể cả sau restart.
- **Require-proxy**: khi bật, mọi request outbound không có proxy khả dụng sẽ bị chặn,
  tránh lộ IP thật của server (kể cả đường refresh token / OIDC discovery của IdP ngoài).

---

## Biến môi trường

| Biến | Mô tả | Mặc định |
|------|-------|----------|
| `CONFIG_PATH` | Đường dẫn file config | `data/config.json` |
| `ADMIN_PASSWORD` | Mật khẩu admin (ghi đè config) | - |
| `LOOPBACK_HOST` | Địa chỉ bind loopback SSO, **trong Docker phải đặt `0.0.0.0`** | `127.0.0.1` |
| `LOG_LEVEL` | Mức log `debug`/`info`/`warn`/`error` | `info` |
| `KIRO_MAX_BODY_BYTES` | Body request tối đa | `10485760` (10 MiB) |
| `KIRO_MAX_CONCURRENT` | Tổng số request đồng thời toàn server | `256` |
| `KIRO_IP_RPM` | Request/phút/IP, vượt là reject ngay | `120` |
| `KIRO_PER_KEY_INFLIGHT` | Số request 1 Key được xếp trong RPM-delay cùng lúc, vượt → 429 | `8` |
| `KIRO_TRUST_PROXY` | Đọc IP thật từ `X-Forwarded-For`/`X-Real-IP` | `false` |

> ⚠️ `KIRO_TRUST_PROXY` phải khớp với việc có reverse proxy hay không: phơi thẳng ra
> Internet thì để `false`; có Nginx/Cloudflare phía trước thì **bắt buộc `true`**, nếu không
> mọi request đều bị coi như đến từ `127.0.0.1`. Chi tiết xem `deploy/HARDENING.md`.

---

## Chạy bị lỗi thì sửa thế nào

### Build / chạy

```bash
go build -o kiro-go .    # build
go test ./...            # chạy toàn bộ test
go vet ./...             # kiểm tra tĩnh
```

- **Build fail**: xem dòng lỗi của `go build ./...` trước; đảm bảo Go ≥ 1.21.
- **Sửa code rồi mà vẫn chạy version cũ** (Docker): quên `--build`.
  Dùng `docker compose up -d --build` để ép build lại image.
- **`config.json` bị ghi đè / mất Key vừa thêm**: đảm bảo không có nhiều instance server
  chạy cùng lúc. `run.bat` dừng instance cũ trước chính vì lý do này. Trên Docker, đừng
  trộn `docker run` với `docker compose` tạo ra hai container (xem `DEPLOYMENT.md` lỗi #4).
- **`Refusing to start: admin password is still the default on a non-loopback host`**:
  đây là guard bảo mật cố ý — app từ chối khởi động khi mật khẩu vẫn là `changeme` **và**
  host không phải loopback (vd `0.0.0.0`). Cách sửa: chạy local thì đổi `"host"` trong
  `data/config.json` thành `"127.0.0.1"` (vẫn giữ được `changeme`); deploy public / Docker
  thì đặt biến môi trường `ADMIN_PASSWORD` mạnh. Đừng bao giờ mở `0.0.0.0` với pass mặc định.

### Lỗi đăng nhập SSO

- **Start Login trả 500 / "tất cả port loopback đều bận"**: `LOOPBACK_HOST` trong Docker
  bị đặt sai (vd thiếu octet thành `0.0.0`). Kiểm tra bằng
  `docker compose exec kiro-go printenv LOOPBACK_HOST` phải ra `0.0.0.0`, sửa xong phải
  `docker compose up -d --force-recreate` (env được bake vào container lúc tạo).
- **Xung đột port `49153: address already in use`** (macOS): các port cao đó nằm trong dải
  ephemeral, đã loại khỏi compose, chỉ cần map 5 port thấp (3128–9091).

### Lỗi khi gọi request

- **404 / 503 model not found**: client gửi tên model không tồn tại ở thượng nguồn.
  Dùng [remap model](#remap-model-xử-lý-lỗi-404--503) ở trên để map sang model thật.
- **401 Bad credentials** (tài khoản Microsoft Entra): đảm bảo export / import giữ đủ
  metadata External IdP (issuerUrl/idpClientId/provider/scopes). Dùng nút **Copy JSON**
  trong panel để export là giữ được.
- **Thượng nguồn báo `CONTENT_LENGTH_EXCEEDS_THRESHOLD`**: body request vượt ~2 MB.
  Translator tự cắt bớt; cũng có thể chỉnh `MaxPayloadBytes` trong settings.
- **Bị giới hạn (429 / bị chặn)**: kiểm tra cấu hình RPM / IP đồng thời / IP allowlist của
  Key đó, và `KIRO_IP_RPM` toàn cục.

### Stream SSE bị cắt / giật

- `WriteTimeout` của Go cố ý để 0, đừng đặt lại.
- Có Nginx phía trước thì bắt buộc `proxy_buffering off;` + `proxy_read_timeout 600s;`,
  không thì token nhận giật cục hoặc treo (xem `deploy/HARDENING.md`).

---

## Miễn trừ trách nhiệm

Chỉ dùng cho mục đích học tập và nghiên cứu. Không liên kết với Amazon, AWS hay Kiro.
Người dùng tự chịu trách nhiệm tuân thủ điều khoản dịch vụ và pháp luật liên quan. Rủi ro tự chịu.

## Giấy phép

[MIT](LICENSE)
