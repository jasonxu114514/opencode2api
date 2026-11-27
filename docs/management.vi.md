# Hướng dẫn quản trị và vận hành

[English](management.md) | [简体中文](management.zh-CN.md) | Tiếng Việt | [Italiano](management.it.md)

Tài liệu này mô tả các chức năng quản trị hiện có, OAuth, hạn mức, luân chuyển mô hình, chẩn đoán và lưu trữ trạng thái. Cách khởi động dịch vụ và ví dụ API suy luận có trong [README tiếng Việt](../README.vi.md); cấu hình mẫu đầy đủ nằm ở [config.example.json](../config.example.json). Giao diện quản trị và API suy luận dùng địa chỉ lắng nghe riêng; mọi đường dẫn `/api/*` bên dưới đều thuộc cổng quản trị.

## Khởi động và truy cập

Trên Linux, dùng Go 1.24 trở lên và Bash để biên dịch trong thư mục dự án:

```bash
if [ ! -f config.json ]; then
  cp config.example.json config.json
fi
export GOCACHE="$PWD/.local/go-cache"
export GOMODCACHE="$PWD/.local/go-modules"
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Trước khi chạy, chỉnh cấu hình trên máy: thay `server_keys` và `webui.password`, rồi điền `zen_keys` hoặc `go_keys`. Khi chỉ quản trị cục bộ, nên dùng `webui.listen: "127.0.0.1:8081"`; giá trị mẫu `0.0.0.0:8081` lắng nghe trên mọi giao diện. Mật khẩu phải có ít nhất 10 ký tự; tên quản trị viên trong cấu hình mẫu là `admin`.

```bash
./opencode2api -config config.json
```

Trên Windows, biên dịch bằng `go build -trimpath -o opencode2api.exe ./cmd/opencode2api`, sau đó chạy `.\opencode2api.exe -config .\config.json` trong PowerShell. Hướng dẫn khởi tạo volume, nhập tệp, cổng và image cho Docker/Compose có trong README. Khi quản trị, phải dùng cấu hình đang chạy thực tế; chỉ sửa bản sao trên máy chủ mà chưa nhập lại sẽ không thay đổi cấu hình trong container.

Kênh ẩn danh `anonymous` mặc định tắt. Muốn khởi động, phải có ít nhất một key thượng nguồn, kết nối Go/Zen OAuth đã lưu, hoặc chủ động bật ẩn danh. Không thể khởi động khi mọi thông tin xác thực đều trống rồi vào WebUI để đăng nhập OAuth lần đầu. Thượng nguồn có thể chặn lưu lượng ẩn danh; không nên bật suy luận ẩn danh chỉ để kiểm tra proxy.

Thư mục cấu hình phải ghi được. Lần chạy đầu chuyển `webui.password` thành `password_hash` Argon2id và cố xóa bản sao lưu chứa mật khẩu khởi tạo dạng rõ; thao tác này không mã hóa các thông tin xác thực khác.

## Các trang và xác thực quản trị

| Trang | Địa chỉ | Chức năng |
| --- | --- | --- |
| Tổng quan | `/#overview` | Yêu cầu, token, lần thử thượng nguồn và tài nguyên đang chạy. |
| Chẩn đoán | `/#diagnostics` | Định tuyến mô hình, Playground và nhật ký trực tiếp. |
| Hướng dẫn kết nối | `/#access` | Thông số kết nối cho client. |
| Quản lý API key | `/#api-keys` | Vòng đời và mức sử dụng key cục bộ. |
| Mô hình và hạn mức | `/#model-quotas` | Danh mục Go, hạn mức dùng chung, hai loại OAuth và tính khả dụng của mô hình miễn phí. |
| Luân chuyển mô hình | `/#rotation` | Thứ tự nhóm, vị trí hiện tại, tạm dừng và lịch sử thử. |
| Trung tâm cấu hình | `/#config` | Key, proxy, thời gian chờ, định tuyến, WARP và nhật ký. |
| Bảo mật tài khoản | `/#account` | Đổi tên quản trị viên và mật khẩu. |

Giao diện hỗ trợ tiếng Trung giản thể, Anh, Việt và Ý; ngôn ngữ và giao diện được lưu trong trình duyệt. Các địa chỉ cũ `#playground`, `#logs` mở trang chẩn đoán; `#availability` mở trang mô hình và hạn mức.

Quản trị viên đăng nhập bằng tên và mật khẩu để nhận cookie phiên có `HttpOnly` và `SameSite=Strict`. Mọi API quản trị, trừ đăng nhập, đều cần phiên hợp lệ; thao tác ghi còn cần `X-CSRF-Token`. Nếu có header `Origin`, giá trị phải trùng với nguồn của dịch vụ. Phản hồi đăng nhập/phiên cung cấp `csrf_token`. `server_keys` cục bộ không thay thế được đăng nhập quản trị.

Phiên mặc định kéo dài 720 phút, cho phép cấu hình từ 5 đến 10.080 phút. Đổi thông tin xác thực quản trị hoặc khởi động lại tiến trình làm các phiên hiện có mất hiệu lực; API đổi tài khoản yêu cầu xác minh lại mật khẩu hiện tại. Đăng nhập có giới hạn tần suất; Playground cho phép tối đa 12 lần mỗi phút trên mỗi client kết nối trực tiếp. Dịch vụ không tin cậy `X-Forwarded-For`, nên các client sau reverse proxy dùng chung giới hạn của đầu kết nối; với `-trust-cloudflare-client-ip`, peer loopback (`cloudflared` cục bộ) được giới hạn theo `CF-Connecting-IP`, IPv6 theo /64. Reverse proxy HTTPS phải đặt đúng `X-Forwarded-Proto` vì header này tham gia xác định cờ Secure của cookie và kiểm tra nguồn.

## Lưu và nạp lại cấu hình

`GET /api/config` trả về cấu hình đang chạy, ID/mặt nạ an toàn, địa chỉ lắng nghe thực tế và những trường cần khởi động lại. Khi cập nhật key/proxy, dùng một trong hai dạng sau:

```json
{"id":"EXISTING_FINGERPRINT"}
```

```json
{"value":"NEW_VALUE"}
```

`PUT /api/config` nhận toàn bộ biểu mẫu cấu hình, không phải PATCH từng phần; danh sách key/proxy bị bỏ qua có thể được coi là danh sách rỗng. Muốn giữ giá trị hiện có, gửi ID của giá trị đó. Lưu bằng giao diện có thể giữ các cấu hình nâng cao chưa được sửa trong biểu mẫu. Thông tin xác thực OAuth, luân chuyển và mật khẩu quản trị được quản lý qua API riêng; thao tác lưu thông thường không nhận hoặc ghi đè token OAuth do trình duyệt gửi lên.

Thứ tự lưu là kiểm tra, tạo runtime ứng viên, ghi xuống đĩa rồi chuyển sang runtime mới. Nếu kiểm tra hoặc ghi cấu hình thất bại, runtime cũ tiếp tục phục vụ. Tệp cấu hình cho phép chú thích `//` và `/* ... */`, còn yêu cầu API phải là JSON chuẩn. Trường không xác định, trường cấp cao nhất bị lặp, giá trị không hợp lệ và việc dùng đồng thời trường OAuth cũ/mới cho cùng một gói đều bị từ chối. Sau khi lưu, tệp được chuẩn hóa và mất các chú thích.

Các thiết lập key, proxy, thượng nguồn, thời gian chờ, mô hình, ưu tiên, reasoning và nhật ký áp dụng ngay cho yêu cầu mới. `listen`, `webui.listen`, `webui.enabled` cần khởi động lại; giao diện hiển thị cả giá trị cấu hình lẫn địa chỉ lắng nghe thực tế. Sau khi sửa tệp thủ công, phải chọn “Nạp lại từ đĩa” hoặc gọi `POST /api/config/reload`; chương trình không tự theo dõi thay đổi tệp.

`POST /api/config/reveal` yêu cầu phiên quản trị, CSRF và xác minh lại mật khẩu; API có thể trả về giá trị key thủ công và proxy, nhưng không trả mật khẩu, hash mật khẩu hoặc token OAuth. Chỉ dùng khi thực sự cần sao chép, tránh ghi kết quả vào nhật ký hoặc chia sẻ.

Các giá trị mặc định quản trị thường dùng:

| Cấu hình | Mặc định / phạm vi |
| --- | --- |
| `webui.enabled` | Mặc định tích hợp là tắt; cấu hình mẫu bật. |
| `webui.listen` | `0.0.0.0:8081`. |
| `logging.level` | `info`; hỗ trợ `debug`, `info`, `warn`, `error`. |
| `logging.ring_size` | `2000`; phạm vi 100–50.000. |
| `logging.dump_request_bodies` | `false`; cần thêm mức debug để xuất, tối đa 64 KiB cho mỗi nội dung yêu cầu đã chuẩn bị. |
| `models.active_probes` | `false`; tính khả dụng mặc định được quan sát từ lưu lượng thực. |

## Go OAuth và Zen PAYG OAuth

Trang “Mô hình và hạn mức” có hai bảng độc lập. Go dùng định tuyến thuê bao; Zen tính phí theo mức sử dụng (PAYG) của workspace Console, **suy luận sẽ tiêu thụ credits của workspace đó**. Đăng nhập Go không cấp quyền hoặc số dư Zen và không tự kết nối PAYG.

Quy trình:

1. Bắt đầu đăng nhập trong bảng tương ứng; với Zen, phải xác nhận rõ việc dùng PAYG trước.
2. Tự mở liên kết xác minh OpenCode Device OAuth chính thức, nhập `user_code` hiển thị và xác nhận đây là yêu cầu cấp quyền do mình khởi tạo. Trang không tự chuyển hướng.
3. Backend thăm dò theo khoảng thời gian được trả về. Nếu có nhiều tổ chức, quá trình tự hoàn tất tạm dừng để chọn tổ chức sở hữu thuê bao/credits rồi kết nối tổ chức đó; chỉ được chọn tổ chức mà lần cấp quyền này cho phép.
4. Khi thành công, làm mới cấu hình và danh sách mô hình. Nếu thất bại hoặc hết hạn, có thể hủy rồi bắt đầu lại.

Không cần dán token, đọc tệp xác thực trên máy hoặc gửi URL callback. Rời trang hoặc ẩn trình duyệt sẽ dừng thăm dò tự động; khi quay lại, trang đọc lại trạng thái và có thể tiếp tục lần đăng nhập chưa hết hạn. Hai loại đăng nhập chờ được tách theo phiên quản trị và gói; không thể dùng ID đăng nhập của gói này cho gói khác. Hủy chỉ xóa lần đăng nhập chờ tương ứng; đăng xuất phiên quản trị hủy các lần đăng nhập chờ của phiên đó nhưng giữ các kết nối đã lưu.

| Thao tác | API Go | API Zen | Nội dung yêu cầu |
| --- | --- | --- | --- |
| Trạng thái an toàn | `GET /api/go/auth` | `GET /api/zen/auth` | Không có. |
| Bắt đầu đăng nhập | `POST /api/go/auth/start` | `POST /api/zen/auth/start` | Go dùng `{}`; Zen dùng `{"acknowledge_payg":true}`. |
| Thăm dò/chọn tổ chức | `POST /api/go/auth/poll` | `POST /api/zen/auth/poll` | `{"id":"LOGIN_ID"}`; thêm `organization_id` khi chọn tổ chức. |
| Hủy đăng nhập chờ | `DELETE /api/go/auth` | `DELETE /api/zen/auth` | Không có. |
| Ngắt kết nối đã lưu | `DELETE /api/go/connection` | `DELETE /api/zen/connection` | Không có. |

Tất cả API đều cần phiên; thao tác ghi cần CSRF. Phản hồi đăng nhập, trạng thái, cấu hình và reveal đều không trả thông tin xác thực truy cập/làm mới OAuth.

Hai kết nối được lưu riêng trong `go_oauth` và `zen_oauth`; backend làm mới và lưu khi cần. Vì vậy, cấu hình và bản sao lưu chứa thông tin xác thực truy cập/làm mới nhạy cảm và cần giới hạn quyền đọc. `go_subscription`/`zen_subscription` cũ chỉ là bí danh để chuyển đổi khi nạp; khi lưu sẽ dùng trường mới. Dùng đồng thời trường cũ và mới cho cùng một gói sẽ bị từ chối.

Giới hạn định tuyến:

- Chỉ chấp nhận mô hình do cấu hình từ xa chính thức đã xác thực cung cấp, vượt qua kiểm tra host/đường dẫn của gói tương ứng và SDK được hỗ trợ. Không xóa `/go` khỏi URL Go để đoán PAYG, cũng không chấp nhận header xác thực từ xa tùy ý. Định tuyến `/zen` cũ chỉ được chấp nhận khi cấu hình từ xa cung cấp rõ ràng.
- Go và Zen có thông tin xác thực, nguồn mô hình và quy trình làm mới độc lập. `prefer` quyết định lựa chọn ưu tiên khi cùng một mô hình có nhiều đường đi; chọn `zen` cho phép Zen OAuth đã kết nối tiêu thụ credits.
- Sau khi chọn OAuth, chỉ dùng tier đó. Go mất hiệu lực, hết hạn mức, nhận 429 hoặc gặp lỗi khác sẽ không tự chuyển sang Zen PAYG, key thủ công hay kênh ẩn danh.
- `go_keys`/`zen_keys` thủ công vẫn được giữ nhưng không làm dự phòng khi OAuth tương ứng tồn tại, kể cả khi cần đăng nhập lại hoặc không có định tuyến khả dụng.
- Ngắt kết nối chỉ xóa kết nối cục bộ tương ứng, không thu hồi phiên chính thức và không ảnh hưởng kết nối còn lại. Sau đó, gateway khám phá lại mô hình thủ công của tier đó và từ chối đoán định tuyến khi quá trình chưa xong. Khi ẩn danh tắt, không thể ngắt thông tin xác thực thượng nguồn duy nhất còn lại; cần cấu hình key/kết nối khác trước.
- Dung lượng thuê bao OAuth và credits hiện chưa biết; không dùng lại hạn mức của key thủ công hoặc suy ra từ số mô hình hay yêu cầu thành công.

Triển khai tham chiếu các snapshot mã nguồn cố định: Go dùng [v2 `4617210822bdbb31e5749751afbfb4584ccedac8`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8) và [dev `7745079647b65036fe142e09ccd8623b25be1878`](https://github.com/anomalyco/opencode/tree/7745079647b65036fe142e09ccd8623b25be1878); Console/PAYG dùng [dev `03d848f7b8d71d31d3e56f2abd8b337efbc1bc32`](https://github.com/anomalyco/opencode/tree/03d848f7b8d71d31d3e56f2abd8b337efbc1bc32). Tương thích mã nguồn không có nghĩa đã xác minh cấp quyền và suy luận thực tế. Khi quyền, workspace hoặc định dạng cấu hình thượng nguồn thay đổi, có thể cần đăng nhập lại hoặc cập nhật triển khai.

## Mô hình, hạn mức và tính khả dụng

`GET /api/model-quotas` trả về mô hình Go và tóm tắt hạn mức; danh sách mô hình lấy theo danh mục gateway. Với key Go thủ công, gateway truy vấn `upstream.go` đã cấu hình cộng `/v1/usage`, mặc định là `https://opencode.ai/zen/go/v1/usage`; trình duyệt không nhận key thượng nguồn.

- Cung cấp các cửa sổ phần trăm dùng chung trong 5 giờ cuốn chiếu, theo tuần và theo tháng cùng thời điểm đặt lại; không cung cấp số dư từng mô hình, số tiền hoặc số lần dùng còn lại.
- Nhiều key có thể dùng chung gói, nên không được cộng các giá trị. Độ chính xác phần trăm do thượng nguồn quyết định.
- Lưu cache thành công 60 giây, thất bại 15 giây. Khi thất bại, giữ giá trị thành công gần nhất và đánh dấu là lịch sử; nếu không có lịch sử thì hiển thị chưa biết.
- Trang truy vấn mỗi 60 giây khi đang hiển thị; dừng truy vấn định kỳ khi bị ẩn hoặc rời trang. Làm mới thủ công cũng chịu giới hạn cache.
- Khi có Go OAuth, không mượn dữ liệu của key Go thủ công; hạn mức OAuth và credits Zen vẫn là chưa biết.

Định dạng hạn mức dựa trên [mã nguồn usage thượng nguồn](https://github.com/anomalyco/opencode/blob/dev/packages/console/app/src/routes/zen/go/v1/usage.ts). Đây không phải cam kết về một API công khai ổn định. Nếu định dạng thay đổi, trang báo chưa biết/bất thường và không tự tạo số dư.

Tính khả dụng miễn phí mặc định lấy từ yêu cầu thực. Chỉ lỗi rõ ràng rằng mô hình không tồn tại/đã bị gỡ mới có thể vô hiệu hóa mô hình. Riêng 401, 403, 429, 4xx/5xx thông thường, hết thời gian chờ, lỗi kết nối và phản hồi sai định dạng không chứng minh mô hình bị gỡ. Khôi phục thủ công giữ mô hình có thể định tuyến và đưa mô hình ra khỏi thăm dò tự động; chỉ lập lịch lại sau khi bật lại điều khiển tự động.

Chỉ `models.active_probes: true` mới bật suy luận tổng hợp ở nền. Bộ lập lịch kiểm tra các mục đến hạn mỗi phút, lần lượt theo mô hình và kênh. Mô hình bình thường/chưa thể kết luận thường được kiểm tra lại sau một giờ; mô hình đã xác nhận không khả dụng sau 24 giờ. Mỗi kênh có thời gian chờ 60 giây. Yêu cầu đi qua kênh ẩn danh đã bật trước, sau đó thử key Zen khả dụng; nếu bật WARP thì dùng lối ra WARP. Không có kênh phù hợp sẽ bỏ qua. Thăm dò chủ động tiêu thụ hạn mức thực nhưng không đổi thời gian chờ phục hồi của key/proxy sản xuất hoặc thống kê yêu cầu.

| API quản trị | Mục đích |
| --- | --- |
| `GET /api/models/availability` | Đọc trạng thái mô hình miễn phí và kết quả thăm dò reasoning. |
| `POST /api/models/probe` | Đánh dấu mô hình điều khiển tự động là đến hạn và đánh thức bộ lập lịch; không bỏ qua công tắc `active_probes`. |
| `POST /api/models/restore` | `{"model":"MODEL_ID"}`: khôi phục và chuyển sang điều khiển thủ công. |
| `POST /api/models/manual` | `{"model":"MODEL_ID","manual":true}`; `false` bật lại điều khiển tự động. |

`reasoning.effort: "auto"` dùng mức reasoning cao nhất của Zen miễn phí/ẩn danh đã được xác minh thành công gần đây. Khi thăm dò chủ động tắt, gateway không chủ động học mức trần mới; điều khiển thủ công cũng tạm dừng thăm dò tự động. Kết quả quan sát hết hiệu lực sau hai giờ. Không suy ra mức cho Go/mô hình trả phí hoặc mô hình chưa biết năng lực; effort do client chỉ định rõ luôn được ưu tiên.

## API key cục bộ

Đối tượng quản lý là `server_keys` của gateway, tách biệt với key thượng nguồn OpenCode. API tạo key trả toàn bộ key mới trong phản hồi; danh sách chỉ trả dấu vân tay, tên, mặt nạ, trạng thái, thời gian và mức sử dụng. Giao diện hỗ trợ sao chép key và biến môi trường cho client sau khi tạo.

| API | Yêu cầu và kết quả |
| --- | --- |
| `GET /api/api-keys` | Danh sách an toàn. |
| `POST /api/api-keys` | `{"name":"client-a"}` tạo tự động; có thể thêm `value` để tùy chọn giá trị; thành công trả 201. |
| `PATCH /api/api-keys/{id}` | `{"name":"client-b","enabled":false}`; chỉ cần gửi các trường muốn đổi. |
| `DELETE /api/api-keys/{id}` | Xóa key cục bộ. |

Key tự động dùng 192 bit ngẫu nhiên. Key tùy chọn dài 16–256 ký tự, gồm chữ cái, chữ số, `.`, `_`, `-`; tên dài 1–80 ký tự. API tạo cho phép tối đa 200 key. Phải giữ ít nhất một key đang bật. Tắt/xóa khiến yêu cầu mới nhận 401 ngay; yêu cầu đang xử lý có thể đã xác thực xong.

`server_key_metadata` lưu tên, trạng thái bật và thời điểm tạo theo dấu vân tay, tự tương thích với `server_keys` cũ. Mức sử dụng gồm một giờ gần nhất và tổng của tiến trình hiện tại; yêu cầu và token được phân loại theo key đã xác thực thành công. Playground được ghi riêng là `playground`, không tính vào key cục bộ mượn để xác thực. Thống kê giữ qua nạp lại nóng và xóa khi khởi động lại; không cung cấp hạn mức riêng, quyền mô hình hoặc ngày hết hạn cho từng key.

## Proxy và WARP

Trung tâm cấu hình hỗ trợ nhập hàng loạt proxy nội tuyến, kiểm tra từng mục và đánh dấu xóa trong bản nháp. Nội dung tệp proxy `proxyfile` cần được quản lý tại tệp nguồn; đường dẫn tương đối tính từ thư mục cấu hình. Xóa ở giao diện chỉ là bản nháp và có hiệu lực sau khi lưu toàn bộ cấu hình.

`POST /api/proxies/check` yêu cầu phiên và CSRF; chỉ được chọn một trong hai dạng yêu cầu sau:

```json
{"id":"SAVED_PROXY_ID"}
```

```json
{"value":"https://proxy.example:8443"}
```

ID của proxy đã lưu lấy từ `GET /api/config`; `value` dùng cho bản nháp. Mỗi lần kiểm tra có thời gian chờ 20 giây. Giao diện cho tối đa bốn kiểm tra đồng thời, máy chủ tối đa tám; vượt giới hạn trả 429, không xếp hàng. Khi hoàn tất bình thường, phản hồi HTTP 200 có `ok`, `state`, `reason`, `latency_ms` và `status` nếu có. Lỗi xác thực, CSRF và định dạng dùng trạng thái HTTP tương ứng.

Kiểm tra chỉ xác minh proxy được chọn có kết nối được đến Zen thượng nguồn đã cấu hình hay không: client riêng gửi GET không xác thực qua proxy tới `upstream.zen` cộng `/v1/models`. Không gửi key Zen/Go, `public` hoặc OAuth; không suy luận, không theo redirect, không dùng WARP và không thay đổi trạng thái khỏe, thời gian chờ phục hồi hay liên kết của key trong lưu lượng sản xuất. Vẫn dùng được khi ẩn danh tắt.

| `state` | Ý nghĩa |
| --- | --- |
| `available` | Thượng nguồn trả 200–399 hoặc 401; `authentication_required` của 401 chỉ chứng minh đã tới đích, chưa xác minh key. |
| `unavailable` | Lần kiểm tra này xác nhận phía proxy từ chối kết nối/DNS không tồn tại (`proxy_unreachable`), hoặc đường hầm proxy trả 407 (`proxy_authentication_required`). |
| `limited` | 429; không chứng minh proxy bị hỏng. |
| `rejected` | Từ chối khác từ thượng nguồn hoặc đường hầm proxy; 407 từ thượng nguồn không đồng nghĩa xác thực proxy thất bại. |
| `inconclusive` | Hết thời gian chờ, lỗi truyền tải không xác định được nguồn hoặc thiếu điều kiện; không thể kết luận proxy bị hỏng. |

Khi dọn hàng loạt, chỉ nên coi `unavailable` trong lần kiểm tra này là xác nhận không khả dụng. Không coi mọi trường hợp giới hạn tần suất, từ chối hay hết thời gian chờ là hỏng vĩnh viễn. Suy luận thực vẫn có thể chịu giới hạn của mô hình, xác thực, hạn mức và chính sách.

WARP mặc định tắt. Khi bật, yêu cầu miễn phí ẩn danh/Zen thủ công và các truy vấn danh mục/thăm dò tương ứng dùng đường hầm trong tiến trình; Go, mô hình trả phí và OAuth vẫn đi qua proxy. Cần UDP `2408` tới Cloudflare, không cần TUN hoặc sửa bảng định tuyến. `warp.pool_size` là giới hạn 1–32 lối ra đồng thời, mặc định 2; `warp.stack` mặc định `ipv6`, cũng hỗ trợ `ipv4`/`dual`. Khởi tạo thất bại sẽ từ chối khởi động hoặc cấu hình ứng viên, không âm thầm chuyển sang proxy.

403/429 ở lối ra có thể kích hoạt loại bỏ và bổ sung lối ra, nhưng thử lại vẫn chịu giới hạn thời gian/số lần. Đổi lối ra không bảo đảm có thêm hạn mức. Lối ra IPv6 `/64`, IPv4 `/32` được chống trùng và chờ phục hồi theo dải mạng. `warp.source_ipv6` yêu cầu stack IPv6 và prefix `/64` hoặc ngắn hơn. Linux dùng `IPV6_FREEBIND`; Windows phải được gán sẵn địa chỉ nguồn đã chọn. Thiết lập này không cấu hình mạng của máy. `/healthz` chỉ cung cấp trạng thái/số lượng WARP; `resources.warp` trong `/api/monitor` còn có thông tin lối ra.

## Luân chuyển SOTA / Sweet

Client tiếp tục dùng API Base URL, key và ba API thông điệp hiện tại, chỉ đổi `model` thành bí danh `sota` hoặc `sweet`. Có thể đổi bí danh; khi đủ điều kiện, bí danh xuất hiện trong `/v1/models`, còn lệnh gọi mô hình thực vẫn tương thích. Luân chuyển chỉ dùng Go, qua key thủ công hoặc Go OAuth; không chuyển dự phòng sang PAYG giữa các tier. System One không tham gia luân chuyển.

Các thành viên SOTA cố định là `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, `qwen3.8-max`. Sweet gồm các mô hình thông điệp Go khả dụng còn lại, ban đầu ưu tiên `glm-5.3-flash`, sau đó sắp theo tên và thêm mô hình mới ở cuối. Thành viên đã bị gỡ vẫn nằm trong danh sách nhưng không chọn được. Tính khả dụng thực lấy từ danh mục và định tuyến; tên cố định không bảo đảm gọi được.

Mỗi nhóm dùng chung một vị trí hiện tại, giữ nguyên sau thành công và chuyển tiếp sau thất bại. Số thế hệ ngăn lỗi từ yêu cầu đồng thời cũ ghi đè vị trí mới; chọn thủ công không ngắt câu trả lời đang chạy. Một yêu cầu không ghé lại cùng mô hình.

- Lỗi thông thường được thử tối đa ba lần mỗi mô hình, với ngân sách tối đa ba mô hình gặp lỗi thông thường; mô hình đã dùng hết số lần được tạm dừng 15 giây.
- 429/lỗi hạn mức rõ ràng chuyển ngay sang mô hình tiếp theo và tạm dừng theo `Retry-After`, mặc định 60 giây. Trường hợp này không tăng số mô hình tính vào ngân sách lỗi thông thường; lỗi thông thường đã xảy ra vẫn được tính.
- Không dùng cơ chế thử lại key thông thường giữa các tier hoặc gửi lại để sửa stale reasoning; lỗi của một mô hình không tạm dừng cả key Go. Hạn mức dùng chung có thể khiến toàn nhóm không dùng được.
- Tổng thời gian giới hạn bởi `retry.timeout_seconds`. `rotation.first_output_seconds` giới hạn mỗi lần đợi đầu ra có hiệu lực, mặc định 30 giây, phạm vi 1–300 giây. Với phản hồi không streaming, phải đọc xong phản hồi hợp lệ trong thời gian này.
- Các sự kiện mở đầu của stream được giữ lại đến khi có đầu ra thực/kết thúc bình thường mới gửi cho client. Trước đó có thể thử lại; sau khi đã gửi, lỗi giữ nguyên nội dung đã có, gửi lỗi SSE của giao thức rồi kết thúc. Không ghép câu trả lời thay thế vào một stream. Client hủy không kích hoạt chuyển mô hình hoặc tạm dừng.
- `model` trong phản hồi giữ bí danh được yêu cầu; `X-Resolved-Model` chỉ mô hình thực, token được tính cho mô hình thực. Bản ghi giữ nguyên nhân đã phân loại, không lưu nội dung yêu cầu, key hay nội dung lỗi thượng nguồn.

| API quản trị | Nội dung |
| --- | --- |
| `GET /api/rotation` | Thứ tự/bí danh hai nhóm, mô hình hiện tại, thời gian tạm dừng, trạng thái lưu trữ, 100 lần thử gần nhất và 30 lần chuyển của mỗi nhóm. |
| `PUT /api/rotation` | Gửi toàn bộ `alias`, `order` của `sota`/`sweet` cùng `first_output_seconds`; chỉ được sắp lại trong nhóm, không bỏ thành viên. |
| `POST /api/rotation/current` | `{"group":"sota","model":"glm-5.3"}`: chuyển mô hình và xóa thời gian tạm dừng của mô hình đó. |

Bí danh dài 1–64 ký tự gồm chữ cái, chữ số, `.`, `_`, `-`; ký tự đầu phải là chữ cái/chữ số. Bí danh không được trùng nhau hoặc xung đột với mô hình thực. Nếu danh mục thay đổi khiến thành viên gửi lên không khớp, hãy làm mới rồi sắp xếp lại.

Cấu hình nằm trong `rotation`, trạng thái runtime ở `config.json.rotation.json`. Các lần nạp lại nóng dùng chung trạng thái. Nếu không thể đọc/ghi trạng thái lúc khởi động đầu tiên, dịch vụ từ chối khởi động; lỗi ghi về sau được báo trên trang quản trị.

## Playground, nhật ký và giám sát

Playground gọi cùng gateway trong tiến trình qua `POST /api/debug/inference` và bắt buộc dùng phản hồi không streaming. Ví dụ định tuyến tự động:

```json
{"protocol":"chat","key":{"mode":"auto"},"request":{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}}
```

Muốn chỉ định key, thay `key` bằng `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}`; ID lấy từ `GET /api/debug/models`. Key được chọn chỉ thực hiện một lần thử thượng nguồn, không ẩn danh, không đổi key, không chuyển tier và không phát lại reasoning. Có thể thêm `relay` với chỉ mục đã biết để chọn proxy, ví dụ `{"index":0}`. Chỉ mục phải lấy từ danh mục chẩn đoán hiện tại.

Chẩn đoán không thay đổi thời gian chờ phục hồi, số lỗi, trạng thái khỏe hoặc liên kết của key/proxy sản xuất, nhưng vẫn gửi yêu cầu thực, tiêu thụ hạn mức/credits và được ghi vào giám sát. Khi lớp bao phản hồi quản trị thành công, API trả HTTP 200; `ok`, `http_status`, `request_id`, `route`, `response` biểu thị kết quả thực. Với key được chọn còn có `selected_key`, `key_test`, chẳng hạn `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error`, `unavailable`. Lỗi định dạng/xác thực/giới hạn tần suất vẫn dùng trạng thái HTTP tương ứng.

Nhật ký có tại `GET /api/logs?after=SEQUENCE&limit=COUNT` và `GET /api/logs/stream`. SSE hỗ trợ nối tiếp bằng `Last-Event-ID` hoặc `after`; sự kiện hết hạn/bị mất được báo bằng `gap`, và dữ liệu đã loại khỏi bộ đệm không thể khôi phục. Máy chủ kết thúc stream nhật ký khi phiên mất hiệu lực.

`GET /api/monitor` cung cấp tổng hợp yêu cầu, token, lần thử thượng nguồn và tài nguyên. `GET /api/debug/models` cung cấp định tuyến, dấu vân tay key, metadata mô hình, proxy và kết quả chẩn đoán gần nhất. Token chỉ dùng số thượng nguồn báo cáo, không ước tính phần thiếu; đầu vào gồm cả cache đọc/ghi, còn cached ghi riêng cache đọc. `usage.server_keys` nhóm theo key cục bộ và ghi riêng `playground`.

Kết quả yêu cầu phản ánh toàn bộ quá trình suy luận: stream thất bại vẫn được ghi `stream_error` dù HTTP là 200; bên gọi hủy được ghi `client_canceled`. Lần thử thượng nguồn chỉ đo trao đổi đến lúc nhận header phản hồi, nên không thể coi tỷ lệ thành công và độ trễ đó là tỷ lệ thành công của toàn bộ câu trả lời.

## Lưu trữ, trạng thái khỏe và bảo mật

| Dữ liệu | Vị trí và vòng đời |
| --- | --- |
| Cấu hình/phiên bản trước | `config.json`, `config.json.bak`, có thông tin nhạy cảm. |
| Cache mô hình/giá | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Tính khả dụng miễn phí | `config.json.<dấu vân tay thượng nguồn Zen>.availability.json`, tách theo thượng nguồn. |
| Trạng thái luân chuyển | `config.json.rotation.json`. |
| Phiên, token tích lũy, bộ đệm nhật ký vòng, Playground gần đây | Bộ nhớ tiến trình; xóa khi khởi động lại. |
| Chi tiết yêu cầu/lần thử | Một giờ gần nhất, tối đa 10.000/20.000 bản ghi; mỗi API trả tối đa 500 bản ghi. |
| Nhật ký dài hạn | JSON trên stdout; cần tự thu thập và lưu giữ an toàn. |

Tiền tố tệp phụ thuộc đường dẫn `-config` thực tế. Nhiều tiến trình không dùng chung phiên, giám sát hay bộ đếm trong bộ nhớ; tránh để chúng đồng thời sửa cùng một tệp trạng thái.

`GET /healthz` nằm trên cổng suy luận, không cần xác thực và không kích hoạt kiểm tra thượng nguồn hoặc đếm giám sát. API trả 503 khi danh mục chưa sẵn sàng/rỗng, WARP không có lối ra hoạt động hoặc proxy bắt buộc không có nút khỏe. Danh mục còn dùng được nhưng quá hạn có thể trả 200 với `degraded`; ngưỡng quá hạn là hai lần khoảng làm mới và ít nhất 60 giây. Nhu cầu đăng nhập lại OAuth xuất hiện trong issues, nhưng trạng thái sẵn sàng không bảo đảm lần suy luận tiếp theo thành công. Thông tin công khai chỉ gồm trạng thái và số lượng, không lộ key/địa chỉ proxy.

Khi dùng cục bộ, nên giới hạn ở loopback; quản trị qua mạng cần HTTPS và giới hạn truy cập. Chương trình không tự xử lý kết thúc TLS. Cấu hình/bản sao lưu chứa key thủ công, mật khẩu proxy và token OAuth; cần hạn chế bằng quyền tệp hoặc Windows ACL và loại khỏi phần mã nguồn chia sẻ. Nội dung yêu cầu debug đã che bí mật và cắt ngắn vẫn có thể chứa hội thoại riêng tư, nên chỉ bật khi cần trong thời gian ngắn. Không dùng cấu hình đầy đủ, kết quả reveal, cookie, header xác thực hoặc nội dung suy luận thực làm tệp đính kèm để tìm lỗi.

## Kiểm tra và bảo trì

Kiểm tra khi phát triển cục bộ:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Nghiệm thu triển khai thông thường cần xác minh đăng nhập quản trị và CSRF, tắt/khôi phục key, lưu cấu hình và khởi động lại để đổi địa chỉ lắng nghe, khám phá mô hình, ba giao thức thông điệp và lỗi SSE, trạng thái luân chuyển, cùng các chức năng OAuth/thượng nguồn thực cần dùng. Suy luận thực, OAuth, WARP và truy vấn hạn mức có thể phụ thuộc quyền tài khoản và tiêu thụ hạn mức; kiểm tra tĩnh không thay thế các bước này.

Trước khi nâng cấp, sao lưu an toàn chương trình, cấu hình thực tế và trạng thái, dừng tiến trình rồi thay chương trình. Quay về phiên bản cũ cần khôi phục cấu hình và trạng thái tương thích. Người dùng Compose cần lưu đúng các tệp thực trong volume. Không thêm thông tin xác thực, nhật ký, trạng thái runtime hoặc sản phẩm xác minh cục bộ vào mã nguồn.
