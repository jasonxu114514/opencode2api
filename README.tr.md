# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md) | [Türkçe](README.tr.md)

OpenCode Zen ve Zen Go için Go ile yazılmış bir ağ geçidi. İstemciler OpenAI Chat Completions, OpenAI Responses veya Anthropic Messages kullanır; ağ geçidi uygun bir üst akış (upstream) seçer ve yerel protokol farklı olduğunda isteği ve yanıtı dönüştürür. System One kararları için ayrı bir doğrudan aktarım (pass-through) uç noktası bulunur.

Çalıştırılabilir dosya yönetim WebUI'ını içinde barındırır. Çalıştırmak için ne Node.js ne de bir veritabanı gerekir. Model keşfi, çıkarım (inference), OAuth ve isteğe bağlı WARP yine de ilgili üst akış servislerine erişim gerektirir.

## Hızlı başlangıç

[GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases) sayfasından hazır ikili dosyayı indirin veya **Go 1.24 ya da üzeri** ile derleyin:

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

Başlatmadan önce `config.json` dosyasını düzenleyin:

1. `server_keys` değerini kendi yerel API anahtarınızla değiştirin.
2. `zen_keys` içine bir Zen API Anahtarı (önerilen) veya `go_keys` içine bir Go anahtarı ekleyin. Örnek dosya her iki diziyi boş bırakır ve anonim erişimi kapatır; bu yüzden başlatmadan önce bir üst akış kimlik bilgisi yapılandırın. Önceden kaydedilmiş bir Go OAuth (`go_oauth`) veya Zen OAuth (`zen_oauth`) bağlantısı da bu gereksinimi karşılar; yalnızca OAuth WebUI'ını açmak için boş kimlik bilgileriyle başlatmak desteklenmez.
3. `webui.password` değerini değiştirin. Örnek yapılandırma WebUI'ı `admin` kullanıcı adıyla etkinleştirir.

**Anonim trafik üst akışta ağır biçimde engellenebilir. Zen API Anahtarı kullanmayı tercih edin.** Anonim erişim yapılandırmada ve WebUI'da varsayılan olarak kapalıdır; açıkça `anonymous: true` ayarlı mevcut yapılandırmalar bu seçimi korur. Yalnızca ücretsiz isteklerin yine de başarısız olabileceğini kabul ediyorsanız bilinçli olarak etkinleştirin.

```bash
./opencode2api -config config.json
```

Windows'ta `Copy-Item config.example.json config.json` komutunu kullanın, `go build -o opencode2api.exe ./cmd/opencode2api` ile derleyin ve `.\opencode2api.exe -config config.json` ile çalıştırın.

Örnek yapılandırma API için `127.0.0.1:8080`, WebUI için `0.0.0.0:8081` adresini dinler. Yerelde `http://localhost:8081` adresini açın. Ağ üzerinden erişirken yönetim erişimini kısıtlayın ve bir HTTPS ters vekil sunucu (reverse proxy) kullanın.

Komut satırı seçenekleri:

| Seçenek       | Varsayılan    | Amaç                                |
| ------------- | ------------- | ----------------------------------- |
| `-config`     | `config.json` | Yapılandırma dosyası yolu.          |
| `-listen`     | Ayarlı değil  | API dinleme adresini geçersiz kılar. |
| `-web-listen` | Ayarlı değil  | WebUI dinleme adresini geçersiz kılar. |

Parola geçişi, yapılandırma kayıtları ve model önbellekleri için yapılandırma dizini yazılabilir olmalıdır.

## Docker ve Compose

Yayınlanan imaj `ghcr.io/jasonxu114514/opencode2api` adresindedir. Başlatmadan önce `config.json` dosyasını yukarıdaki gibi hazırlayın. Yayınlanan kapsayıcı portları için iç API/WebUI dinleyicilerini `0.0.0.0:8080` ve `0.0.0.0:8081` üzerinde tutun; gerektiğinde ana makine port bağlamaları ve HTTPS ile erişimi kısıtlayın.

```bash
docker compose up -d
docker compose logs -f
```

Compose, `opencode2api-state` birimini ana makine yapılandırmasından **yalnızca ilk başlangıçta** besler. Sonrasında etkin yapılandırma bu birimde yaşar; değişiklikler için WebUI'ı kullanın. Düzenlenmiş bir ana makine dosyasını tekrar içe aktarmak için:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Üzerine yazmadan önce etkin yapılandırmayı ve durumu yedekleyin. Giriş noktası servisi ayrıcalıksız bir kullanıcı olarak çalıştırır ve yalnızca ilk besleme sırasında `127.0.0.1:8080` örnek API dinleyicisini `0.0.0.0:8080` olarak değiştirir. Compose salt okunur kök dosya sistemi, yazılabilir durum birimi ve `/tmp` tmpfs kullanır. Varsayılan yayınlanan portlar tüm ana makine arayüzlerine bağlanır; yalnızca yerel erişim için `ports` ayarını `127.0.0.1:8080:8080` ve `127.0.0.1:8081:8081` olarak düzenleyin.

| Compose değişkeni | Varsayılan | Etki |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | İmaj etiketi; sabit sürüm için bir sürüm etiketi belirtin. |
| `OPENCODE2API_PORT` | `8080` | Yayınlanan ana makine API portu. |
| `OPENCODE2API_WEBUI_PORT` | `8081` | Yayınlanan ana makine yönetim portu. |
| `OPENCODE2API_LISTEN` | Ayarlı değil | Kapsayıcı API dinleyicisi için açık geçersiz kılma. |
| `OPENCODE2API_WEBUI_LISTEN` | Ayarlı değil | Kapsayıcı yönetim dinleyicisi için açık geçersiz kılma. |

Son ikisi kapsayıcı içinde `LISTEN_ADDRESS` ve `WEBUI_LISTEN_ADDRESS` olur. Boş değerler etkin yapılandırmaya bırakılır. Ana makine portlarını değiştirmek iç dinleyicileri değiştirmez. İç API portu değişirse hem port eşlemesini hem de 8080 kullanan imaj sağlık kontrolünü güncelleyin. Bir `proxyfile` yapılandırılan kapsayıcı yoluna bağlanmalıdır.

Gerekirse yerel bir imaj derleyip çalıştırın:

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

WARP, Cloudflare `2408` portuna giden UDP trafiğine ihtiyaç duyar; imaj host ağı veya ek ağ yetkileri gerektirmez. Bu trafiği engelleyen bir ağ WARP başlatmasını önler.

## API'yi çağırma

Mevcut uyumluluk rotaları aşağıdadır. OpenAI uyumlu bir istemcinin temel URL'sini `http://127.0.0.1:8080/v1` olarak ayarlayın; yönetim ayrı dinleyicisini kullanır.

| Yöntem | Yol | Davranış |
| --- | --- | --- |
| `GET` | `/v1/models` | Uygun döndürme takma adları dahil mevcut yapılandırmayla yönlendirilebilen modeller. |
| `POST` | `/v1/chat/completions` | Chat Completions, JSON veya SSE. |
| `POST` | `/v1/responses` | Responses, JSON veya SSE. |
| `POST` | `/v1/messages` | Anthropic Messages, JSON veya SSE. |
| `POST` | `/v1/systemone` | Protokol çevirisi olmadan yerel System One kararları. |
| `GET` | `/healthz` | Kimlik doğrulamasız hazırlık ve kaynak özeti. |

Tüm `/v1/*` rotaları `Authorization: Bearer ...` veya `x-api-key` içinde etkin bir `server_keys` değeri gerektirir. İstekler 32 MiB ile sınırlıdır. Çıkarım yanıtları günlük eşleştirmesi için `x-request-id` taşır.

İstek yapmadan önce gerçek model kimliklerini keşfedin:

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}'
```

İlgili uç noktalarla şu standart JSON gövdelerini kullanın:

```json
{"model":"MODEL_ID","input":"Hello"}
```

```json
{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}
```

İlk gövde Responses, ikincisi Anthropic Messages içindir. Anthropic istemcileri ayrıca `anthropic-version: 2023-06-01` gönderebilir. SSE için `"stream": true` ekleyin ve `curl -N` ya da olayları geldikçe tüketen bir istemci kullanın. Bir akış HTTP 200 sonrasında başarısız olabilir; hata olaylarını ve nihai sonucunu inceleyin.

System One, `native_protocol` değeri `systemone` olan bir model ve şöyle bir karar gövdesi gerektirir:

```json
{"model":"SYSTEM_ONE_MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}
```

Karar gövdesini ve yanıtı dönüştürmeden iletir. Mesaj uç noktaları da bir System One modeli için bu karar biçimini tanır; sıradan sohbet mesajları kararlara dönüştürülemez. Üst akış System One akışı, akış sıfırlama kurtarması olmadan aktarılır.

### Uyumluluk sınırları

Üç mesaj protokolü metni, temsil edilebilir görsel/dosya içeriğini, akıl yürütmeyi, fonksiyon tanımlarını, araç çağrılarını, araç sonuçlarını ve akışı destekler. Yerel istekler, ağ geçidinin bilinçli olarak şekillendirmediği sağlayıcıya özgü alanları korur. Protokoller arası dönüştürme yalnızca desteklenen bir karşılığı olan alanları taşır; örneğin Chat `seed` ve JSON çıktı kısıtları Anthropic seçeneklerine çevrilmez. Desteklenmeyen içerik veya araçlar doğrulama hatası üretebilir.

Responses asistan akıl yürütme, metin ve fonksiyon çağrısı öğeleri dönüştürülürken asistan turları halinde gruplanır. Kullanıcı mesajları ve araç sonuçları turları ayırır; eksik veya eşleşmeyen araç sonuçları Chat Completions'a dönüştürülürken hata olarak kalır.

Ağ geçidi gömme (embeddings), dosya yükleme, görsel üretimi veya Responses alma/iptal işlemlerini uygulamaz. İstemciler için sohbet geçmişi saklamaz. Geçmişi isteklerde gönderin; üst akış durumlu (stateful) özellikleri yerel oturum depolama vaadi değildir.

## Yapılandırma ve yönlendirme

Başlamak için yapılandırma başvurusu [config.example.json](config.example.json) dosyasıdır. Dosya girdisi `//` ve `/* ... */` yorumlarına izin verir; API istek gövdeleri standart JSON gerektirir. Bilinmeyen alanlar, geçersiz değerler, yinelenen üst düzey alanlar ve çakışan eski/yeni OAuth alanları reddedilir. Kaydetme işlemi yapılandırmayı normalleştirir ve yorumları kaldırır.

| Ayar | Varsayılan / anlam |
| --- | --- |
| `server_keys` | En az bir etkin yerel anahtar gereklidir. |
| `zen_keys`, `go_keys` | Bağımsız üst akış anahtar havuzları; örnekte boştur. |
| `prefer` | `go`; tercih edilen kullanılabilir kimlik doğrulamalı katmanı (`go` veya `zen`) seçer. |
| `anonymous` | `false`; uygun ücretsiz modeller için üst akış `public` kimlik bilgisini etkinleştirir. |
| `upstream.zen` | Elle girilen Zen anahtarları için `https://opencode.ai/zen`. |
| `upstream.go` | Elle girilen Go anahtarları ve kullanım uç noktaları için `https://opencode.ai/zen/go`. |
| `upstream.anonymous` | Anonim trafik için `https://opencode.ai/inference`. |
| `models.refresh_seconds` | `300`; model keşif aralığı. |
| `models.protocols` | Model bazlı yerel protokol geçersiz kılmaları: `chat`, `responses`, `anthropic` veya `systemone`. |
| `models.free_only` | `false`; hem model keşfini hem yönlendirmeyi ücretsiz modellerle sınırlar. |
| `models.active_probes` | `false`; isteğe bağlı yapay ücretsiz model çıkarım yoklamaları. |
| `reasoning.effort` | Yerleşik varsayılanlarda ayarlı değil; örnekte `high` ayarlıdır. |
| `reasoning.effort_by_model` | Varsayılan akıl yürütme düzeyinin model bazlı geçersiz kılınması. |

### Modeller ve üst akış seçimi

Keşif, üst akış model listelerini `https://models.opencode.ai/api.json` adresindeki OpenCode yetenek ve fiyat kataloğuyla birleştirir. Katman başına yerel protokolleri izler ve `/v1/models` üzerinden mevcut bağlam/token sınırlarını, fiyatları, `native_protocol`, `supported_endpoints` ve akıl yürütme üst verilerini sunar. Fiyat/kullanımdan kalkma üst verileri her 24 saatte bir yenilenir; başarısız yenilemeler mevcut veriyi korur. Fiyatlar üst veridir, fatura tahmini veya hesap bakiyesi değildir.

Elle girilen anahtarlarla uygun ücretsiz modeller açıkça etkinleştirildiğinde önce anonim hattı, ardından `prefer` sırasına göre mevcut anahtarlı katmanları kullanabilir. Anonim erişim olmadan yapılandırılmış anahtarları kullanırlar. Ücretsiz uygunluğu model kimliğinde büyük/küçük harfe duyarsız `free` geçmesinden veya sıfır girdi/çıktı maliyetli kullanımdan kalkmamış üst veriden gelir; uygunluk üst akışta kabul garantisi vermez. Ücretli modeller uygun bir kimlik doğrulamalı rota gerektirir. `models.free_only` döndürme gruplarının üyelerini de filtreler.

OAuth rotaları ayrı kurallar izler: seçilen OAuth katmanı isteğin sahibi olur, yalnızca onaylanmış rotalarını kullanır ve hata sonrasında anonim trafiğe, elle girilen anahtarlara veya başka bir katmana düşmez. Go OAuth seçmek yetkilendirme hatasında, kota tükenmesinde veya 429'da asla bir PAYG isteği tetiklemez. Her iki katman da bir modeli sunuyorsa açıkça `prefer: "zen"` seçmek bağlı Zen PAYG rotasına izin verir.

### Zaman aşımları, vekil sunucular ve yakınlık (affinity)

| Ayar | Varsayılan | Anlam |
| --- | --- | --- |
| `retry.max_attempts` | `3` | Anahtarlı döngüler için yeniden deneme bütçesi; anonim ve döndürme yollarının kendi seçim kuralları vardır. |
| `retry.timeout_seconds` | `300` | Akış tüketimi dahil tüm çıkarım bütçesi. |
| `performance.attempt_timeout_seconds` | `0` | İsteğe bağlı daha kısa deneme sınırı; sıfır istek zaman aşımını bırakır. |
| `performance.first_event_timeout_seconds` | `0` | İsteğe bağlı ilk olay bekleme süresi; sıfır devre dışı bırakır. |
| `performance.connect_timeout_seconds` | `5` | Bağlantı kurma zaman aşımı. |
| `performance.failure_cooldown_seconds` | `15` | Hatalardan sonraki temel bekleme süresi. |
| `performance.max_anonymous_exits` | `8` (etkin) | En fazla farklı statik anonim çıkış sayısı; sıfır bu yerleşik sınırı seçer. |
| `performance.free_tier_block_seconds` | `900` (etkin) | Ücretsiz katman çıkış engelleme bekleme süresi; sıfır yerleşik değeri seçer. |
| `performance.max_idle_conns` | `2048` | Aktarım başına boşta bağlantı sayısı. |
| `performance.max_idle_conns_per_host` | `256` | Ana makine/aktarım başına boşta bağlantı sayısı. |
| `performance.max_conns_per_host` | `0` | Bağlantı sınırı; sıfır sınırsız demektir. |
| `performance.idle_conn_timeout_seconds` | `120` | Boşta bağlantı yaşam süresi. |

`proxies` ayarı `direct`, HTTP, HTTPS, SOCKS5 ve SOCKS5H URL'lerini kabul eder. `proxyfile` yapılandırma dizinine göreli olarak boş olmayan her satırda bir vekil sunucu ekler; girdiler tekilleştirilir ve boş çözümlenen liste doğrudan erişim anlamına gelir. Satır yorumları başta veya boşluktan sonra `#`, `;` ya da `//` ile başlar. Vekil sunucu URL'lerine gömülü kimlik bilgileri gizli veridir.

Sağlayıcı tarafından yönetilen dönen bir uç noktayı `pool+https://proxy.example:8443` veya `https://proxy.example:8443#pool` ile işaretleyin. Bu durumda her deneme keep-alive/HTTP/2 yeniden kullanımı olmadan yeni bir bağlantı açar; bu girdiler statik düğüm bekleme süresi veya sağlık tahliyesi kullanmaz. Sağlayıcı çıkışlarını gerçekten döndürmelidir. Statik vekil sunucular bağlantı yeniden kullanımını, sağlık takibini ve anahtar bağlamalarını korur. `proxy_bindings` bir anahtar parmak izini yapılandırılmış bir vekil sunucuya sabitleyebilir.

Açık bir `x-session-id` sohbet yakınlığı (affinity) sağlar. OpenCode oturum başlıkları ve desteklenen sohbet alanları da çalışır; aksi halde ilk sistem/kullanıcı içeriği bir çekirdek değer sağlar. Üretilen kimlikler yerel API anahtarı ve süreç ömrüyle kapsamlıdır. Geçerli resmi OpenCode oturum kimlikleri korunur. Bağımsız sohbetleri açık kimliklerle ayırın ve kullanıcı yalıtımı gerektiğinde ayrı anahtarlar kullanın; tek bir anahtar altındaki aynı başlangıçlar yakınlığı paylaşabilir.

İsteğe bağlı bir `models.anonymous_backoff` nesnesi yinelenen hatalardan sonra anonim hattı duraklatır. Varsayılanları eşik `6`, temel `1` saat, en fazla `24` saattir. Nesneyi eklememek bu hat düzeyindeki devre kesiciyi devre dışı bırakır.

### Ücretsiz model istek şekillendirme ve akıl yürütme

Ücretsiz model uyumluluğu resmi OpenCode sistem istemini ve araç tanımlarını enjekte edebilir, istemci araçlarını hat üzerinde yeniden adlandırabilir ve yanıtlarda adlarını geri yükleyebilir. Bu durum anahtarlarla sunulan ücretsiz modelleri de etkiler. Ağ geçidi araç çağrılarını aktarır; ana makinede araç çalıştırmaz. System One mesaj/araç şekillendirmesinin dışındadır.

Şekillendirme sırasında istemci araçlarının üst düzey açıklamalarındaki sıradan ASCII boşlukları varsayılan olarak bölünmez boşluklarla (`U+00A0`, NBSP) değiştirilir. Şemalar, parametre açıklamaları ve eklenen resmi araç tanımları bu dönüşümden etkilenmez. Bu açıklamalardaki kod örneklerinin içindeki boşluklar da değişir; üst akış eşleştirme kuralları zamanla değişebilir.

Enjekte edilen istem modeli ve UTC tarihini içerir; bilinmeyen istemci dizinini, çalışma alanını, platformu veya Git durumunu `unknown` olarak bildirir. Asla ağ geçidi ana makinesinin ortamını yerine koymaz. Bu bilgileri açıkça sağlamak için her çıkarım isteğinin üst düzeyine yalnızca ağ geçidine özel `client_environment` alanını ekleyin:

```json
{"client_environment":{"working_directory":"C:\\work\\project","workspace_root":"C:\\work\\project","platform":"windows","is_git_repo":true}}
```

Ağ geçidi bu uzantıyı iletmeden önce tüketip kaldırır. Ayrıca sistem/geliştirici yönergelerindeki biçimlendirilmiş `<env>` bloklarını ve bağımsız kullanıcı `<environment_context>` güncellemelerini tanır; en son kullanıcı güncellemesi geçerlidir. İstem enjekte edilirken kaynak önceliği sırasıyla açık `client_environment`, tanınan istemci bildirimleri, yapılandırılmış `upstream.prompt_environment` ve ardından `unknown` şeklindedir. Her kaynak tam bir anlık görüntü sağlar: eksik alanlar daha düşük öncelikli kaynaklardan doldurulmaz ve bir çalışma dizini çalışma alanı kökü ima etmez. Her istek kendi anlık görüntüsüne sahiptir; istekler veya oturumlar arasında ortam önbelleği yoktur. Ortamı doğrudan veya sohbet geçmişinde her istekle birlikte gönderin.

Yollar dosya sistemine erişilmeden metin olarak doğrulanır. `windows` değeri `win32` olarak normalleştirilir; `linux` ve `darwin` de kabul edilir. Dağıtım yedeği ayarlamak için yukarıdaki dört iç alanı `upstream.prompt_environment` içine yerleştirin, yapılandırma dosyasını düzenleyin ve yeniden yükleyin; bu gelişmiş değerlerin özel arayüz alanları yoktur. İstemcinin sağladığı resmi istem yine korunur. Bilinmeyen ortam değerleri canlı `deepseek-v4.1-flash-free` isteklerinde kabul edilmiştir; diğer modeller ve hatlar henüz doğrulanmamıştır.

Akıl yürütme `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `none`, `auto` veya boş değer kabul eder. İstemcinin açık düzeyi önceliklidir. `none` akıl yürütme ayarlarını kaldırır. `auto` yalnızca uygun ücretsiz Zen/anonim modeller için yakın zamanda doğrulanmış bir düzey tavanı kullanır; yoklama kapsamını genişletmez. Otomatik keşif etkin yoklamalar gerektirir, birkaç ücretli/kota tüketen istek yapabilir ve iki saat sonra sona erer. Go/ücretli modeller ve doğrulanmamış düzeyler çıkarımsal geçersiz kılma almaz.

Erişilebilirlik varsayılan olarak gerçek trafikten öğrenilir. Yapay kontroller yalnızca `models.active_probes: true` ile çalışır; yönetim yoklama eylemi kontrolleri planlar ancak bu anahtarı geçersiz kılmaz. Açık model emekliliği/bulunamadı kanıtı bir modeli devre dışı bırakabilir. Hız sınırları, kimlik doğrulama hataları, genel hatalar ve zaman aşımları emekliliği kanıtlamaz. Elle geri yükleme bir modeli etkin tutar ve otomatik kontrol geri verilene kadar otomatik yoklamadan hariç bırakır.

### WARP

`warp.enabled` varsayılan olarak `false` değerindedir. Etkinleştirildiğinde ücretsiz anonim/elle girilen Zen trafiği ve bunların keşif/yoklama yolları süreç içi bir Cloudflare WARP tüneli kullanır. Go trafiği, ücretli trafik ve OAuth rotaları yapılandırılmış vekil sunucu yönlendirmesini korur. WARP, Cloudflare `2408` portuna giden UDP gerektirir; TUN aygıtı veya yönlendirme tablosu değişikliği gerekmez. Başlatma hatası başlangıcı veya aday yapılandırmayı reddeder.

`warp.pool_size` 1 ile 32 arasında eşzamanlılık odaklı bir çıkış tavanıdır, varsayılanı 2'dir. `warp.stack` değeri `ipv6` (varsayılan), `ipv4` veya `dual` kabul eder. Bir çıkışı değiştirmek yeni bir üst akış hakkı garanti etmez. `warp.source_ipv6` isteğe bağlıdır, IPv6 yığını ve `/64` ya da daha kısa bir önek gerektirir ve Windows'ta seçilen kaynak adresler zaten bir arayüze atanmış olmalıdır. Ana makine ağını yapılandırmaz.

## Yönetim ve OAuth

WebUI; genel bakış, tanılama/Playground/günlükler, istemci erişimi, API anahtarları, modeller/kota, döndürme, yapılandırma ve hesap sayfalarını içerir. Basitleştirilmiş Çince, İngilizce, Vietnamca ve İtalyanca destekler; dil/tema seçimleri tarayıcıda saklanır. Uç nokta ayrıntıları için [yönetim kılavuzuna](docs/management.md) bakın.

Yönetim kimlik doğrulaması bir yönetici oturum çerezi, yazma işlemleri için CSRF belirteçleri ve giriş hız sınırlaması kullanır. Yönetici kimlik bilgilerini değiştirmek oturumları geçersiz kılar. Yerel API anahtarları yönetim erişimi vermez. `GET /api/config` gizli anahtarları maskeler; parola onaylı gösterme elle girilen anahtarları/vekil sunucu değerlerini gösterebilir ancak OAuth belirteçlerini asla göstermez.

Cihaz OAuth girişi başlatmak için **Models & Quota** sayfasını kullanın. Gösterilen resmi doğrulama URL'sini kendiniz açın, `user_code` değerini girin, başlattığınız isteği yetkilendirin ve sorulursa bir kuruluş/çalışma alanı seçin. Arka uç kimlik bilgilerini yoklar ve yeniler; belirteç yapıştırma, yerel kimlik dosyası içe aktarma veya geri çağırım URL'si akışı yoktur.

| Bağlantı | Kapsam ve kısıtlar |
| --- | --- |
| Go OAuth | Yalnızca resmi Go abonelik rotaları; kullanım hakkı bilinmiyor olarak gösterilir. |
| Zen OAuth | Çalışma alanı kredilerini kullanan konsol PAYG; başlatmadan önce açık onay gerekir. Kredi bakiyesi bilinmiyor olarak gösterilir. |

Go ve Zen bekleyen girişleri, kayıtlı kimlik bilgileri, yenileme durumu ve bağlantı kesme eylemleri ayrıdır. Sayfayı gizlemek/ayrılmak otomatik yoklamayı durdurur; geri dönmek süresi dolmamış bir girişi sürdürebilir. Çıkış yapmak o oturumun bekleyen girişlerini iptal eder ve tamamlanmış bağlantıları korur. Bekleyen bir girişi iptal etmek mevcut bir hesabın bağlantısını kesmez.

Kayıtlı bağlantılar yapılandırmada `go_oauth` ve `zen_oauth` olarak tutulur ve erişim/yenileme kimlik bilgilerini içerir. Yapılandırmayı ve yedeklerini koruyun. Elle girilen anahtarlar kayıtlı kalır ancak ilgili OAuth bağlantısı mevcutken — yeni giriş gerektirdiği durumlar dahil — yedek rota olmaz. Bağlantıyı kesmek yalnızca yerel bağlantıyı kaldırır, keşiften sonra elle yönlendirmeyi geri yükler ve resmi oturumu iptal etmez. Anonim erişim kapalıyken son kullanılabilir bağlantıyı kesmeden önce başka bir kimlik bilgisi bulundurun.

Yalnızca plana özgü URL ve SDK doğrulamasından geçen kimlik doğrulamalı resmi uzak yapılandırma rotaları kabul edilir. Ağ geçidi Go rotalarından PAYG URL'leri türetmez veya rastgele uzak başlıkları içe aktarmaz. Sıradan yapılandırma kayıtları arka uç tarafından yönetilen OAuth kimlik bilgilerini korur. Eski `go_subscription`/`zen_subscription` alanları yüklemede taşınır; tek bir plan için eski ve yeni alanları birleştirmek reddedilir.

OAuth entegrasyonu yönetim kılavuzunda listelenen üst akış kaynak anlık görüntülerini izler. Bu, her hesapta başarılı canlı yetkilendirme veya çıkarımı değil uygulama davranışını belgeler; üst akış izinleri ve rota biçimleri değişebilir.

### Yerel anahtarlar, kota ve döndürme

API anahtarı sayfası yerel anahtarlar oluşturur, adlandırır, etkinleştirir, devre dışı bırakır ve siler. Üretilen anahtarlar 192 rastgele bit içerir. Özel anahtarlar harf, rakam, `.`, `_` ve `-` karakterlerinden oluşan 16–256 karakter kabul eder; adlar 1–80 karakterdir ve en fazla 200 anahtar olabilir. En az biri etkin kalmalıdır. Yeni istekler değişiklikleri hemen görür. Anahtar başına model izni, harcama sınırı veya son kullanma ayarı yoktur.

Elle girilen Go anahtarı kotası yapılandırılmış Go üst akışının `/v1/usage` uç noktasından gelir: model bazlı bakiyeler değil paylaşılan kayan/haftalık/aylık yüzde pencereleridir. Anahtarlar aynı hakkı paylaşabileceğinden yüzdeleri toplanmaz. Başarılar 60 saniye, hatalar 15 saniye önbelleklenir; eski veriler bayat (stale) olarak işaretlenir. OAuth kotası/kredileri bilinmez ve asla elle girilen anahtarlardan veya başarılı isteklerden çıkarılmaz.

`sota` ve `sweet` döndürme takma adları Go modellerini kullanır ve akış dahil üç mesaj uç noktasıyla çalışır. Her grubun başarı sonrasında korunan ve hata durumunda ilerleyen servis genelinde bir güncel modeli vardır. SOTA `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3` ve `qwen3.8-max` modellerini içerir; Sweet diğer uygun Go modellerini toplar. Erişilebilirlik sabit adları değil gerçek keşfi izler. Takma adlar/sıra ve ilk çıktı zaman aşımı `/#rotation` adresinde düzenlenebilir.

Sıradan hata bütçesi her biri en fazla üç denemeli üç modeldir. Kota/hız sınırı hataları doğrudan bir sonraki modele atlar; döndürme ek paylaşılan kota yaratamaz. Akış çıktısı başladıktan sonra bir hata o yanıtı sonlandırır ve bir sonraki istek için grubu ilerletir. Yanıtlar `model` alanında istenen takma adı korur; `X-Resolved-Model` gerçek modeli belirtir. Durum yapılandırmanın yanında saklanır.

## Operasyon ve güvenlik

Yapılandırma kayıtları uygulamadan önce yedek bir çalışma zamanını doğrular ve oluşturur. Başarısız doğrulama veya kaydetme etkin çalışma zamanını korur. Anahtarlar, vekil sunucular, yönlendirme, model ayarları ve günlükleme yeni isteklere uygulanır; dinleyiciler ve WebUI etkinleştirme yeniden başlatma gerektirir. Dosya düzenlemeleri `POST /api/config/reload` veya arayüzdeki yeniden yükleme eylemini gerektirir; otomatik dosya izleyici yoktur.

| Veri | Depolama |
| --- | --- |
| Yapılandırma ve önceki sürüm | `config.json`, `config.json.bak`. |
| Model ve üst veri önbellekleri | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Erişilebilirlik ve döndürme | `config.json.<upstream-fingerprint>.availability.json`, `config.json.rotation.json`. |
| Oturumlar, kullanım, son istekler/denemeler, günlük halkası, son Playground sonucu | Bellek; süreç yeniden başladığında sıfırlanır. |
| Yapılandırılmış günlükler | stdout üzerinde JSON; saklama için harici bir toplayıcı gerekir. |

Adlar gerçek yapılandırma yolunu kullanır. İzleme sıcak yeniden yüklemelerde korunur ancak süreçler arasında paylaşılmaz. Token sayıları üst akışın bildirdiği kullanımı esas alır; eksik kullanım tahmin edilmez. İstek tamamlanması ve üst akış başlık denemesi metrikleri farklı şeyleri ölçer. Playground'un ayrı bir kullanım kovası vardır. Son saat tamponları en fazla 10.000 istek / 20.000 deneme tutar ve her birinden en fazla 500 döndürür.

`GET /healthz` hiçbir üst akış isteği yapmaz ve anahtar gerektirmez. Bekleyen/boş keşif, kullanılamayan WARP veya eksik zorunlu sağlıklı vekil sunucular 503 döndürür. Kullanılabilir bayat bir katalog `status: "degraded"` ile 200 döndürebilir; bayat, yenileme aralığının iki katından eski ve en az 60 saniye demektir. Hazırlık durumu geçerli bir anahtarın, kalan hakkın veya başarılı çıkarımın kanıtı değildir. Genel yanıt gizli değerleri veya vekil sunucu adreslerini değil sayıları/durumu içerir.

Yerel kullanım için API ve yönetim dinleyicilerini loopback üzerinde tutun. Ağ erişimi için HTTPS kullanın ve yönetime erişimi kısıtlayın. Güvenilir bir ters vekil sunucu `X-Forwarded-Proto` başlığını tutarlı biçimde ayarlamalıdır; hız sınırlama doğrudan eşi kullanır, dolayısıyla tek bir vekil sunucu arkasındaki istemciler sınırlarını paylaşır. Çalıştırılabilir dosyanın kendisi HTTP sunar.

Yapılandırma dosyaları ve yedekler üst akış anahtarlarını, vekil sunucu parolalarını ve OAuth belirteçlerini taşır; parola özetleme bunları şifrelemez. Dosya sistemi izinlerini kısıtlayın (uygun olduğunda Windows ACL'leri) ve çalışma zamanı dosyalarını kaynak paylaşımından hariç tutun. `logging.dump_request_bodies` varsayılan olarak kapalıdır ve hata ayıklama günlüğü gerektirir; maskelenmiş ve sınırlandırılmış gövde dökümleri bile sohbet içeriğini açığa çıkarabilir. Playground ve erişilebilirlik yoklamaları gerçek üst akış istekleri gönderir ve kota ya da kredi tüketebilir.

## Yerel geliştirme ve doğrulama

`cmd/opencode2api` servisi başlatır. `internal/admin` yönetimi, `gateway` yönlendirme/çalışma zamanını, `protocol` JSON/SSE dönüştürmesini, `models` keşfi, `oauth` resmi yetkilendirmeyi, `config` doğrulama/kalıcılığı ve `telemetry` metrik/günlüklemeyi üstlenir. Statik varlıklar `webui` içindedir; ön uç paket kurulumu gerekmez.

Normal bir geliştirme ortamı için yerel Go komutları:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Çalışma alanına özel önbellekler için Go'yu çağırmadan önce `GOCACHE` ve `GOMODCACHE` değişkenlerini kalıcı proje dizinlerine ayarlayın. Paket kurmadan JavaScript sözdizimini kontrol etmek için Node.js isteğe bağlıdır:

```bash
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Bu komutları Bash içinde çalıştırın (Windows'ta Git Bash). Dağıtım kontrolleri için [doğrulama ve bakım](docs/management.md#verification-and-maintenance) bölümüne bakın. Bir sürüm numarası `-ldflags "-X main.version=vX.Y.Z"` ile enjekte edilebilir.

| Belirti | Neyi incelemeli |
| --- | --- |
| API 401 | Yerel anahtar, etkin durumu ve istemci başlığı. |
| Başlangıç kimlik bilgilerini reddediyor | Bir üst akış anahtarı/kayıtlı OAuth bağlantısı yapılandırın veya anonim erişimi bilinçli olarak etkinleştirin. |
| Model yok / sağlık 503 | Keşif durumu, desteklenen yerel protokoller, yapılandırılmış kimlik bilgileri ve kaynak erişilebilirliği. |
| OAuth istekleri başarısız | Güvenli bağlantı durumu ve yeniden yetkilendirme gerekip gerekmediği; otomatik PAYG yedeklemesi yapılmaz. |
| HTTP 200 ama üretim başarısız | SSE hata olayları, istek sonucu ve `x-request-id` günlükleri. |
| Değişiklikler uygulanmıyor | Etkin yapılandırmayı kaydedin/yeniden yükleyin; dinleyici değişiklikleri için yeniden başlatın. |
| Ana makine config düzenlemeleri Compose'u etkilemiyor | Etkin yapılandırma durum birimindedir; dosyayı tekrar içe aktarın veya WebUI'ı kullanın. |
| Yeniden başlatma sonrası kullanım kayboluyor | Sayaçlar bellektedir; yapılandırılmış günlükleri ayrı saklayın. |

Yükseltmeden önce çalıştırılabilir dosyayı, yapılandırmayı ve durumu güvenli biçimde yedekleyin. Çalıştırılabilir dosyayı değiştirmeden önce süreci durdurun; geri alma eşleşen bir program/yapılandırma/durum setini geri yüklemelidir.

## Teşekkürler

Desteği için [LINUX DO](https://linux.do) topluluğuna teşekkürler.

## Lisans

SPDX-License-Identifier: GPL-3.0-or-later. Bkz. [LICENSE](LICENSE). Yazılım hiçbir garanti olmadan sunulmaktadır.
