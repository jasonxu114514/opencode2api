# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md) | [Türkçe](README.tr.md)

Gateway Go per OpenCode Zen e Zen Go. I client usano OpenAI Chat Completions, OpenAI Responses o Anthropic Messages; il gateway sceglie un upstream disponibile e converte richieste e risposte quando il protocollo nativo è diverso. Le decisioni System One hanno un endpoint di inoltro dedicato.

La WebUI di gestione è inclusa nell'eseguibile: per eseguirlo non servono Node.js né un database. Rilevamento dei modelli, inferenza, OAuth e WARP opzionale richiedono comunque l'accesso ai rispettivi servizi upstream.

## Avvio rapido

Scarica un binario da [GitHub Releases](https://github.com/jasonxu114514/opencode2api/releases) oppure compila con **Go 1.24 o superiore**:

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
cp config.example.json config.json
go build -o opencode2api ./cmd/opencode2api
```

Prima di avviare il servizio, modifica `config.json`:

1. Sostituisci `server_keys` con una tua API key locale.
2. Prima dell'avvio, inserisci una Zen API Key (consigliata) in `zen_keys` oppure una chiave Go in `go_keys`. L'esempio disabilita l'accesso anonimo (`anonymous: false`) e lascia vuoti entrambi gli array; anche una connessione Go OAuth (`go_oauth`) o Zen OAuth (`zen_oauth`) già salvata soddisfa il requisito. Non è supportato avviare senza credenziali soltanto per effettuare il primo login OAuth nella WebUI. Il canale anonimo potrebbe essere fortemente bloccato a monte: preferisci una Zen API Key.
3. Sostituisci `webui.password`. La configurazione di esempio abilita la WebUI con l'utente `admin`.

```bash
./opencode2api -config config.json
```

Su Windows, copia `config.example.json` in `config.json` e compila con `go build -o opencode2api.exe ./cmd/opencode2api`. Poi avvia il binario dalla directory corrente specificando `-config config.json`.

La configurazione di esempio usa `127.0.0.1:8080` per l'API e `0.0.0.0:8081` per la WebUI. Apri `http://localhost:8081` in locale. Limita l'accesso alla console di gestione e usa un reverse proxy HTTPS per gli accessi dalla rete.

Opzioni da riga di comando:

| Opzione       | Valore predefinito | Descrizione                              |
| ------------- | ------------------ | ---------------------------------------- |
| `-config`     | `config.json`      | Percorso del file di configurazione.     |
| `-listen`     | Non impostato      | Sostituisce l'indirizzo di ascolto API.  |
| `-web-listen` | Non impostato      | Sostituisce l'indirizzo di ascolto WebUI. |

La directory di configurazione deve essere scrivibile per migrare la password, salvare la configurazione e memorizzare le cache dei modelli.

## Docker e Compose

L'immagine pubblicata è `ghcr.io/jasonxu114514/opencode2api`. Prepara `config.json` prima dell'avvio. Per raggiungere le porte pubblicate, mantieni i listener interni API/WebUI su `0.0.0.0:8080` e `0.0.0.0:8081`; limita l'accesso tramite il binding delle porte host e HTTPS dove necessario.

```bash
docker compose up -d
docker compose logs -f
```

Compose importa la configurazione host in `opencode2api-state` **solo al primo avvio**. Successivamente il file attivo è nel volume: modificalo dalla WebUI. Per importare di nuovo un file host modificato:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Prima della sostituzione salva configurazione e stato attivi. L'entrypoint esegue il servizio senza privilegi e cambia l'esatto listener API dell'esempio `127.0.0.1:8080` in `0.0.0.0:8080` soltanto durante l'inizializzazione. Compose usa filesystem radice in sola lettura, volume di stato scrivibile e tmpfs `/tmp`. Le porte pubblicate si legano per default a tutte le interfacce host; per uso locale modifica `ports` in `127.0.0.1:8080:8080` e `127.0.0.1:8081:8081`.

| Variabile Compose | Predefinito | Effetto |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | Tag immagine; fissa un tag di rilascio per una versione precisa. |
| `OPENCODE2API_PORT` | `8080` | Porta API pubblicata sull'host. |
| `OPENCODE2API_WEBUI_PORT` | `8081` | Porta di gestione pubblicata sull'host. |
| `OPENCODE2API_LISTEN` | Non impostata | Override esplicito del listener API interno. |
| `OPENCODE2API_WEBUI_LISTEN` | Non impostata | Override esplicito del listener di gestione interno. |

Le ultime due diventano `LISTEN_ADDRESS` e `WEBUI_LISTEN_ADDRESS` nel container; se vuote, vale il file attivo. Cambiare porte host non modifica i listener interni. Se cambia la porta API interna, aggiorna mapping e health check dell'immagine, che usa 8080. Un `proxyfile` deve essere montato nel percorso configurato del container.

Per un'immagine locale:

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

WARP richiede UDP in uscita verso Cloudflare sulla porta `2408`, senza rete host o capacità di rete aggiuntive. Se la rete blocca questo traffico, WARP non può inizializzarsi.

## Uso dell'API

Queste sono le rotte di compatibilità esistenti. Per un client compatibile OpenAI usa `http://127.0.0.1:8080/v1` come URL di base; la gestione usa un listener separato.

| Metodo | Percorso | Comportamento |
| --- | --- | --- |
| `GET` | `/v1/models` | Modelli instradabili con la configurazione attuale e alias di rotazione disponibili. |
| `POST` | `/v1/chat/completions` | Chat Completions, JSON o SSE. |
| `POST` | `/v1/responses` | Responses, JSON o SSE. |
| `POST` | `/v1/messages` | Anthropic Messages, JSON o SSE. |
| `POST` | `/v1/systemone` | Decisioni System One native, senza traduzione. |
| `GET` | `/healthz` | Stato di disponibilità e risorse, senza autenticazione. |

Tutte le rotte `/v1/*` richiedono una chiave abilitata di `server_keys` in `Authorization: Bearer ...` o `x-api-key`. Il corpo ha un limite di 32 MiB. Le risposte di inferenza includono `x-request-id` per trovare i log correlati.

Consulta prima gli ID dei modelli effettivamente disponibili:

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Ciao"}]}'
```

Per Responses e Anthropic Messages invia, rispettivamente, questi corpi JSON standard al relativo endpoint:

```json
{"model":"MODEL_ID","input":"Ciao"}
```

```json
{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Ciao"}]}
```

I client Anthropic possono inviare anche `anthropic-version: 2023-06-01`. Per SSE aggiungi `"stream": true` e usa `curl -N` o un client che consumi gli eventi mentre arrivano. Uno stream può fallire dopo HTTP 200: controlla gli eventi di errore e l'esito finale.

System One richiede un modello con `native_protocol: "systemone"` e un corpo decisionale, per esempio:

```json
{"model":"SYSTEM_ONE_MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}
```

Il corpo e la risposta sono inoltrati senza conversione. Gli endpoint dei messaggi riconoscono questa struttura per i modelli System One, ma non convertono messaggi di chat ordinari in decisioni. Uno stream System One viene inoltrato senza recupero tramite reset.

### Limiti di compatibilità

I tre protocolli dei messaggi supportano testo, contenuti immagine/file rappresentabili, ragionamento, funzioni, chiamate e risultati degli strumenti, e streaming. Le richieste native mantengono i campi del provider che il gateway non modifica deliberatamente. La conversione trasferisce solo i campi con una rappresentazione supportata: ad esempio `seed` e i vincoli JSON di Chat non diventano opzioni Anthropic. Contenuti o strumenti non supportati possono causare un errore di validazione.

Nella conversione della cronologia Responses, ragionamento, testo e chiamate a funzioni dell'assistente vengono raggruppati in turni. Messaggi utente e risultati degli strumenti separano i turni; risultati mancanti o non corrispondenti restano errori nella conversione verso Chat Completions.

Non sono implementati embeddings, caricamento di file, generazione di immagini, recupero o annullamento Responses. Il gateway non conserva la cronologia delle conversazioni per i client. Inviala nelle richieste; le funzionalità con stato dell'upstream non costituiscono una memoria locale del gateway.

## Configurazione e instradamento

Parti da [config.example.json](config.example.json). Il file ammette commenti `//` e `/* ... */`; i corpi delle API richiedono JSON standard. Campi sconosciuti, valori non validi, campi duplicati al livello principale e conflitti tra vecchi e nuovi nomi OAuth vengono rifiutati. Il salvataggio normalizza il file ed elimina i commenti.

| Impostazione | Predefinito / significato |
| --- | --- |
| `server_keys` | Deve restare almeno una chiave locale abilitata. |
| `zen_keys`, `go_keys` | Pool upstream separati, vuoti nell'esempio. |
| `prefer` | `go`; accetta anche `zen` e sceglie il tier autenticato disponibile preferito. |
| `anonymous` | `false`; abilita la credenziale upstream `public` per modelli gratuiti idonei. |
| `upstream.zen` | `https://opencode.ai/zen` per chiavi Zen manuali. |
| `upstream.go` | `https://opencode.ai/zen/go` per chiavi Go manuali e relativo consumo. |
| `upstream.anonymous` | `https://opencode.ai/inference` per richieste anonime. |
| `models.refresh_seconds` | `300`; intervallo di rilevamento. |
| `models.protocols` | Override per modello: `chat`, `responses`, `anthropic`, `systemone`. |
| `models.free_only` | `false`; limita elenco e instradamento ai modelli gratuiti. |
| `models.active_probes` | `false`; test sintetici di inferenza gratuiti facoltativi. |
| `reasoning.effort` | Non impostato nei valori interni; l'esempio usa `high`. |
| `reasoning.effort_by_model` | Override del ragionamento per modello. |

### Modelli e scelta dell'upstream

Il rilevamento combina gli elenchi upstream con capacità e prezzi OpenCode da `https://models.opencode.ai/api.json`. I protocolli nativi sono distinti per tier. `/v1/models` espone i limiti noti di contesto/token, prezzi, `native_protocol`, `supported_endpoints` e metadati del ragionamento. Prezzi e deprecazioni vengono aggiornati ogni 24 ore; gli errori conservano i dati precedenti. I prezzi non sono un saldo né una stima della fattura.

Con chiavi manuali, i modelli gratuiti idonei possono usare prima la corsia anonima, se abilitata esplicitamente, poi i tier con chiave nell'ordine `prefer`. Con accesso anonimo disabilitato usano le chiavi configurate. Sono gratuiti gli ID che contengono `free` senza distinzione di maiuscole, oppure i modelli non deprecati con costo di input/output zero nei metadati; l'idoneità non garantisce l'accettazione upstream. I modelli a pagamento richiedono una rotta autenticata appropriata. `models.free_only` filtra anche i gruppi di rotazione.

OAuth ha regole separate: una richiesta che seleziona un tier OAuth usa soltanto le sue rotte confermate, senza ripiego su anonimo, chiavi manuali o altri tier dopo un errore. Go OAuth non attiva PAYG quando falliscono autorizzazione, quota o richieste con 429. Se entrambi i tier servono un modello, scegliere esplicitamente `prefer: "zen"` consente l'uso della connessione Zen PAYG.

### Timeout, proxy e affinità

| Impostazione | Predefinito | Significato |
| --- | --- | --- |
| `retry.max_attempts` | `3` | Budget dei cicli con chiavi; anonimo e rotazione hanno regole proprie. |
| `retry.timeout_seconds` | `300` | Budget totale dell'inferenza, incluso il consumo dello stream. |
| `performance.attempt_timeout_seconds` | `0` | Limite facoltativo più breve per tentativo; zero mantiene il timeout totale. |
| `performance.first_event_timeout_seconds` | `0` | Attesa facoltativa del primo evento; zero la disabilita. |
| `performance.connect_timeout_seconds` | `5` | Timeout di connessione. |
| `performance.failure_cooldown_seconds` | `15` | Pausa di base dopo un errore. |
| `performance.max_anonymous_exits` | `8` effettivo | Uscite statiche distinte per richiesta anonima; zero usa il limite interno. |
| `performance.free_tier_block_seconds` | `900` effettivo | Pausa di un'uscita bloccata dal tier gratuito; zero usa il valore interno. |
| `performance.max_idle_conns` | `2048` | Connessioni inattive per transport. |
| `performance.max_idle_conns_per_host` | `256` | Connessioni inattive per host/transport. |
| `performance.max_conns_per_host` | `0` | Limite connessioni; zero significa illimitato. |
| `performance.idle_conn_timeout_seconds` | `120` | Durata delle connessioni inattive. |

`proxies` accetta `direct`, HTTP, HTTPS, SOCKS5 e SOCKS5H. `proxyfile` aggiunge un proxy per riga non vuota, con percorso relativo alla configurazione; i duplicati sono eliminati e un elenco risolto vuoto equivale ad accesso diretto. I commenti iniziano con `#`, `;` o `//` a inizio riga o dopo spazi. Le credenziali nei relativi URL sono segrete.

Per un endpoint già ruotato dal provider usa `pool+https://proxy.example:8443` o `https://proxy.example:8443#pool`. Ogni tentativo apre una connessione nuova senza riuso keep-alive/HTTP/2, cooldown o esclusione sanitaria dei nodi statici. La rotazione deve essere fornita dal provider. I proxy statici mantengono riuso, stato di salute e associazione alle chiavi. `proxy_bindings` può associare l'impronta di una chiave a un proxy configurato.

Un `x-session-id` esplicito determina l'affinità di conversazione. Sono riconosciuti anche gli header OpenCode e i campi di conversazione supportati; in loro assenza il seme deriva dal contenuto iniziale system/user. Gli ID generati sono isolati per chiave locale e durata del processo. Gli ID ufficiali OpenCode validi sono conservati. Usa ID distinti per conversazioni indipendenti e chiavi separate per isolare gli utenti: inizi identici con la stessa chiave possono condividere affinità.

L'oggetto facoltativo `models.anonymous_backoff` sospende la corsia anonima dopo errori ripetuti: soglia predefinita `6`, durata base `1` ora, massimo `24` ore. Ometterlo disabilita questa protezione a livello di corsia.

### Adattamento dei modelli gratuiti e ragionamento

La compatibilità gratuita può inserire prompt e strumenti ufficiali OpenCode, rinominare gli strumenti sul collegamento upstream e ripristinarne i nomi nelle risposte. Riguarda anche modelli gratuiti serviti con chiave. Il gateway inoltra le chiamate, senza eseguire strumenti sul computer. System One è escluso dall'adattamento di messaggi e strumenti.

Durante l'adattamento, gli spazi ASCII nelle descrizioni principali degli strumenti del client vengono sostituiti per impostazione predefinita con spazi non separabili (`U+00A0`, NBSP). La conversione non modifica schemi, descrizioni dei parametri o definizioni ufficiali aggiunte. Cambiano anche gli spazi negli esempi di codice contenuti nelle descrizioni; le regole di confronto dell'upstream possono cambiare nel tempo.

Il prompt inserito include modello e data UTC; directory, workspace, piattaforma e stato Git del client non noti sono indicati come `unknown`. Non vengono sostituiti con l'ambiente del gateway. Per fornire questi dati esplicitamente, aggiungi il campo `client_environment`, riservato al gateway, al livello principale di ogni richiesta di inferenza:

```json
{"client_environment":{"working_directory":"C:\\work\\project","workspace_root":"C:\\work\\project","platform":"windows","is_git_repo":true}}
```

Il gateway legge e rimuove questa estensione prima dell'inoltro. Riconosce anche blocchi `<env>` formattati nelle istruzioni system/developer e aggiornamenti `<environment_context>` in messaggi user dedicati; prevale l'ultimo aggiornamento user. Quando viene inserito un prompt, l'ordine di priorità è: `client_environment` esplicito, dichiarazioni riconosciute del client, `upstream.prompt_environment` configurato, ambiente sconosciuto. Ogni fonte fornisce uno stato completo: i campi mancanti non vengono integrati da fonti con priorità inferiore e la directory di lavoro non implica la radice del workspace. Lo stato appartiene alla singola richiesta, senza cache tra richieste o sessioni. Invia l'ambiente con ogni richiesta, direttamente o nella cronologia della conversazione.

I percorsi vengono validati come testo senza accesso al filesystem. `windows` diventa `win32`; sono ammessi anche `linux` e `darwin`. Per un valore predefinito del deployment, inserisci i quattro campi interni dell'esempio in `upstream.prompt_environment`, modifica il file di configurazione e ricaricalo; questi valori avanzati non hanno campi dedicati nella UI. Un prompt ufficiale già fornito dal client viene preservato. I valori di ambiente `unknown` sono stati accettati in richieste reali a `deepseek-v4.1-flash-free`; altri modelli e corsie restano da verificare.

Il ragionamento accetta `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `none`, `auto` o un valore vuoto. Il livello esplicito del client ha precedenza. `none` rimuove le impostazioni. `auto` usa un limite recentemente verificato soltanto per modelli gratuiti Zen/anonimi e non estende la portata dei test. La scoperta richiede test attivi, può consumare quota con più richieste e scade dopo due ore. Nessun livello viene dedotto per Go, modelli a pagamento o capacità non verificate.

La disponibilità viene appresa dal traffico reale per impostazione predefinita. I test sintetici richiedono `models.active_probes: true`; il comando di gestione li pianifica senza aggirare l'opzione. Solo prove esplicite di modello ritirato/inesistente possono disabilitarlo. Rate limit, errori di autenticazione, errori generici e timeout non dimostrano un ritiro. Il ripristino manuale mantiene il modello abilitato e lo esclude dai test fino al ritorno al controllo automatico.

### WARP

`warp.enabled` è `false`. Se abilitato, il traffico gratuito anonimo/Zen manuale e i relativi elenchi/test passano per il tunnel Cloudflare WARP del processo. Go, modelli a pagamento e OAuth mantengono i proxy configurati. Serve UDP in uscita verso Cloudflare sulla porta `2408`; non occorrono dispositivi TUN o modifiche alle tabelle di routing. Un errore di inizializzazione rifiuta l'avvio o la configurazione candidata.

`warp.pool_size` è il massimo di uscite adattato alla concorrenza, da 1 a 32, predefinito 2. `warp.stack` ammette `ipv6` (predefinito), `ipv4` o `dual`. Cambiare uscita non garantisce nuova quota upstream. `warp.source_ipv6` è facoltativo, richiede stack IPv6 e prefisso `/64` o più corto; su Windows gli indirizzi sorgente scelti devono essere già assegnati a un'interfaccia. Il gateway non configura la rete del computer.

## Gestione e OAuth

La WebUI comprende panoramica, diagnostica/Playground/log, accesso client, chiavi API, modelli/quota, rotazione, configurazione e account. Supporta cinese semplificato, inglese, vietnamita e italiano; lingua e tema sono conservati nel browser. La [guida di gestione](docs/management.it.md) contiene i dettagli degli endpoint.

La gestione usa cookie di sessione amministratore, token CSRF per le scritture e limiti al login. Modificare le credenziali amministrative invalida le sessioni. Le chiavi API locali non concedono accesso amministrativo. `GET /api/config` maschera i segreti; la visualizzazione con verifica della password può mostrare chiavi manuali/proxy, ma mai token OAuth.

Da **Modelli e quota** avvia un login Device OAuth. Apri personalmente l'URL ufficiale mostrato, inserisci `user_code`, autorizza la richiesta iniziata da te e seleziona organizzazione/workspace se richiesto. Il backend gestisce polling e rinnovo. Non si incollano token, importano file di credenziali locali o inviano URL di callback.

| Connessione | Ambito e limiti |
| --- | --- |
| Go OAuth | Solo rotte ufficiali dell'abbonamento Go; quota sconosciuta. |
| Zen OAuth | Console PAYG che consuma credits del workspace; conferma esplicita prima dell'avvio, saldo sconosciuto. |

Login in attesa, credenziali, rinnovi e disconnessioni Go e Zen sono separati. Nascondere o lasciare la pagina interrompe il polling; tornando si può riprendere un login non scaduto. Il logout annulla i login in attesa della sessione senza rimuovere le connessioni completate. Annullare un login non disconnette l'account già salvato.

Le connessioni `go_oauth` e `zen_oauth` nella configurazione contengono credenziali di accesso e rinnovo: proteggi file e backup. Le chiavi manuali sono conservate ma non sono alternative mentre esiste l'OAuth corrispondente, anche se richiede un nuovo login. Disconnettere rimuove soltanto la connessione locale, ripristina le rotte manuali dopo il rilevamento e non revoca la sessione ufficiale. Conserva un'altra credenziale prima di rimuovere l'ultima con accesso anonimo disabilitato.

Sono accettate solo rotte della configurazione remota ufficiale autenticata che superano i controlli URL/SDK del relativo piano. Non si ricavano URL PAYG da Go né si importano header remoti arbitrari. I normali salvataggi conservano le credenziali OAuth gestite dal backend. I vecchi campi `go_subscription`/`zen_subscription` migrano al caricamento; combinare nome vecchio e nuovo per uno stesso piano causa un errore.

L'implementazione segue le versioni sorgente upstream elencate nella guida. È una descrizione del codice, non una verifica di autorizzazione o inferenza reale per ogni account: permessi e formati delle rotte possono cambiare.

### Chiavi locali, quota e rotazione

La pagina delle chiavi crea, rinomina, abilita, disabilita ed elimina le chiavi locali. Quelle generate contengono 192 bit casuali. Le chiavi personalizzate ammettono 16–256 lettere, cifre, `.`, `_`, `-`; i nomi 1–80 caratteri, fino a 200 chiavi e almeno una abilitata. Le modifiche valgono subito per nuove richieste. Non ci sono permessi per modello, limiti di spesa o scadenze per chiave.

La quota delle chiavi Go manuali deriva da `/v1/usage` dell'upstream Go configurato: finestre condivise mobili, settimanali e mensili, non saldi per modello. Più chiavi possono condividere la stessa quota, quindi non sommare le percentuali. Cache: 60 secondi sui successi, 15 sugli errori; i dati precedenti sono segnalati come obsoleti. Quota/credits OAuth restano sconosciuti e non vengono dedotti da chiavi manuali o richieste riuscite.

Gli alias `sota` e `sweet` usano modelli Go sui tre endpoint dei messaggi, anche in streaming. Ogni gruppo ha un modello corrente condiviso dal servizio: rimane dopo un successo e avanza dopo un errore. SOTA contiene `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, `qwen3.8-max`; Sweet raccoglie gli altri modelli Go idonei. La disponibilità effettiva dipende dal rilevamento. Modifica alias, ordine e timeout del primo output in `/#rotation`.

Il budget ordinario è di tre modelli, fino a tre tentativi ciascuno. Quota/rate limit passano direttamente al successivo; la rotazione non crea quota condivisa aggiuntiva. Dopo l'inizio effettivo dello stream, un errore termina la risposta e fa avanzare il gruppo per la richiesta seguente. `model` conserva l'alias, `X-Resolved-Model` indica il modello reale. Lo stato è salvato accanto alla configurazione.

## Esercizio e sicurezza

Il salvataggio convalida e costruisce un nuovo runtime prima di applicarlo. Errori di validazione o persistenza mantengono quello attivo. Chiavi, proxy, rotte, modelli e log si applicano alle nuove richieste; listener e abilitazione WebUI richiedono riavvio. Dopo modifiche al file usa la ricarica nella UI o `POST /api/config/reload`: non esiste un monitor automatico del file.

| Dati | Memorizzazione |
| --- | --- |
| Configurazione e versione precedente | `config.json`, `config.json.bak`. |
| Cache modelli e metadati | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Disponibilità e rotazione | `config.json.<upstream-fingerprint>.availability.json`, `config.json.rotation.json`. |
| Sessioni, uso, richieste/tentativi recenti, log circolari, ultimo Playground | Memoria; azzerati al riavvio del processo. |
| Log strutturati | JSON su stdout; occorre un raccoglitore per conservarli. |

I nomi derivano dal percorso di configurazione effettivo. Il monitoraggio sopravvive alla ricarica ma non è condiviso fra processi. I token sono quelli dichiarati dall'upstream; i dati mancanti non vengono stimati. Completamento delle richieste e tentativi fino agli header sono misure diverse. Playground ha un conteggio separato. Nell'ultima ora sono conservate al massimo 10.000 richieste/20.000 tentativi e l'API ne restituisce fino a 500 per tipo.

`GET /healthz` non chiama l'upstream e non richiede chiavi. Rilevamento in attesa/vuoto, WARP indisponibile o assenza di proxy sani necessari producono 503. Un catalogo obsoleto ma utilizzabile può restituire 200 e `status: "degraded"`; la soglia è due volte l'intervallo di aggiornamento, almeno 60 secondi. La disponibilità non certifica chiavi, quota o inferenza. La risposta pubblica include conteggi/stato, senza segreti o indirizzi proxy.

Per uso locale limita API e gestione a loopback. Per accesso in rete usa HTTPS e limita l'ingresso amministrativo. Un reverse proxy fidato deve impostare correttamente `X-Forwarded-Proto`; i limiti usano il peer diretto, quindi i client dietro un proxy li condividono. L'eseguibile serve HTTP.

Configurazione e backup contengono chiavi upstream, password proxy e token OAuth; l'hash della password non li cifra. Limita i permessi dei file, con ACL su Windows, ed escludi i file di runtime dalla condivisione del codice. `logging.dump_request_bodies` è disabilitato e richiede livello debug; i corpi possono rivelare conversazioni anche dopo riduzione e mascheramento. Playground e test attivi consumano potenzialmente quota o credits reali.

## Sviluppo locale e verifiche

`cmd/opencode2api` avvia il servizio. In `internal`, `admin` gestisce l'amministrazione, `gateway` rotte/runtime, `protocol` conversione JSON/SSE, `models` rilevamento, `oauth` autorizzazione ufficiale, `config` validazione/persistenza, `telemetry` metriche/log. Le risorse statiche sono in `webui`; non occorre installare pacchetti frontend.

In un normale ambiente di sviluppo usa questi comandi nativi. Per cache nel progetto, imposta `GOCACHE` e `GOMODCACHE` su directory persistenti locali:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Node.js serve soltanto per il controllo facoltativo della sintassi JavaScript, senza installazione di pacchetti. In Bash, oppure Git Bash su Windows:

```bash
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

Esegui i controlli in Bash; su Windows puoi usare Git Bash. Per le verifiche prima della distribuzione consulta [Verifica e manutenzione](docs/management.it.md#verifica-e-manutenzione). La versione di rilascio si inietta con `-ldflags "-X main.version=vX.Y.Z"`.

| Sintomo | Cosa controllare |
| --- | --- |
| API 401 | Chiave locale, abilitazione e header del client. |
| Avvio senza credenziali | Configurare chiavi/OAuth salvato o abilitare consapevolmente l'anonimo. |
| Nessun modello / health 503 | Catalogo, protocolli supportati, credenziali configurate e risorse. |
| Errori OAuth | Stato sicuro dell'account e necessità di nuovo login; nessun ripiego automatico PAYG. |
| HTTP 200 ma generazione fallita | Eventi SSE, esito e log `x-request-id`. |
| Modifiche senza effetto | Salvare/ricaricare il file attivo; riavviare per i listener. |
| Modifiche host senza effetto in Compose | Il file attivo è nel volume di stato; importarlo di nuovo o usare la WebUI. |
| Uso azzerato al riavvio | I contatori sono in memoria; conservare i log separatamente. |

Prima dell'aggiornamento salva in modo sicuro eseguibile, configurazione e stato. Arresta il processo per sostituire il programma; per tornare indietro ripristina versioni compatibili di tutti e tre.

## Ringraziamenti

Grazie alla community [LINUX DO](https://linux.do) per il supporto.

## Licenza

SPDX-License-Identifier: GPL-3.0-or-later. Termini completi in [LICENSE](LICENSE). Il software è fornito senza garanzia.
