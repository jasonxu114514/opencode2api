# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md)

Gateway viết bằng Go cho OpenCode Zen và Zen Go. Client dùng OpenAI Chat Completions, OpenAI Responses hoặc Anthropic Messages; gateway chọn upstream khả dụng và chuyển đổi yêu cầu, phản hồi khi giao thức gốc khác nhau. Quyết định System One dùng endpoint chuyển tiếp riêng.

WebUI quản trị được nhúng trong tệp thực thi, không cần Node.js hay cơ sở dữ liệu khi chạy. Khám phá mô hình, suy luận, OAuth và WARP tùy chọn vẫn cần kết nối đến dịch vụ upstream tương ứng.

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
2. Điền Zen API Key (khuyến nghị) vào `zen_keys` hoặc Go key vào `go_keys` trước khi khởi động. Cấu hình mẫu tắt truy cập ẩn danh (`anonymous: false`) và để trống cả hai mảng key; kết nối Go OAuth (`go_oauth`) hoặc Zen OAuth (`zen_oauth`) đã lưu cũng đáp ứng yêu cầu này. Không hỗ trợ khởi động khi chưa có thông tin xác thực chỉ để đăng nhập OAuth lần đầu trong WebUI. Làn ẩn danh có thể bị chặn mạnh ở thượng nguồn, nên ưu tiên Zen API Key.
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
| `-trust-cloudflare-client-ip` | `false` | Giới hạn đăng nhập/Playground của WebUI dùng `CF-Connecting-IP`, chỉ khi peer trực tiếp là loopback (`cloudflared` cục bộ). |

Thư mục cấu hình phải ghi được để thực hiện việc chuyển mật khẩu, lưu cấu hình và cache mô hình.

## Docker và Compose

Image phát hành là `ghcr.io/jasonxu114514/opencode2api`. Chuẩn bị `config.json` trước khi chạy. Để dùng cổng xuất bản, listener API/WebUI bên trong phải là `0.0.0.0:8080`, `0.0.0.0:8081`; giới hạn truy cập bằng địa chỉ bind phía host và HTTPS khi cần.

```bash
docker compose up -d
docker compose logs -f
```

Compose nạp cấu hình host vào volume `opencode2api-state` **chỉ ở lần khởi động đầu**. Sau đó cấu hình hoạt động nằm trong volume; nên sửa qua WebUI. Để nhập lại tệp host đã sửa:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Sao lưu cấu hình/trạng thái đang dùng trước khi thay. Entrypoint chạy dịch vụ bằng người dùng không đặc quyền và chỉ đổi đúng giá trị API mẫu `127.0.0.1:8080` thành `0.0.0.0:8080` lúc tạo cấu hình ban đầu. Compose có filesystem gốc chỉ đọc, volume trạng thái ghi được và tmpfs `/tmp`. Cổng mặc định bind mọi giao diện host; chỉ dùng cục bộ thì đổi `ports` thành `127.0.0.1:8080:8080`, `127.0.0.1:8081:8081`.

| Biến Compose | Mặc định | Tác dụng |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | Tag image; ghim tag phát hành để cố định phiên bản. |
| `OPENCODE2API_PORT` | `8080` | Cổng API phía host. |
| `OPENCODE2API_WEBUI_PORT` | `8081` | Cổng quản trị phía host. |
| `OPENCODE2API_LISTEN` | Chưa đặt | Ghi đè listener API trong container. |
| `OPENCODE2API_WEBUI_LISTEN` | Chưa đặt | Ghi đè listener quản trị trong container. |

Hai biến cuối thành `LISTEN_ADDRESS`, `WEBUI_LISTEN_ADDRESS` trong container; rỗng thì theo cấu hình hoạt động. Đổi cổng host không đổi listener bên trong. Nếu đổi cổng API bên trong, cập nhật mapping và health check image vốn dùng 8080. `proxyfile` phải được mount tại đường dẫn container đã cấu hình.

Để dùng image tự biên dịch:

```bash
docker build -t opencode2api:local .
docker volume create opencode2api-state
docker run -d --name opencode2api \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$PWD/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

WARP cần UDP ra Cloudflare cổng `2408`, không cần host networking hay quyền mạng bổ sung. Nếu mạng chặn lưu lượng này, WARP không thể khởi tạo.

## Gọi API

Đây là các tuyến tương thích hiện có. Với client tương thích OpenAI, đặt URL gốc là `http://127.0.0.1:8080/v1`; quản trị dùng listener riêng.

| Phương thức | Đường dẫn | Hành vi |
| --- | --- | --- |
| `GET` | `/v1/models` | Mô hình có thể định tuyến với cấu hình hiện tại và bí danh luân chuyển đủ điều kiện. |
| `POST` | `/v1/chat/completions` | Chat Completions, JSON hoặc SSE. |
| `POST` | `/v1/responses` | Responses, JSON hoặc SSE. |
| `POST` | `/v1/messages` | Anthropic Messages, JSON hoặc SSE. |
| `POST` | `/v1/systemone` | Quyết định System One gốc, không chuyển đổi. |
| `GET` | `/healthz` | Mức sẵn sàng và tóm tắt tài nguyên, không cần xác thực. |

Mọi tuyến `/v1/*` yêu cầu giá trị `server_keys` đang bật qua `Authorization: Bearer ...` hoặc `x-api-key`. Giới hạn nội dung yêu cầu là 32 MiB. Phản hồi suy luận có `x-request-id` để đối chiếu log.

Lấy ID mô hình thực tế trước khi gửi yêu cầu:

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Xin chào"}]}'
```

Với Responses và Anthropic Messages, lần lượt gửi nội dung JSON chuẩn sau đến endpoint tương ứng:

```json
{"model":"MODEL_ID","input":"Xin chào"}
```

```json
{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Xin chào"}]}
```

Client Anthropic cũng có thể gửi `anthropic-version: 2023-06-01`. Để dùng SSE, thêm `"stream": true`, dùng `curl -N` hoặc client đọc sự kiện ngay khi nhận. Stream có thể lỗi sau HTTP 200; cần xem sự kiện lỗi và kết quả cuối.

System One cần mô hình có `native_protocol: "systemone"` và nội dung quyết định như:

```json
{"model":"SYSTEM_ONE_MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}
```

Nội dung và câu trả lời được chuyển tiếp nguyên dạng. Endpoint tin nhắn cũng nhận cấu trúc quyết định này cho mô hình System One, nhưng không chuyển tin nhắn chat thông thường thành quyết định. Stream System One từ upstream được chuyển tiếp, không có cơ chế khôi phục bằng reset stream.

### Giới hạn tương thích

Ba giao thức tin nhắn hỗ trợ văn bản, ảnh/tệp biểu diễn được ở giao thức đích, suy luận, hàm, lời gọi/kết quả công cụ và streaming. Yêu cầu dùng giao thức gốc giữ các trường riêng của nhà cung cấp nếu gateway không chủ động chỉnh dạng. Chuyển đổi chỉ giữ những trường có biểu diễn tương ứng; ví dụ `seed` và ràng buộc JSON của Chat không được đổi thành tùy chọn Anthropic. Nội dung hoặc công cụ không hỗ trợ có thể bị từ chối.

Khi đổi lịch sử Responses, suy luận, văn bản và lời gọi hàm của assistant được nhóm thành lượt. Tin nhắn user và kết quả công cụ ngăn cách lượt; kết quả thiếu hoặc không khớp vẫn là lỗi khi đổi sang Chat Completions.

Gateway không triển khai embeddings, tải tệp lên, sinh ảnh, truy xuất/hủy Responses. Nó không lưu lịch sử hội thoại cho client. Hãy gửi lịch sử trong yêu cầu; tính năng có trạng thái của upstream không có nghĩa gateway lưu phiên cục bộ.

## Cấu hình và định tuyến

Bắt đầu từ [config.example.json](config.example.json). Tệp cho phép chú thích `//` và `/* ... */`; nội dung API phải là JSON chuẩn. Trường không biết, giá trị sai, trường cấp cao nhất lặp lại và xung đột tên OAuth cũ/mới đều bị từ chối. Khi lưu, cấu hình được chuẩn hóa và bỏ chú thích.

| Thiết lập | Mặc định / ý nghĩa |
| --- | --- |
| `server_keys` | Phải giữ ít nhất một key cục bộ đang bật. |
| `zen_keys`, `go_keys` | Hai pool key upstream độc lập; cấu hình mẫu để trống. |
| `prefer` | `go`; cũng nhận `zen`, chọn tier xác thực ưu tiên trong các tier khả dụng. |
| `anonymous` | `false`; bật thông tin xác thực upstream `public` cho mô hình miễn phí đủ điều kiện. |
| `upstream.zen` | `https://opencode.ai/zen` cho key Zen thủ công. |
| `upstream.go` | `https://opencode.ai/zen/go` cho key Go thủ công và endpoint hạn mức. |
| `upstream.anonymous` | `https://opencode.ai/inference` cho lưu lượng ẩn danh. |
| `models.refresh_seconds` | `300`; chu kỳ khám phá mô hình. |
| `models.protocols` | Ghi đè theo mô hình: `chat`, `responses`, `anthropic`, `systemone`. |
| `models.free_only` | `false`; chỉ liệt kê và định tuyến mô hình miễn phí. |
| `models.active_probes` | `false`; yêu cầu suy luận thử cho mô hình miễn phí, tùy chọn. |
| `reasoning.effort` | Chưa đặt trong mặc định nội bộ; mẫu dùng `high`. |
| `reasoning.effort_by_model` | Ghi đè mức suy luận theo mô hình. |

### Mô hình và chọn upstream

Khám phá kết hợp danh sách upstream với khả năng và giá OpenCode tại `https://models.opencode.ai/api.json`. Giao thức gốc được lưu riêng theo tier. `/v1/models` cung cấp giới hạn ngữ cảnh/token đã biết, giá, `native_protocol`, `supported_endpoints` và thông tin suy luận. Giá và trạng thái ngừng hỗ trợ cập nhật mỗi 24 giờ; lỗi cập nhật giữ dữ liệu trước đó. Giá không phải số dư hay dự toán hóa đơn.

Với key thủ công, mô hình miễn phí đủ điều kiện có thể thử kênh ẩn danh trước nếu chủ động bật, sau đó các tier có key theo `prefer`. Khi tắt ẩn danh, dùng key đã cấu hình. Điều kiện miễn phí là ID chứa `free` không phân biệt hoa thường, hoặc metadata chưa ngừng hỗ trợ có giá input/output bằng không; điều này không bảo đảm upstream chấp nhận. Mô hình trả phí cần tuyến xác thực phù hợp. `models.free_only` cũng lọc thành viên nhóm luân chuyển.

OAuth có quy tắc riêng: yêu cầu đã chọn tier OAuth chỉ dùng tuyến được xác nhận của tier đó, không chuyển sang ẩn danh, key thủ công hay tier khác sau lỗi. Go OAuth không kích hoạt PAYG khi lỗi xác thực, hết hạn mức hoặc 429. Nếu cả hai tier có mô hình, chủ động chọn `prefer: "zen"` cho phép dùng kết nối Zen PAYG.

### Timeout, proxy và gắn kết phiên

| Thiết lập | Mặc định | Ý nghĩa |
| --- | --- | --- |
| `retry.max_attempts` | `3` | Ngân sách vòng thử với key; ẩn danh và luân chuyển có quy tắc riêng. |
| `retry.timeout_seconds` | `300` | Ngân sách toàn yêu cầu, kể cả đọc stream. |
| `performance.attempt_timeout_seconds` | `0` | Giới hạn ngắn hơn cho từng lần thử; zero giữ timeout toàn yêu cầu. |
| `performance.first_event_timeout_seconds` | `0` | Thời gian đợi sự kiện đầu, tùy chọn; zero tắt. |
| `performance.connect_timeout_seconds` | `5` | Timeout thiết lập kết nối. |
| `performance.failure_cooldown_seconds` | `15` | Thời gian nghỉ cơ sở sau lỗi. |
| `performance.max_anonymous_exits` | Hiệu lực `8` | Số lối ra tĩnh riêng biệt cho một yêu cầu ẩn danh; zero dùng giới hạn nội bộ. |
| `performance.free_tier_block_seconds` | Hiệu lực `900` | Thời gian nghỉ khi upstream chặn lối ra miễn phí; zero dùng giá trị nội bộ. |
| `performance.max_idle_conns` | `2048` | Kết nối nhàn rỗi cho mỗi transport. |
| `performance.max_idle_conns_per_host` | `256` | Kết nối nhàn rỗi theo host/transport. |
| `performance.max_conns_per_host` | `0` | Giới hạn kết nối; zero là không giới hạn. |
| `performance.idle_conn_timeout_seconds` | `120` | Tuổi thọ kết nối nhàn rỗi. |

`proxies` nhận `direct`, HTTP, HTTPS, SOCKS5 và SOCKS5H. `proxyfile` thêm một proxy trên mỗi dòng không trống, đường dẫn tương đối với thư mục cấu hình; loại trùng và dùng trực tiếp nếu danh sách cuối trống. Chú thích bắt đầu bằng `#`, `;`, `//` ở đầu dòng hoặc sau khoảng trắng. Tài khoản/mật khẩu trong URL proxy là bí mật.

Với endpoint do nhà cung cấp tự xoay lối ra, dùng `pool+https://proxy.example:8443` hoặc `https://proxy.example:8443#pool`. Mỗi lần thử mở kết nối mới, không tái sử dụng keep-alive/HTTP/2, không áp dụng thời gian nghỉ hay loại theo sức khỏe như nút tĩnh. Nhà cung cấp phải thực hiện việc xoay. Proxy tĩnh giữ tái sử dụng kết nối, theo dõi sức khỏe và gắn key. `proxy_bindings` gắn dấu vân tay key với proxy đã cấu hình.

Gửi `x-session-id` để tách hội thoại rõ ràng. Header phiên OpenCode và các trường hội thoại được hỗ trợ cũng có hiệu lực; nếu thiếu, nội dung system/user ban đầu làm hạt giống. ID tự tạo được giới hạn theo key cục bộ và vòng đời tiến trình. ID chính thức OpenCode hợp lệ được giữ nguyên. Dùng ID riêng cho hội thoại độc lập và key riêng để cách ly người dùng: các hội thoại cùng key có mở đầu giống nhau có thể chia sẻ gắn kết.

Đối tượng tùy chọn `models.anonymous_backoff` tạm dừng cả kênh ẩn danh sau nhiều lỗi: ngưỡng mặc định `6`, thời gian cơ sở `1` giờ, tối đa `24` giờ. Không đặt đối tượng này sẽ tắt bộ ngắt cấp kênh.

### Chỉnh dạng yêu cầu miễn phí và suy luận

Tương thích mô hình miễn phí có thể chèn prompt/công cụ chính thức OpenCode, đổi tên công cụ trên đường truyền và khôi phục tên trong phản hồi. Mô hình miễn phí dùng key cũng chịu xử lý này. Gateway chỉ chuyển tiếp lời gọi, không chạy công cụ trên máy. System One không đi qua chỉnh dạng tin nhắn/công cụ.

Khi chỉnh dạng, khoảng trắng ASCII trong mô tả cấp cao nhất của công cụ client mặc định được thay bằng khoảng trắng không ngắt dòng (`U+00A0`, NBSP). Phép chuyển đổi này không sửa schema, mô tả tham số hay định nghĩa công cụ chính thức được thêm vào. Khoảng trắng trong ví dụ mã nằm trong mô tả cũng thay đổi; quy tắc so khớp của upstream có thể thay đổi theo thời gian.

Prompt chèn thêm có mô hình và ngày UTC; thư mục, workspace, nền tảng hoặc trạng thái Git chưa biết của client được ghi là `unknown`. Gateway không thay bằng môi trường của máy chủ. Để cung cấp rõ các thông tin này, thêm trường `client_environment` dành riêng cho gateway ở cấp cao nhất của mỗi yêu cầu suy luận:

```json
{"client_environment":{"working_directory":"C:\\work\\project","workspace_root":"C:\\work\\project","platform":"windows","is_git_repo":true}}
```

Gateway đọc rồi xóa trường mở rộng này trước khi chuyển tiếp. Gateway cũng nhận diện khối `<env>` đúng định dạng trong chỉ dẫn system/developer và bản cập nhật `<environment_context>` trong tin nhắn user riêng; bản cập nhật user cuối cùng được ưu tiên. Khi chèn prompt, thứ tự nguồn là `client_environment` rõ ràng, khai báo client được nhận diện, cấu hình `upstream.prompt_environment`, rồi đến trạng thái chưa biết. Mỗi nguồn cung cấp một bản chụp đầy đủ: trường thiếu không được bổ sung từ nguồn có ưu tiên thấp hơn, và thư mục làm việc không đồng nghĩa với thư mục gốc workspace. Bản chụp chỉ thuộc yêu cầu hiện tại, không được lưu đệm giữa các yêu cầu hay phiên. Gửi môi trường trong mỗi yêu cầu, trực tiếp hoặc qua lịch sử hội thoại.

Đường dẫn chỉ được kiểm tra như văn bản, không truy cập hệ thống tệp. `windows` thành `win32`; cũng chấp nhận `linux`, `darwin`. Để đặt giá trị mặc định cho bản triển khai, đưa bốn trường bên trong của ví dụ vào `upstream.prompt_environment`, sửa và nạp lại tệp cấu hình; UI chưa có trường riêng cho các giá trị nâng cao này. Prompt chính thức client đã gửi vẫn được giữ nguyên. Các giá trị môi trường `unknown` đã được chấp nhận trong yêu cầu thực tế tới `deepseek-v4.1-flash-free`; các mô hình và kênh khác vẫn chưa được xác minh.

Mức suy luận nhận `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `none`, `auto` hoặc rỗng. Mức client chỉ định rõ có ưu tiên. `none` bỏ thiết lập suy luận. `auto` dùng mức trần mới được xác minh cho mô hình miễn phí Zen/ẩn danh, không mở rộng phạm vi thử. Khám phá tự động cần bật thử chủ động, có thể dùng nhiều yêu cầu tiêu hao hạn mức và hết hiệu lực sau hai giờ. Không suy đoán mức cho Go, mô hình trả phí hay khả năng chưa xác minh.

Mặc định học khả dụng từ lưu lượng thật. Yêu cầu thử tổng hợp chỉ chạy khi `models.active_probes: true`; lệnh thử trong quản trị chỉ lên lịch, không bỏ qua công tắc. Chỉ bằng chứng rõ ràng mô hình không tồn tại/đã nghỉ mới có thể tắt mô hình. Giới hạn tốc độ, lỗi xác thực, lỗi chung và timeout không chứng minh việc ngừng cung cấp. Khôi phục thủ công giữ mô hình bật và loại khỏi thử tự động cho đến khi chuyển lại điều khiển tự động.

### WARP

`warp.enabled` mặc định `false`. Khi bật, lưu lượng miễn phí ẩn danh/Zen thủ công cùng đường khám phá/thử dùng tunnel Cloudflare WARP trong tiến trình. Go, mô hình trả phí và OAuth vẫn dùng proxy đã cấu hình. Cần UDP đi ra đến Cloudflare cổng `2408`; không cần TUN hay đổi bảng định tuyến. Lỗi khởi tạo khiến việc khởi động hoặc cấu hình mới bị từ chối.

`warp.pool_size` là trần số lối ra điều chỉnh theo đồng thời, từ 1 đến 32, mặc định 2. `warp.stack` nhận `ipv6` (mặc định), `ipv4`, `dual`. Thay lối ra không bảo đảm có hạn mức mới. `warp.source_ipv6` là tùy chọn, cần stack IPv6 và prefix `/64` hoặc ngắn hơn; trên Windows, địa chỉ nguồn được chọn phải được gán sẵn cho giao diện. Gateway không cấu hình mạng của máy.

## Quản trị và OAuth

WebUI có tổng quan, chẩn đoán/Playground/log, hướng dẫn client, key API, mô hình/hạn mức, luân chuyển, cấu hình và tài khoản. Hỗ trợ tiếng Trung giản thể, Anh, Việt và Ý; lựa chọn ngôn ngữ/giao diện lưu trong trình duyệt. Chi tiết endpoint có trong [hướng dẫn quản trị](docs/management.vi.md).

Quản trị dùng cookie phiên, token CSRF cho thao tác ghi và giới hạn đăng nhập. Đổi thông tin quản trị làm phiên mất hiệu lực. Key API cục bộ không cấp quyền quản trị. `GET /api/config` che bí mật; xác minh mật khẩu lại có thể hiện key thủ công/proxy nhưng không bao giờ trả token OAuth.

Từ **Mô hình và hạn mức**, bắt đầu Device OAuth. Tự mở URL xác minh chính thức, nhập `user_code`, chấp thuận yêu cầu do mình khởi tạo và chọn tổ chức/workspace nếu được hỏi. Backend xử lý thăm dò và làm mới; không dùng dán token, nhập tệp xác thực cục bộ hay gửi URL callback.

| Kết nối | Phạm vi và giới hạn |
| --- | --- |
| Go OAuth | Chỉ tuyến gói Go chính thức; hạn mức hiển thị chưa biết. |
| Zen OAuth | Console PAYG tiêu hao credits workspace; phải xác nhận rõ trước khi bắt đầu, số dư chưa biết. |

Go và Zen tách riêng phiên đăng nhập chờ, thông tin xác thực, làm mới và ngắt kết nối. Ẩn/rời trang dừng thăm dò; quay lại có thể tiếp tục đăng nhập chưa hết hạn. Đăng xuất quản trị hủy đăng nhập chờ của phiên nhưng giữ kết nối hoàn tất. Hủy đăng nhập chờ không ngắt tài khoản đã lưu.

`go_oauth` và `zen_oauth` trong cấu hình chứa thông tin truy cập/làm mới: phải bảo vệ cấu hình và bản sao lưu. Key thủ công còn được giữ nhưng không làm dự phòng khi OAuth tương ứng tồn tại, kể cả khi cần đăng nhập lại. Ngắt chỉ xóa kết nối cục bộ, phục hồi tuyến thủ công sau khám phá, không thu hồi phiên chính thức. Giữ thông tin xác thực khác trước khi ngắt kết nối cuối với ẩn danh đang tắt.

Chỉ nhận tuyến từ cấu hình từ xa chính thức đã xác thực, vượt kiểm tra URL/SDK theo từng gói. Không suy ra URL PAYG từ Go hay nhập header từ xa tùy ý. Lưu cấu hình thông thường giữ thông tin OAuth do backend quản lý. Trường cũ `go_subscription`/`zen_subscription` được chuyển khi nạp; kết hợp tên cũ/mới cho cùng gói sẽ bị từ chối.

Triển khai dựa trên ảnh chụp mã nguồn upstream liệt kê trong hướng dẫn. Đây là mô tả mã, không phải xác minh đăng nhập/suy luận thật cho mọi tài khoản; quyền và định dạng tuyến có thể đổi.

### Key cục bộ, hạn mức và luân chuyển

Trang key hỗ trợ tạo, đặt tên, bật, tắt, xóa. Key sinh tự động có 192 bit ngẫu nhiên. Key tự đặt dài 16–256 ký tự chữ, số, `.`, `_`, `-`; tên dài 1–80 ký tự, tối đa 200 key, ít nhất một key bật. Thay đổi áp dụng ngay cho yêu cầu mới. Chưa có quyền mô hình, trần chi tiêu hay ngày hết hạn riêng theo key.

Hạn mức key Go thủ công lấy từ `/v1/usage` trên upstream Go đã cấu hình: phần trăm cửa sổ dùng chung theo chu kỳ trượt/tuần/tháng, không phải số dư từng mô hình. Nhiều key có thể dùng chung gói, không cộng phần trăm. Thành công cache 60 giây, lỗi 15 giây; dữ liệu trước đó được đánh dấu cũ. Hạn mức/credits OAuth chưa biết và không suy từ key thủ công hay yêu cầu thành công.

Bí danh `sota`, `sweet` dùng mô hình Go tại ba endpoint tin nhắn, kể cả streaming. Mỗi nhóm có mô hình hiện tại dùng chung toàn dịch vụ: giữ sau thành công, tiến khi lỗi. SOTA gồm `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, `qwen3.8-max`; Sweet thu thập các mô hình Go phù hợp còn lại. Khả dụng thực tế theo kết quả khám phá. Sửa bí danh, thứ tự, timeout đầu ra đầu tiên tại `/#rotation`.

Ngân sách lỗi thường là ba mô hình, tối đa ba lần mỗi mô hình. Hết hạn mức/giới hạn tốc độ bỏ qua ngay; luân chuyển không tạo thêm hạn mức dùng chung. Khi nội dung stream đã gửi, lỗi kết thúc câu trả lời và tiến nhóm cho yêu cầu tiếp theo. `model` giữ bí danh, `X-Resolved-Model` chỉ mô hình thật. Trạng thái lưu cạnh cấu hình.

## Vận hành và bảo mật

Lưu cấu hình kiểm tra và tạo runtime thay thế trước khi áp dụng. Lỗi kiểm tra/lưu giữ runtime hiện tại. Key, proxy, tuyến, mô hình và log áp dụng cho yêu cầu mới; listener và bật/tắt WebUI cần khởi động lại. Sau khi sửa tệp, nạp lại qua UI hoặc `POST /api/config/reload`; không tự theo dõi tệp.

| Dữ liệu | Nơi lưu |
| --- | --- |
| Cấu hình và phiên bản trước | `config.json`, `config.json.bak`. |
| Cache mô hình/metadata | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Khả dụng và luân chuyển | `config.json.<upstream-fingerprint>.availability.json`, `config.json.rotation.json`. |
| Phiên, sử dụng, yêu cầu/lần thử gần đây, log vòng, Playground gần nhất | Bộ nhớ; xóa khi tiến trình khởi động lại. |
| Log cấu trúc | JSON trên stdout; cần bộ thu để lưu lâu dài. |

Tên tệp theo đường dẫn cấu hình thực tế. Theo dõi giữ qua nạp nóng nhưng không chia sẻ giữa tiến trình. Token chỉ lấy số upstream báo, không ước tính phần thiếu. Kết quả toàn yêu cầu và lần thử đến khi nhận header là hai thước đo khác nhau. Playground có nhóm sử dụng riêng. Một giờ gần nhất giữ tối đa 10.000 yêu cầu/20.000 lần thử, API trả tối đa 500 mỗi loại.

`GET /healthz` không gọi upstream, không cần key. Khám phá chưa xong/trống, WARP không dùng được hoặc thiếu proxy khỏe bắt buộc trả 503. Danh mục cũ nhưng dùng được có thể trả 200 với `status: "degraded"`; ngưỡng cũ là hai lần chu kỳ, ít nhất 60 giây. Sẵn sàng không chứng minh key hợp lệ, đủ hạn mức hay suy luận thành công. Phản hồi công khai chỉ có số lượng/trạng thái, không có bí mật hay địa chỉ proxy.

Dùng loopback cho API và quản trị cục bộ. Khi truy cập qua mạng, dùng HTTPS và giới hạn lối vào quản trị. Reverse proxy tin cậy phải đặt `X-Forwarded-Proto` đúng; giới hạn dùng địa chỉ peer trực tiếp, nên client cùng proxy chia sẻ giới hạn. Khi dùng đường hầm `cloudflared` trên cùng máy, thêm `-trust-cloudflare-client-ip` để mỗi khách có giới hạn riêng: `CF-Connecting-IP` chỉ được dùng cho peer loopback (Cloudflare ghi đè header này), IPv6 gộp theo /64. Không bật cho reverse proxy khác. Tệp thực thi phục vụ HTTP.

Cấu hình/bản sao lưu chứa key upstream, mật khẩu proxy, token OAuth; hash mật khẩu không mã hóa chúng. Hạn chế quyền tệp, dùng ACL trên Windows, và loại tệp runtime khi chia sẻ mã. `logging.dump_request_bodies` mặc định tắt và cần mức debug; nội dung đã cắt/che vẫn có thể lộ hội thoại. Playground và thử chủ động có thể tiêu hao hạn mức hoặc credits thật.

## Phát triển và xác minh cục bộ

`cmd/opencode2api` khởi động dịch vụ. Trong `internal`, `admin` quản trị, `gateway` định tuyến/runtime, `protocol` chuyển JSON/SSE, `models` khám phá, `oauth` xác thực chính thức, `config` kiểm tra/lưu, `telemetry` số liệu/log. Tài nguyên tĩnh ở `webui`, không cần cài gói frontend.

Trong môi trường phát triển thông thường, dùng các lệnh Go sau. Muốn giữ cache trong dự án, đặt `GOCACHE`, `GOMODCACHE` vào thư mục cục bộ bền vững:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Node.js chỉ cần cho kiểm tra cú pháp JavaScript tùy chọn, không cài gói. Chạy Bash, hoặc Git Bash trên Windows:

```bash
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Chạy các lệnh bằng Bash; trên Windows có thể dùng Git Bash. Xem các bước kiểm tra trước khi triển khai trong [hướng dẫn quản trị](docs/management.vi.md#kiểm-tra-và-bảo-trì). Có thể chèn phiên bản phát hành bằng `-ldflags "-X main.version=vX.Y.Z"`.

| Hiện tượng | Nội dung cần xem |
| --- | --- |
| API 401 | Key cục bộ, trạng thái bật và header client. |
| Không khởi động do thiếu xác thực | Cấu hình key/OAuth đã lưu hoặc chủ động bật ẩn danh. |
| Không có mô hình / health 503 | Danh mục, giao thức hỗ trợ, xác thực cấu hình và tài nguyên. |
| OAuth lỗi | Trạng thái tài khoản an toàn và yêu cầu đăng nhập lại; không tự chuyển PAYG. |
| HTTP 200 nhưng sinh thất bại | Sự kiện SSE, kết quả yêu cầu và log `x-request-id`. |
| Thay đổi không có hiệu lực | Lưu/nạp tệp đang dùng; khởi động lại khi đổi listener. |
| Sửa tệp host không đổi Compose | Cấu hình hoạt động ở volume trạng thái; nhập lại hoặc dùng WebUI. |
| Sử dụng mất sau khởi động lại | Bộ đếm ở bộ nhớ; lưu log riêng. |

Trước nâng cấp, sao lưu an toàn chương trình, cấu hình và trạng thái. Dừng tiến trình trước khi thay tệp thực thi; quay lui cần khôi phục bộ phiên bản tương thích của cả ba.

## Ghi nhận

Cảm ơn cộng đồng [LINUX DO](https://linux.do) đã ủng hộ.

## Giấy phép

SPDX-License-Identifier: GPL-3.0-or-later. Xem đầy đủ tại [LICENSE](LICENSE). Phần mềm không kèm bảo hành.
