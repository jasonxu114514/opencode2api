# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md)

Một gateway viết bằng Go cho **OpenCode Zen và Zen Go**. Nó cung cấp các endpoint Chat Completions, Responses và Anthropic Messages, chuyển đổi qua lại giữa các giao thức gốc của chúng, và quản lý upstream key cùng proxy.

File thực thi đã bao gồm WebUI. Việc chạy dịch vụ không cần Node.js runtime hay cơ sở dữ liệu.

## Tính năng

- Phản hồi JSON và SSE trên cả ba giao thức suy luận.
- Văn bản, hình ảnh, suy luận, function tool, tool call và tool result; nội dung tệp ở nơi giao thức đích có thể biểu diễn.
- Hai nhóm key Zen và Go riêng biệt, tier ưu tiên có thể cấu hình, retry và session affinity.
- Truy cập Zen ẩn danh tuỳ chọn bằng credential `public` của OpenCode.
- Kết nối direct, HTTP, HTTPS, SOCKS5 và SOCKS5H, bao gồm cả file proxy.
- Tự động phát hiện mô hình, metadata giao thức gốc và cache trên đĩa.
- Cổng quản trị riêng với chỉnh sửa cấu hình, Playground, chẩn đoán, thống kê token và log trực tiếp.
- Nạp lại cấu hình nóng sau khi kiểm tra, trước khi chuyển request mới sang gateway thay thế.
- Bảng hạn mức gói Go với phần trăm còn lại, thời điểm reset và danh sách mô hình khả dụng.
- Tạo, đặt tên, bật, tắt và xoá API key cục bộ, có hiệu lực tức thì.
- Nhóm mô hình `sota` / `sweet` bám dính, có thứ tự, chuyển khi lỗi, bảo vệ luồng và trạng thái được lưu.

Giao diện quản trị hỗ trợ **简体中文** (mặc định), **English** và **Tiếng Việt**; lựa chọn được lưu trong trình duyệt và không làm thay đổi bản gốc.

Xem [hướng dẫn bảng điều khiển quản trị (tiếng Trung)](docs/management.zh-CN.md) để biết các trang mới và tuỳ chọn cấu hình.

## Bắt đầu nhanh

Tải file nhị phân từ [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases), hoặc build với **Go 1.24 trở lên**:

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

Trước khi khởi động, hãy sửa `config.json`:

1. Thay `server_keys` bằng API key cục bộ của bạn.
2. Cung cấp key Zen hoặc Go. Nếu không, đặt `anonymous: true` và để cả hai mảng key upstream rỗng.
3. Thay `webui.password`. File mẫu bật WebUI với tài khoản `admin`.

```bash
./opencode2api -config config.json
```

Trên Windows, dùng `Copy-Item config.example.json config.json`, build bằng `go build -o opencode2api.exe ./cmd/opencode2api`, rồi chạy `.\opencode2api.exe -config config.json`.

File mẫu lắng nghe `127.0.0.1:8080` cho API và `0.0.0.0:8081` cho WebUI. Mở `http://localhost:8081` ở máy cục bộ. Hãy giới hạn quyền truy cập quản trị và dùng reverse proxy HTTPS khi truy cập qua mạng.

Tuỳ chọn dòng lệnh:

| Tuỳ chọn       | Mặc định       | Mục đích                                     |
| -------------- | -------------- | -------------------------------------------- |
| `-config`      | `config.json`  | Đường dẫn file cấu hình.                     |
| `-listen`      | Không đặt      | Ghi đè địa chỉ lắng nghe của API.            |
| `-web-listen`  | Không đặt      | Ghi đè địa chỉ lắng nghe của WebUI.          |

Thư mục cấu hình phải ghi được để thực hiện việc chuyển mật khẩu, lưu cấu hình và cache mô hình.

## Docker

Image được phát hành là `ghcr.io/jasonxu114514/opencode2api`.

```bash
cp config.example.json config.json
# Cấu hình key và thay mật khẩu WebUI trước khi khởi động.
docker compose up -d
docker compose logs -f
```

Compose chỉ import cấu hình từ máy host vào volume `opencode2api-state` **ở lần khởi động đầu tiên**. Những thay đổi sau đó nên thực hiện qua WebUI. Để import lại file cấu hình đã sửa trên host:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Container chạy dịch vụ bằng user không có đặc quyền. Compose dùng hệ thống file gốc chỉ đọc, volume trạng thái ghi được và hệ thống file tạm `/tmp`.

| Biến Compose                     | Mặc định  | Tác dụng                                                    |
| -------------------------------- | --------- | ----------------------------------------------------------- |
| `OPENCODE2API_VERSION`           | `latest`  | Tag image. Ghim tag release khi cần phiên bản cố định.       |
| `OPENCODE2API_PORT`              | `8080`    | Cổng host ánh xạ tới cổng 8080 của container.                |
| `OPENCODE2API_WEBUI_PORT`        | `8081`    | Cổng host ánh xạ tới cổng 8081 của container.                |
| `OPENCODE2API_LISTEN`            | Không đặt | Ghi đè rõ ràng địa chỉ lắng nghe API trong container.        |
| `OPENCODE2API_WEBUI_LISTEN`      | Không đặt | Ghi đè rõ ràng địa chỉ lắng nghe WebUI trong container.      |

Hai biến cuối trở thành `LISTEN_ADDRESS` và `WEBUI_LISTEN_ADDRESS` bên trong container. Giá trị rỗng sẽ dùng file cấu hình. Khi khởi tạo cấu hình, entrypoint đổi địa chỉ API mẫu `127.0.0.1:8080` thành `0.0.0.0:8080` để các cổng đã publish truy cập được.

Đổi cổng host không làm thay đổi cổng lắng nghe trong container. Nếu đổi cổng API bên trong, cũng phải cập nhật ánh xạ cổng và health check của image (dùng cổng 8080).

Build image cục bộ:

```bash
docker build -t opencode2api:local .
docker volume create opencode2api-state
docker run -d --name opencode2api \
  -p 8080:8080 -p 8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

`proxyfile` dùng trong Docker cũng phải tồn tại bên trong container tại đường dẫn đã cấu hình.

## Sử dụng API

`server_keys` dùng để xác thực client với gateway này. Chúng tách biệt với `zen_keys` và `go_keys` và không bao giờ được dùng làm credential upstream.

Gửi `Authorization: Bearer YOUR_LOCAL_API_KEY` hoặc `x-api-key: YOUR_LOCAL_API_KEY`. Health check không cần xác thực.

| Phương thức | Đường dẫn               | Mô tả                                                          |
| ----------- | ----------------------- | ------------------------------------------------------------- |
| GET         | `/v1/models`            | Các mô hình có thể định tuyến với cấu hình hiện tại.          |
| POST        | `/v1/chat/completions`  | Chat Completions.                                              |
| POST        | `/v1/responses`         | Responses.                                                     |
| POST        | `/v1/messages`          | Anthropic Messages.                                            |
| POST        | `/v1/systemone`         | Quyết định có cấu trúc của System One.                        |
| GET         | `/healthz`              | Trạng thái sẵn sàng và tóm tắt tài nguyên.                     |

Hãy tìm một mô hình khả dụng trước:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"
```

Thay `MODEL_ID` bên dưới bằng một ID có trong phản hồi đó.

**Chat Completions**

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}'
```

**Responses**

```bash
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","input":"Hello"}'
```

**Anthropic Messages**

```bash
curl http://localhost:8080/v1/messages \
  -H "x-api-key: YOUR_LOCAL_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}'
```

Để dùng streaming, thêm `"stream": true` vào body và dùng `curl -N`. Phản hồi có `x-request-id` để đối chiếu. Body của request API giới hạn 32 MiB.

**System One**

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

System One ghép một state tự do với các câu hỏi có kiểu và trả về câu trả lời có kiểu, nên payload này không dùng chung hình dạng với cầu nối chat/responses và được chuyển tiếp nguyên văn, không bao giờ dịch. Chỉ những mô hình có giao thức gốc là System One mới được phục vụ tại đây; một payload quyết định gửi tới endpoint message (`/v1/chat/completions`, `/v1/responses`) cũng được chuyển tiếp nguyên văn thay vì chuyển đổi. Câu trả lời streaming từ upstream được chuyển tiếp nguyên trạng, không khôi phục luồng.

Để kiểm tra các mô hình và lưu ID của những mô hình gọi thành công vào `working-models.txt`, chạy `bash scripts/check-models.sh`. Script dùng `config.json` để lấy `server_keys` và sẽ hỏi xác nhận trước khi gửi request; thêm `--yes` để bỏ qua xác nhận.

### Ranh giới tương thích

Request dùng giao thức gốc của upstream sẽ giữ nguyên các trường riêng của nhà cung cấp. Request đi qua nhiều giao thức sẽ đi qua một biểu diễn chung; không phải tuỳ chọn nào cũng có phần tương đương. Ví dụ, ràng buộc output JSON của Chat và `seed` không được chuyển tiếp sang Anthropic Messages. Nội dung hoặc loại tool không được hỗ trợ có thể bị từ chối.

Gateway cung cấp đúng các route nêu trên. Nó không triển khai embeddings, tải tệp lên, sinh ảnh, hay truy xuất và huỷ của Responses. Nó không lưu lịch sử hội thoại; client phải tự cung cấp lịch sử hoặc dùng các tính năng được upstream thật hỗ trợ.

## Định tuyến và mô hình

### Phát hiện mô hình

Gateway làm mới `/v1/models` của Zen/Go và [danh mục năng lực](https://models.opencode.ai/api.json) của OpenCode theo chu kỳ đã cấu hình. Giao thức gốc và giới hạn mô hình được theo dõi riêng cho từng tier. Tài liệu Zen/Go của OpenCode là nguồn dự phòng để xác định giao thức.

`models.protocols` ghi đè việc phát hiện:

```json
{
  "models": {
    "refresh_seconds": 300,
    "protocols": {
      "custom-model": "chat"
    }
  }
}
```

Giá trị giao thức hợp lệ là `chat`, `responses` và `anthropic`. Mô hình dùng giao thức gốc không được hỗ trợ sẽ bị lọc khỏi danh sách phát hiện trừ khi được ghi đè.

Chi phí, trạng thái ngừng cấp và giới hạn mô hình đến từ [endpoint metadata chính thức của OpenCode](https://models.opencode.ai/api.json), làm mới mỗi 24 giờ. Danh mục năng lực của OpenCode vẫn là nguồn chuẩn cho giới hạn context và token; các trường metadata `limit.context`, `limit.input` và `limit.output` điền cho các trường còn thiếu trong danh mục đó. `/v1/models` công khai các giới hạn này qua `context_window`/`context_length`, `max_input` và `max_output` cho các client phát hiện như CLIProxyAPI và Pi. Mỗi request metadata dùng timeout 30 giây trên HTTP client. Khi làm mới thất bại, dữ liệu cũ vẫn được giữ.

Mỗi phần tử của `/v1/models` cũng mang trường `cost` (USD trên một triệu token: `input`, `output`, `cache_read`, `cache_write`, cùng `cacheRead`/`cacheWrite` của OpenClaw) khi metadata nêu giá. Mô hình có suy luận liệt kê các bậc của chúng trong `reasoning_efforts` và trong `compat.supportedReasoningEfforts` của OpenClaw: từ `minimal` đến `max`, bị chặn ở trần đã dò khi bậc của mô hình là `auto`. Bậc bị ép hoặc được dò sẽ được báo là `default_reasoning_effort`. Giá cache hiển thị sau lần làm mới metadata kế tiếp.

### Truy cập ẩn danh và dự phòng

Khi `anonymous: true`, chỉ cần một trong hai điều kiện sau là mô hình đủ điều kiện cho làn Zen ẩn danh:

- ID của nó chứa `free`, không phân biệt hoa thường.
- Metadata OpenCode báo chi phí input và output bằng 0 và mô hình không bị ngừng cấp.

Điều kiện là một quyết định định tuyến; upstream vẫn có thể từ chối hoặc giới hạn tần suất request. Request ẩn danh dùng `public` trong header xác thực của upstream.

Trình tự định tuyến là:

1. Với mô hình đủ điều kiện, thử lần lượt mỗi proxy ẩn danh khả dụng.
2. Thử các tier có xác thực theo thứ tự `prefer`, chỉ dùng những tier có key được cấu hình và có route cho mô hình đó.
3. Áp dụng `retry.max_attempts` riêng cho từng tier có xác thực.

Các lần thử ẩn danh không bị `retry.max_attempts` cắt ngắn, nhưng mọi lần thử đều dùng chung timeout của request. Lỗi mạng, xác thực thất bại, giới hạn tần suất và lỗi máy chủ có thể làm luân chuyển key. Các phản hồi 4xx khác kết thúc tier hiện tại; một tier khác còn khả dụng vẫn có thể được thử.

Request được mã hoá theo giao thức riêng của từng tier. Khi bật `performance.first_event_timeout_seconds`, một phản hồi SSE thành công sẽ được giữ lại cho tới sự kiện dữ liệu hoàn chỉnh đầu tiên. Timeout hoặc mất kết nối trước sự kiện đó sẽ dùng ngân sách retry key/proxy hiện có; comment không tính là thoát timeout. Sau khi được giải phóng xuống phía dưới, luồng không bao giờ được phát lại. Mặc định là `0` (tắt); hãy thử `15` giây cho các upstream nhận request nhưng không gửi sự kiện nào. Giới hạn này áp dụng cho sự kiện đầu tiên, không phải token văn bản đầu tiên, và không áp dụng cho System One. Bí danh luân chuyển dùng timeout riêng `rotation.first_output_seconds`. Một tham chiếu reasoning Responses bị lỗi thời đã nhận diện có thể kích hoạt một lượt sửa; chẩn đoán chọn key cụ thể không bao giờ dùng lượt phát lại đó.

Khi chỉ cấu hình truy cập ẩn danh, `/v1/models` chỉ công khai các mô hình đủ điều kiện cho làn đó.

Mô hình miễn phí không bao giờ cần key Zen: với `anonymous: true` chúng được phục vụ bởi làn public, và key được cấu hình chỉ là phương án dự phòng. Muốn giữ key nhưng chỉ công khai mô hình miễn phí, hãy đặt `models.free_only: true`. Khi đó mô hình trả phí biến mất khỏi `/v1/models`, request trực tiếp tới chúng bị từ chối, và bí danh luân chuyển chỉ giữ lại thành viên miễn phí; mô hình miễn phí vẫn có thể dự phòng về các key đã cấu hình.

### Tính khả dụng của mô hình miễn phí

Gateway dò các mô hình có ID chứa `free` (không phân biệt hoa thường), hoặc có metadata báo chi phí input/output bằng 0 mà không bị ngừng cấp, bằng một request ngắn dùng giao thức gốc của mô hình. Nó thử làn ẩn danh đang bật trước, rồi tới một Zen Key đã cấu hình. Một phản hồi hợp lệ và hoàn tất trên bất kỳ làn nào sẽ giữ mô hình ở trạng thái bật; mô hình chỉ bị tắt trong phát hiện và định tuyến khi mọi làn được thử đều báo rõ ràng rằng mô hình không tồn tại hoặc đã bị thu hồi. Lỗi xác thực, giới hạn tần suất, timeout, lỗi truyền tải, phản hồi 5xx và câu trả lời sai định dạng đều không kết luận: chúng không tạo ra lần tắt mới và sẽ được kiểm tra lại sau một giờ. Lưu lượng dò không làm thay đổi cooldown key/proxy sản xuất hay thống kê request. Nó vẫn đi tới upstream và có thể tiêu tốn hạn mức miễn phí của họ.

Mô hình System One dùng câu hỏi `noul` gốc hỏi rằng 1 + 1 có bằng 2 không; chúng không bao giờ được gửi payload chat/tool. Việc phát hiện mô hình gồm `native_protocol` và `supported_endpoints`; bí danh luân chuyển chỉ được liệt kê khi có key Go và mô hình đủ điều kiện. Lần tắt còn sót từ bản v1.3.6 sẽ được xoá khi nạp và lên lịch kiểm tra lại ngay, vì các lần dó đó dùng session ID không hợp lệ.

Chu kỳ là cố định: mô hình đang bật được kiểm tra mỗi **1 giờ**, mô hình đã xác nhận không khả dụng được kiểm tra mỗi **24 giờ**. Một lần kiểm tra lại không kết luận đối với mô hình đã tắt sẽ giữ nguyên lý do tắt ban đầu và hẹn thử lại sau 24 giờ. Một lần kiểm tra lại thành công sẽ tự động khôi phục mô hình. Trang **Tính khả dụng mô hình miễn phí** của WebUI có thể khôi phục ngay, và điều đó đưa mô hình về điều khiển thủ công: nó giữ nguyên bật và bị loại khỏi việc dò tự động, nên kết quả dò về sau không thể ghi đè. Dùng nút **thủ công/tự động** theo từng mô hình trên cùng trang để trở lại dò tự động; lượt của bộ lập lịch kế tiếp sẽ hẹn một lần dò. Các bản ghi để lại ở trạng thái `manually_enabled` từ phiên bản cũ sẽ được chuyển sang điều khiển thủ công khi nạp. Một lần dò đang chạy không thể huỷ việc khôi phục thủ công. Các mô hình không có làn Zen/ẩn danh dùng được, hoặc không có giao thức suy luận được hỗ trợ, sẽ bị bỏ qua.

Điều khiển thủ công cũng tạm dừng việc dò bậc suy luận cho `reasoning.effort=auto`; các bậc đã học sẽ ngừng bị ép sau hai giờ không xác minh. Trở lại điều khiển tự động cho phép việc dò bậc suy luận tiếp tục.

Trạng thái tồn tại qua các lần khởi động trong `config.json.<upstream-fingerprint>.availability.json`, tách theo URL upstream Zen. Bộ lập lịch nền kiểm tra mô hình đến hạn mỗi phút một lần, bỏ qua mô hình điều khiển thủ công, và dò lần lượt phần còn lại, mỗi làn timeout 60 giây. Không có kênh nào khả dụng nghĩa là không tự động tắt.

Các endpoint quản trị (cần đăng nhập; ghi còn cần CSRF): `GET /api/models/availability`, `POST /api/models/restore` với `{"model":"MODEL_ID"}` (khôi phục và chuyển sang điều khiển thủ công), và `POST /api/models/manual` với `{"model":"MODEL_ID","manual":true|false}` để chuyển qua lại giữa điều khiển thủ công và tự động.

### Session và proxy

Các key ban đầu được phân bố trên nhiều proxy. Lưu lượng thật có thể kích hoạt kiểm tra proxy, gán lại key và cooldown. Proxy không khoẻ được kiểm tra lại mỗi 15 phút bằng Cloudflare trace.

Một hash session ổn định chọn key hoặc proxy ẩn danh ưu tiên. Gửi `x-session-id` tường minh để tách các hội thoại độc lập. Gateway cũng chấp nhận `x-opencode-session`, `x-session-affinity`, `conversation-id`, `conversation_id` và `metadata.session_id`.

Không có ID tường minh, tin nhắn người dùng đầu tiên được dùng làm seed. Vì vậy các hội thoại mở đầu bằng nội dung giống nhau có thể dùng chung affinity. Thay đổi thành phần nhóm có thể làm đổi nút được chọn.

Cooldown tăng theo cấp số nhân tối đa tám lần `performance.failure_cooldown_seconds`, và giá trị `Retry-After` dài hơn sẽ được tôn trọng. Nếu mọi key có xác thực đều đang nghỉ, định tuyến có thể thử key mà cooldown kết thúc sớm nhất. Các nút ẩn danh còn trong cooldown sẽ bị bỏ qua.

## Tham chiếu cấu hình

Xem [config.example.json](config.example.json) để có một cấu hình khởi đầu đầy đủ. JSON hỗ trợ chú thích `//` và `/* ... */`; trường lạ và giá trị không hợp lệ đều bị từ chối.

### Key, cổng lắng nghe và định tuyến

| Trường                     | Mặc định / yêu cầu                                              |
| -------------------------- | ---------------------------------------------------------------- |
| `listen`                   | `127.0.0.1:8080`.                                                |
| `server_keys`              | Cần ít nhất một key cục bộ.                                       |
| `zen_keys`, `go_keys`      | Cần ít nhất một key upstream trừ khi bật truy cập ẩn danh.       |
| `anonymous`                | `false`.                                                         |
| `prefer`                   | `go`; nhận `go` hoặc `zen`.                                      |
| `upstream.zen`             | `https://opencode.ai/inference/openai`.                          |
| `upstream.go`              | `https://opencode.ai/zen/go`.                                    |
| `proxies`                  | Rơi về `["direct"]` khi cả hai nguồn proxy đều rỗng.              |
| `proxyfile`                | Không bắt buộc; đường dẫn tương đối được phân giải cạnh file cấu hình. |
| `models.refresh_seconds`   | `300`; tối thiểu 1.                                              |
| `models.protocols`         | `{}`; ghi đè giao thức gốc theo từng mô hình.                   |
| `models.free_only`         | `false`; chỉ công khai và định tuyến mô hình miễn phí, kể cả khi có key Zen hoặc Go. |
| `reasoning.effort`         | Rỗng (tắt); `minimal`, `low`, `medium`, `high`, `xhigh`, `max` hoặc `none`. |
| `reasoning.effort_by_model`| `{}`; ghi đè `reasoning.effort` theo từng mô hình.              |

### Bậc suy luận bị ép

`reasoning.effort` đặt bậc suy luận được dùng ở upstream khi một request không tự nêu bậc của nó, và `reasoning.effort_by_model` ghi đè bậc đó cho các ID mô hình mà nó nêu. Nó áp dụng cho cả ba giao thức, theo cả hai chiều chuyển đổi, và quan trọng nhất với lưu lượng liên giao thức: `thinking.budget_tokens` của Anthropic chỉ có thể được xấp xỉ bởi bề mặt Chat và Responses, nên một bậc bị ép có thể đạt tới bậc mà ngân sách token không diễn đạt được.

```json
{
  "reasoning": {
    "effort": "high",
    "effort_by_model": { "claude-opus-5": "max" }
  }
}
```

Bậc do client nêu tường minh luôn thắng, nên thiết lập này chỉ thay thế một bậc mà không ai yêu cầu: bậc bị thiếu, hoặc bậc suy ra từ `thinking.budget_tokens`. Dùng `"none"` để loại bỏ reasoning khỏi các request không yêu cầu nó.

Các bậc được báo trung thực theo cả hai chiều. Một `output_config.effort` tường minh sống sót qua chuyển đổi thay vì bị khối `thinking` che khuất, `budget_tokens` giữ đúng bậc của nó (`8192` là `high`, `32000` là `xhigh`, nên hai mức vẫn phân biệt được), và phía Responses mang bậc này dưới dạng `reasoning.effort` thay vì bỏ mất.

Trung tâm cấu hình của WebUI cung cấp cùng các trường mặc định và theo mô hình. API quản trị trả chúng dưới trường `reasoning` từ `GET /api/config` và nhận đối tượng đó trong `PUT /api/config`; client bỏ qua trường sẽ giữ giá trị hiện tại, còn gửi đối tượng rỗng sẽ xoá nó.

Các mục proxy nhận `direct`, `http://`, `https://`, `socks5://` và `socks5h://`, kể cả credential trong URL. Danh sách nội tuyến được gộp với `proxyfile` và khử trùng lặp theo thứ tự.

Một file proxy chứa mỗi dòng một mục và hỗ trợ dòng trống cùng chú thích:

```text
# Proxy chính
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # Proxy dự phòng
direct
```

Dấu bắt đầu chú thích là `#`, `;` và `//`, ở đầu dòng hoặc sau khoảng trắng.

#### Proxy pool luân chuyển

Thêm hậu tố `#pool` (hoặc tiền tố `pool+`) vào một mục để khai báo rằng endpoint đó là proxy pool luân chuyển như [Resin](https://github.com/Resinat/Resin), nơi mỗi kết nối TCP mới đi ra từ một IP khác:

```text
http://127.0.0.1:7890#pool
socks5://127.0.0.1:1080#pool
pool+http://127.0.0.1:7891
```

Mục `#pool` được xử lý khác hoàn toàn so với proxy tĩnh:

| Hành vi               | Proxy tĩnh                                                     | Mục `#pool`                                                              |
| --------------------- | -------------------------------------------------------------- | ------------------------------------------------------------------------ |
| Tái sử dụng kết nối   | Keep-alive và ghép kênh HTTP/2 (tham số `performance.*`)       | Một tunnel TCP + CONNECT mới cho mỗi request (`DisableKeepAlives`, không HTTP/2) |
| Cooldown khi lỗi      | 15s–120s lùi theo cấp số nhân, cộng thêm `Retry-After`         | Không: request kế tiếp lấy luôn exit IP mới                              |
| Loại bỏ theo sức khoẻ | Đánh dấu không khoẻ, chuyển key, kiểm tra lại mỗi 15 phút      | Không bao giờ bị loại bỏ; bỏ qua hoàn toàn health check                 |
| Số lần thử kênh ẩn danh | Tối đa một lần cho mỗi proxy đã cấu hình                     | Tối đa `retry.max_attempts` lần, quay vòng mục pool                      |

Dùng khi bản thân endpoint sau URL đã tự luân chuyển, còn từng exit IP trong pool bị giới hạn riêng hoặc chập chờn: gateway sẽ coi mỗi lần thử là một endpoint mới thay vì ghim cả phiên vào một exit. Chỉ cần một mục `#pool` là đủ, không cần liệt kê hàng chục exit. Các mục tĩnh vẫn giữ hành vi pooling mô tả bên dưới.

### Timeout và connection pool

| Trường                                   | Mặc định | Ý nghĩa                                                     |
| ---------------------------------------- | -------- | ----------------------------------------------------------- |
| `retry.max_attempts`                     | `3`      | Số lần thử cho mỗi tier có xác thực, kể cả lần đầu.        |
| `retry.timeout_seconds`                  | `300`    | Timeout suy luận tổng, gồm cả thời gian đọc luồng.          |
| `performance.attempt_timeout_seconds`    | `0`      | Giới hạn cho một lần thử; 0 dùng timeout của request.      |
| `performance.connect_timeout_seconds`    | `5`      | Timeout thiết lập kết nối.                                  |
| `performance.failure_cooldown_seconds`   | `15`     | Cooldown lỗi cơ sở.                                          |
| `performance.max_idle_conns`             | `2048`   | Giới hạn kết nối rảnh cho mỗi transport proxy.              |
| `performance.max_idle_conns_per_host`    | `256`    | Giới hạn kết nối rảnh cho mỗi host và transport.            |
| `performance.max_conns_per_host`         | `0`      | Giới hạn kết nối cho mỗi host; 0 là không giới hạn.        |
| `performance.idle_conn_timeout_seconds`  | `120`    | Thời gian sống của kết nối rảnh.                            |

Timeout header cho mỗi lần thử bị chặn trên bởi timeout của request. Hãy đặt nó thấp hơn timeout tổng nếu muốn một upstream chậm vẫn còn thời gian chuyển sang dự phòng. Context của request đã hết hạn sẽ dừng các lần thử tiếp theo mà không phạt oan key hay proxy chưa dùng.

### Nhật ký và quản trị

| Trường                        | Mặc định / yêu cầu                                     |
| ----------------------------- | ------------------------------------------------------- |
| `logging.level`               | `info`; nhận `debug`, `info`, `warn`, `error`.          |
| `logging.ring_size`           | `2000`; khoảng 100–50.000.                              |
| `logging.dump_request_bodies` | `false`. Cần log mức `debug` để xuất body đi ra.        |
| `webui.enabled`               | `false` nếu bỏ qua; file mẫu đặt là `true`.            |
| `webui.listen`                | `0.0.0.0:8081`.                                          |
| `webui.username`              | Bắt buộc khi bật WebUI; file mẫu dùng `admin`.          |
| `webui.password`              | Mật khẩu khởi tạo, tối thiểu 10 ký tự.                  |
| `webui.password_hash`         | Hash Argon2id được sinh ra; thay thế mật khẩu văn bản thuần. |
| `webui.session_ttl_minutes`   | `720`; khoảng 5–10.080.                                 |

Khi khởi động, mật khẩu khởi tạo được băm và gỡ khỏi cấu hình. Bản sao lưu chứa bản văn bản thuần đó bị xoá. Hãy giữ file cấu hình và bản sao lưu ở chế độ riêng tư: upstream key và credential proxy vẫn là bí mật cấu hình cần thiết.

Body dump là tuỳ chọn và giới hạn 64 KiB cho mỗi body đã chuẩn bị. Bí mật đã cấu hình bị che, nhưng nội dung hội thoại vẫn có thể nhạy cảm.

## WebUI và chẩn đoán

Cổng quản trị phục vụ UI và các route `/api/*`. Việc xác thực dùng một tài khoản quản trị, session phía máy chủ, cookie HttpOnly/SameSite, kiểm tra CSRF cho thao tác ghi, và giới hạn số lần đăng nhập.

| Route quản trị                                 | Mục đích                                                        |
| ---------------------------------------------- | --------------------------------------------------------------- |
| `POST /api/auth/login`                        | Đăng nhập và nhận cookie session cùng CSRF token.                |
| `GET /api/auth/session`, `POST /api/auth/logout` | Kiểm tra hoặc kết thúc session.                               |
| `GET /api/config`, `PUT /api/config`          | Đọc cấu hình đã che hoặc áp dụng thay đổi.                      |
| `POST /api/config/reload`                     | Nạp lại file từ đĩa.                                            |
| `POST /api/config/reveal`                     | Hiện bí mật đã cấu hình sau khi xác minh mật khẩu.              |
| `PUT /api/account`                            | Cập nhật thông tin đăng nhập sau khi xác minh mật khẩu; vô hiệu hoá session. |
| `GET /api/monitor`                            | Thống kê request, token, upstream và tài nguyên.                |
| `GET /api/debug/models`                       | Định tuyến mô hình, vân tay key và chẩn đoán metadata.          |
| `POST /api/debug/inference`                   | Chạy một request Playground.                                     |
| `GET /api/logs`, `GET /api/logs/stream`       | Log gần đây hoặc đăng ký SSE trực tiếp.                         |

Request Playground cần session và `X-CSRF-Token`, giới hạn 12 lần mỗi phút cho mỗi IP client:

```json
{
  "protocol": "chat",
  "key": { "mode": "auto" },
  "request": {
    "model": "MODEL_ID",
    "messages": [{ "role": "user", "content": "Hello" }]
  }
}
```

Máy chủ ép `stream: false`. Dùng `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}` để thử một key cụ thể lấy từ `/api/debug/models`. Request chọn key tối đa chỉ thực hiện một lần thử upstream, không truy cập ẩn danh, không luân chuyển key, không dự phòng tier và không phát lại reasoning.

**Cả chẩn đoán tự động và chọn key đều giữ nguyên cooldown key sản xuất, số lần lỗi, sức khoẻ proxy và ràng buộc của chúng.** Chúng vẫn gửi request thật tới upstream, có thể tiêu tốn hạn mức của nhà cung cấp và xuất hiện trong giám sát.

Sau khi thực thi, endpoint quản trị trả HTTP 200 với kết quả thực tế trong `ok`, `http_status`, `request_id`, `route` và `response`. Kết quả chọn key còn có `selected_key` và `key_test`: `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error` hoặc `unavailable`. Dữ liệu đầu vào quản trị không hợp lệ, lỗi xác thực, CSRF và vượt giới hạn vẫn giữ mã lỗi HTTP riêng.

### Ngôn ngữ giao diện

WebUI tải bản dịch từ `webui/js/i18n.js`. Bản dịch có sẵn gồm **简体中文** (mặc định), **English** và **Tiếng Việt**. Ngôn ngữ gốc **简体中文** nằm ngay trong `index.html` và `webui/js/`, và luôn được dùng làm dự phòng cho mọi chuỗi không có bản dịch — vì vậy việc thêm ngôn ngữ không làm thay đổi bản gốc.

- Bộ chọn ngôn ngữ nằm ở góc phải thanh công cụ (và trên thẻ đăng nhập).
- Lựa chọn được lưu trong `localStorage` với khoá `opencode2api.ui.lang`.
- Đổi ngôn ngữ sẽ dịch lại cả văn bản tĩnh (`data-i18n`, `data-i18n-attr`) lẫn văn bản do JavaScript tạo ra, mà không cần tải lại trang.
- Định dạng ngày giờ cũng theo ngôn ngữ đang chọn.

Thêm một ngôn ngữ mới chỉ cần thêm một khối vào `DICTIONARY` trong `webui/js/i18n.js`, rồi khai báo mã trong `SUPPORTED`, `LABELS` và `DATE_LOCALES`.

### Lưu và nạp lại

Máy chủ kiểm tra một cấu hình ứng viên và dựng gateway thay thế trước khi lưu và chuyển đổi. Kiểm tra hoặc lưu thất bại sẽ giữ nguyên gateway đang hoạt động. Các request đang chạy vẫn tiếp tục trên gateway gốc của chúng.

Key, proxy, URL upstream, thiết lập retry, thiết lập mô hình, nhật ký và tuỳ chọn định tuyến có hiệu lực ngay với request mới. Thay đổi `listen`, `webui.listen` hoặc `webui.enabled` cần khởi động lại tiến trình. JSON đã lưu được chuẩn hoá; chú thích không được giữ lại.

## Giám sát và lưu trữ

Kết quả request mô tả lần suy luận đã hoàn tất. Một phản hồi SSE thất bại hoặc luồng bị cắt cụt vẫn được tính là lỗi kể cả khi HTTP 200 đã được gửi; kết quả của nó là `stream_error`. Việc client huỷ được ghi là `client_canceled`. WebUI hiển thị các kết quả đó bên cạnh mã trạng thái HTTP.

Bản ghi **lần thử** upstream đo trao đổi HTTP tính tới lúc nhận được header phản hồi. Thành công và độ trễ của nó tách biệt với việc hoàn tất toàn bộ body JSON hoặc luồng.

Thống kê token chỉ dùng usage được báo. Token input bao gồm đọc và ghi cache; token cache riêng đếm các lần đọc cache. Usage thiếu không được ước tính, và độ phủ cho biết bao nhiêu request suy luận đã định tuyến có báo usage. `usage.server_keys` tách request và token theo server key cục bộ mà caller đã xác thực (vân tay của nó, như hiển thị ở `/api/api-keys`); lưu lượng Playground được tính dưới mục `playground`.

| Dữ liệu                                                  | Lưu trữ / thời gian giữ                                        |
| -------------------------------------------------------- | -------------------------------------------------------------- |
| Session, số đo, tổng token, kết quả Playground gần nhất   | Bộ nhớ tiến trình; bị xoá khi khởi động lại.                   |
| Chi tiết request và lần thử                                | Một giờ gần nhất, tối đa 10.000 request và 20.000 lần thử; API trả tối đa 500 mục mỗi loại. |
| Ring log trực tiếp                                         | Bộ nhớ tiến trình; kích thước do `logging.ring_size` quy định. |
| Log có cấu trúc                                            | JSON trên stdout; thu thập bên ngoài để lưu lâu dài.          |
| Cấu hình và phiên bản trước                                 | `config.json` và `config.json.bak`.                             |
| Cache danh mục mô hình                                      | `config.json.models.catalog.json`.                             |
| Cache giá/ngừng cấp                                         | `config.json.models.dev.json`.                                 |

Tên file cache dựa trên đường dẫn cấu hình thực tế. Các bộ đếm tuổi thọ đều tính theo tuổi thọ tiến trình hiện tại. Các instance không chia sẻ session hay trạng thái giám sát.

### Health check

`/healthz` không làm tăng bộ đếm giám sát cũng không phát sinh request mạng. Nó báo cáo số lượng tài nguyên mà không kèm key hay địa chỉ proxy.

- HTTP 503: danh mục khởi tạo còn đang chờ, không có mô hình nào định tuyến được với cấu hình hiện tại, hoặc không có proxy khoẻ.
- HTTP 200: sẵn sàng, kể cả khi cache danh mục dùng được đã cũ. Trong trường hợp đó trạng thái mô hình là `stale` và trạng thái tổng thể là `degraded`.

Ngưỡng cũ là gấp đôi `models.refresh_seconds`, tối thiểu 60 giây. Khả năng sẵn sàng là một bước kiểm tra phát hiện và tài nguyên; nó không xác minh rằng upstream key sẽ chấp nhận lần suy luận tiếp theo.

## Phát triển

Mã nguồn Go được tổ chức thành các package theo trách nhiệm. Lệnh điều phối dịch vụ; các package triển khai nằm dưới `internal/`.

```text
cmd/
  opencode2api/main.go    Cờ CLI, khởi động và tắt êm
internal/
  admin/                 API quản trị, session đăng nhập và Playground
  buildinfo/             Phiên bản dùng chung cho health và endpoint quản trị
  config/                Cấu hình, lưu trữ, mật khẩu và che giấu
  gateway/               Định tuyến HTTP, retry, pool, làm mới và runtime
  httpx/                 Phản hồi HTTP dùng chung, xử lý body và header
  identity/              Request ID và session affinity
  jsonutil/              Trợ giúp truy cập và giải mã JSON
  models/                Danh mục mô hình, năng lực, giá và cache
  protocol/              Chuyển đổi request/response và phân tích/xuất SSE
  telemetry/             Theo dõi request, số đo, log và phục hồi
webui/
  embed.go               Nhúng các tài nguyên tĩnh vào file thực thi
  index.html             Cấu trúc trang
  css/                   Design token, kiểu cơ sở, thành phần và bố cục trang
  js/                    Module ES: vỏ giao diện, i18n, trợ giúp dùng chung và mỗi trang một module
```

Thứ tự đọc gợi ý:

1. [Lệnh vào](cmd/opencode2api/main.go) → [quản lý runtime](internal/gateway/runtime.go) → [handler HTTP](internal/gateway/gateway.go).
2. [Lần thử upstream](internal/gateway/upstream.go) và [định tuyến mô hình](internal/models/catalog.go) giải thích một request đến nhà cung cấp bằng cách nào.
3. [Chuyển đổi request](internal/protocol/request.go), [chuyển đổi phản hồi](internal/protocol/response.go) và [vận chuyển luồng](internal/protocol/stream.go) bao phủ hành vi giao thức.
4. [Route quản trị](internal/admin/server.go) và [logic WebUI](webui/js/main.js) bao phủ cấu hình và chẩn đoán.

Chạy định dạng Go, phân tích và build lệnh:

```bash
gofmt -w cmd internal webui
go vet ./...
go build -o opencode2api ./cmd/opencode2api
```

Để phát triển cục bộ, khởi động dịch vụ bằng `go run ./cmd/opencode2api -config config.json`. Bản build phát hành vẫn chèn phiên bản bằng `-ldflags "-X main.version=vX.Y.Z"`.

Node.js chỉ cần cho bước kiểm tra cú pháp JavaScript của WebUI:

```bash
for f in webui/js/*.js webui/js/pages/*.js; do node --input-type=module --check < "$f"; done
```

`.editorconfig`, `.gitattributes` và định dạng Go chuẩn hoá mã nguồn. CI chạy `go vet` và build với Go 1.24 và Go ở bản ổn định trên Linux/Windows, đồng thời kiểm tra định dạng Go, cú pháp WebUI và cú pháp shell của entrypoint. Gói phát hành gồm cả ba README.

## Khắc phục sự cố

| Triệu chứng                                 | Cần kiểm tra                                                                        |
| ------------------------------------------- | ----------------------------------------------------------------------------------- |
| API trả 401                                 | Dùng một giá trị `server_keys` cục bộ đã cấu hình.                                 |
| Health vẫn là `starting`                   | Xem log làm mới danh mục và kết nối đi ra; thêm ghi đè giao thức nếu cần.            |
| Không có mô hình nào được công khai          | Kiểm tra tier đã cấu hình, điều kiện ẩn danh và hỗ trợ giao thức gốc.               |
| Request trả 502/504                         | Xem các lần thử upstream, credential, proxy và timeout tổng/theo từng lần thử.       |
| HTTP 200 nhưng sinh nội dung thất bại        | Xem sự kiện lỗi SSE và kết quả request, không chỉ mã trạng thái HTTP.                |
| Sửa trên host không có tác dụng trong Docker | File đang hoạt động nằm trong volume trạng thái; import lại hoặc dùng WebUI.        |
| Không kết nối được cổng của container       | Kiểm tra địa chỉ lắng nghe, cổng đã publish và cấu hình trong volume đang dùng.    |
| Số liệu giám sát mất sau khi khởi động lại  | Giám sát chỉ nằm trong bộ nhớ; thu thập log stdout ra bên ngoài.                     |

## Ghi nhận

Cảm ơn cộng đồng [LINUX DO](https://linux.do) đã ủng hộ.

### Cường độ suy luận thích ứng

Đặt `reasoning.effort` (hoặc ghi đè theo mô hình) là `auto` để dùng cường độ cao nhất đã được bộ dò mô hình miễn phí xác minh thành công. Các bậc cụ thể sẵn có giữ nguyên ý nghĩa và cường độ do client chỉ định được ưu tiên. Không có danh sách cho phép theo tên mô hình nào được duy trì.

Sau khi một mức nền thành công, các mô hình có suy luận được dò theo thứ tự `max`, `xhigh`, `high`, `medium`, `low`, `minimal`. Chỉ khi có tham số suy luận bị từ chối tường minh mới chuyển xuống bậc thấp hơn. Lỗi tạm thời sẽ dừng tìm kiếm và không thể bị hiểu là trần năng lực thấp hơn. Nếu mọi bậc đều bị từ chối, không có ghi đè suy luận nào được chèn vào.

Các quan sát được lưu theo URL upstream, mô hình và giao thức gốc, hiển thị ở trang tính khả dụng, và chỉ dùng tối đa hai giờ. Lần dò đang hoạt động chỉ áp dụng cho mô hình Zen/ẩn danh miễn phí; mô hình Go/trả phí, năng lực chưa biết và bậc chưa xác minh không nhận ghi đè tự động. Đặt `auto` không mở rộng phạm vi dò và không bảo đảm upstream tuân thủ mọi trường được chấp nhận. Việc dò có thể cần vài request nhỏ cho mỗi mô hình và dùng hạn mức của upstream.

## Giấy phép

SPDX-License-Identifier: GPL-3.0-or-later

Dự án này là phần mềm tự do: bạn có thể phân phối lại và sửa đổi theo Giấy phép Công cộng GNU phiên bản 3 do Free Software Foundation công bố, hoặc phiên bản 3 hoặc mới hơn (tuỳ chọn của bạn). Phần mềm được cung cấp nguyên trạng, không kèm bảo đảm. Xem [LICENSE](LICENSE) để biết đầy đủ điều khoản.