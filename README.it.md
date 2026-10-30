# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md) | [Italiano](README.it.md)

Gateway scritto in Go per **OpenCode Zen e Zen Go**. Espone gli endpoint Chat Completions, Responses e Anthropic Messages, traduce tra i rispettivi protocolli nativi e gestisce le chiavi upstream e i proxy.

L'eseguibile include la WebUI: per eseguire il servizio non servono Node.js né un database.

## Funzionalità

- Risposte JSON e SSE per tutti e tre i protocolli di inferenza.
- Testo, immagini, ragionamento, function tool, tool call e tool result; contenuti di file quando il protocollo di destinazione li supporta.
- Pool separati di chiavi Zen e Go, preferenza del tier configurabile, retry e affinità di sessione.
- Accesso anonimo opzionale a Zen con la credenziale `public` di OpenCode.
- Uscita Cloudflare WARP in userspace per il traffico dei modelli gratuiti, senza TUN né root.
- Connessioni dirette e tramite HTTP, HTTPS, SOCKS5 e SOCKS5H, anche da un file proxy.
- Rilevamento dinamico dei modelli, metadati del protocollo nativo e cache su disco.
- Porta di gestione separata con modifica della configurazione, Playground, diagnostica, statistiche dei token e log in tempo reale.
- Ricaricamento a caldo della configurazione, con convalida prima di passare le nuove richieste a un gateway sostitutivo.
- Dashboard delle quote dell'abbonamento Go con percentuali residue, orari di reset e modelli disponibili.
- Creazione, denominazione, abilitazione, disabilitazione ed eliminazione immediata di API key locali.
- Gruppi di modelli `sota` / `sweet` con affinità, ordinamento, failover, protezione dello streaming e stato persistente.

Consulta la [guida della console di gestione (in cinese)](docs/management.zh-CN.md) per informazioni sulle nuove pagine e sulla configurazione.

## Sponsor

<table>
  <tr>
    <td width="190" align="center">
      <a href="https://www.ipwo.net/?ref=githubopencode2api">
        <img src="assets/IPWO.png" width="163" alt="IPWO">
      </a>
    </td>
    <td>
      <a href="https://www.ipwo.net/?ref=githubopencode2api">IPWO</a> offre proxy residenziali con connettività di rete flessibile per lo sviluppo di AI e workflow API, coprendo oltre 195 paesi e regioni con supporto HTTP(S) e SOCKS5.<br>
      Adatti a test di rete cross-region, raccolta dati conforme e sviluppo automatizzato. Sono disponibili proxy dinamici, statici e illimitati, con test gratuito e selezione della regione. Codice sconto: <b>0200</b>.
    </td>
  </tr>
</table>

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
2. Configura le chiavi Zen o Go. In alternativa, imposta `anonymous: true` e lascia vuoti entrambi gli array delle chiavi upstream.
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

## Docker

L'immagine pubblicata è `ghcr.io/jasonxu114514/opencode2api`.

```bash
cp config.example.json config.json
# Configura le chiavi e sostituisci la password WebUI prima dell'avvio.
docker compose up -d
docker compose logs -f
```

Compose importa la configurazione dell'host nel volume `opencode2api-state` **solo al primo avvio**. Le modifiche successive vanno effettuate tramite la WebUI. Per importare di nuovo una configurazione modificata sull'host:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

Il container esegue il servizio con un utente non privilegiato. Compose usa un filesystem root in sola lettura, un volume di stato scrivibile e un filesystem temporaneo `/tmp`.

WARP richiede solo UDP in uscita (endpoint Cloudflare, porta `2408`), quindi la configurazione dell'immagine senza privilegi e senza `NET_ADMIN` funziona così com'è; non serve passare a host networking né aggiungere `--cap-add`. Se la rete del container blocca UDP, l'abilitazione di WARP fallisce all'avvio.

| Variabile Compose           | Predefinito | Effetto                                                    |
| --------------------------- | ----------- | ---------------------------------------------------------- |
| `OPENCODE2API_VERSION`      | `latest`    | Tag dell'immagine; fissa un tag di release per una versione precisa. |
| `OPENCODE2API_PORT`         | `8080`      | Porta host mappata alla porta 8080 del container.          |
| `OPENCODE2API_WEBUI_PORT`   | `8081`      | Porta host mappata alla porta 8081 del container.          |
| `OPENCODE2API_LISTEN`       | Non impostata | Sostituisce esplicitamente l'ascolto API nel container.   |
| `OPENCODE2API_WEBUI_LISTEN` | Non impostata | Sostituisce esplicitamente l'ascolto WebUI nel container. |

Le ultime due variabili diventano `LISTEN_ADDRESS` e `WEBUI_LISTEN_ADDRESS` nel container. I valori vuoti usano quelli del file di configurazione. Durante l'inizializzazione, l'entrypoint cambia l'indirizzo API di esempio `127.0.0.1:8080` in `0.0.0.0:8080`, così le porte pubblicate possono raggiungerlo.

Cambiare le porte host non cambia gli indirizzi di ascolto nel container. Se cambi la porta API interna, aggiorna anche la mappatura delle porte e l'health check dell'immagine, che usa la porta 8080.

Per creare un'immagine locale:

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

Anche il file `proxyfile` usato in Docker deve essere presente nel container al percorso configurato.

## Uso dell'API

`server_keys` autentica i client presso questo gateway. È separato da `zen_keys` e `go_keys` e non viene mai usato come credenziale upstream.

Invia `Authorization: Bearer YOUR_LOCAL_API_KEY` oppure `x-api-key: YOUR_LOCAL_API_KEY`. Gli health check non richiedono autenticazione.

| Metodo | Percorso               | Descrizione                                                |
| ------ | ---------------------- | ---------------------------------------------------------- |
| GET    | `/v1/models`           | Modelli instradabili con la configurazione corrente.      |
| POST   | `/v1/chat/completions` | Chat Completions.                                          |
| POST   | `/v1/responses`        | Responses.                                                 |
| POST   | `/v1/messages`         | Anthropic Messages.                                        |
| POST   | `/v1/systemone`        | Decisioni strutturate System One.                          |
| GET    | `/healthz`             | Stato di disponibilità e riepilogo delle risorse.          |

Prima individua un modello disponibile:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"
```

Sostituisci `MODEL_ID` negli esempi seguenti con un ID restituito dalla richiesta.

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

Per lo streaming, aggiungi `"stream": true` al body e usa `curl -N`. Le risposte includono `x-request-id` per la correlazione. I body delle richieste API sono limitati a 32 MiB.

**System One**

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

System One associa uno stato libero a domande tipizzate e restituisce risposte tipizzate. Il payload non condivide la struttura del bridge Chat/Responses: viene inoltrato senza modifiche e non viene mai tradotto. Questo endpoint serve solo modelli con protocollo nativo System One; anche un payload decisionale inviato a un endpoint messaggi (`/v1/chat/completions`, `/v1/responses`) viene inoltrato senza conversione. Le risposte streaming upstream vengono inoltrate così come sono, senza recupero dopo un'interruzione.

### Limiti di compatibilità

Le richieste che usano il protocollo nativo dell'upstream conservano i campi specifici del provider. Quelle che passano da un protocollo all'altro usano una rappresentazione comune e non tutte le opzioni hanno un equivalente. Per esempio, i vincoli JSON di Chat e `seed` non vengono inoltrati ad Anthropic Messages. Contenuti o tipi di tool non supportati possono essere rifiutati.

Il gateway espone i percorsi elencati sopra. Non implementa embeddings, caricamento di file, generazione di immagini, né recupero o annullamento di Responses. Non memorizza la cronologia delle conversazioni: i client devono inviarla o usare le funzionalità supportate dall'upstream effettivo.

## Instradamento e modelli

### Rilevamento dei modelli

Il gateway aggiorna `/v1/models` di Zen/Go e il [catalogo delle capacità](https://models.opencode.ai/api.json) di OpenCode all'intervallo configurato. Protocolli nativi e limiti dei modelli sono tracciati separatamente per ogni tier. La documentazione Zen/Go di OpenCode è una fonte di ripiego per il rilevamento del protocollo.

`models.protocols` permette di sovrascrivere il rilevamento:

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

I valori di protocollo consentiti sono `chat`, `responses` e `anthropic`. I modelli con protocolli nativi non supportati vengono esclusi dal rilevamento, a meno che non siano sovrascritti.

Costi, stato di deprecazione e limiti dei modelli provengono dall'[endpoint ufficiale dei metadati OpenCode](https://models.opencode.ai/api.json), aggiornato ogni 24 ore. Il catalogo delle capacità di OpenCode resta la fonte autorevole per i limiti di contesto e token; i metadati `limit.context`, `limit.input` e `limit.output` completano i campi mancanti. `/v1/models` espone questi limiti come `context_window`/`context_length`, `max_input` e `max_output` per client di rilevamento quali CLIProxyAPI e Pi. Ogni client HTTP attende al massimo 30 secondi per una richiesta di metadati. Gli aggiornamenti non riusciti conservano i dati esistenti.

Quando i metadati indicano un prezzo, ogni elemento di `/v1/models` include anche `cost` (USD per milione di token: `input`, `output`, `cache_read`, `cache_write`, oltre a `cacheRead`/`cacheWrite` di OpenClaw). I modelli con ragionamento elencano i livelli in `reasoning_efforts` e in `compat.supportedReasoningEfforts` di OpenClaw: da `minimal` a `max`, limitati al livello massimo verificato quando il livello del modello è `auto`. Un livello forzato o verificato è esposto come `default_reasoning_effort`. I prezzi della cache compaiono dopo il successivo aggiornamento dei metadati.

### Accesso anonimo e fallback

Con `anonymous: true`, un modello può usare il canale Zen anonimo se soddisfa una delle seguenti condizioni:

- Il suo ID contiene `free`, senza distinzione tra maiuscole e minuscole.
- I metadati OpenCode indicano un costo input e output pari a zero e il modello non è deprecato.

L'idoneità determina l'instradamento; l'upstream può comunque rifiutare o limitare la richiesta. Le richieste anonime usano `public` nell'header di autenticazione upstream.

L'ordine di instradamento è:

1. Per un modello idoneo, prova una volta ciascun proxy anonimo disponibile.
2. Prova i tier autenticati nell'ordine `prefer`, usando solo i tier con una chiave configurata e una route per quel modello.
3. Applica `retry.max_attempts` separatamente a ogni tier autenticato.

I tentativi anonimi non sono limitati da `retry.max_attempts`, ma condividono il timeout della richiesta. Errori di rete, autenticazione, limiti di frequenza ed errori del server possono causare la rotazione delle chiavi. Gli altri errori 4xx terminano il tier corrente; è comunque possibile provare un altro tier disponibile.

Le richieste vengono codificate con il protocollo specifico di ogni tier. Se è attivo `performance.first_event_timeout_seconds`, una risposta SSE riuscita viene trattenuta fino al primo evento dati completo. Un timeout o una disconnessione prima dell'evento usa il budget di retry esistente per chiavi e proxy; i commenti non soddisfano il timeout. Dopo l'invio al client, lo stream non viene mai riprodotto. Il valore predefinito è `0` (disattivato); prova `15` secondi per upstream che accettano richieste ma non inviano eventi. Questo limite riguarda il primo evento, non il primo token di testo, e non si applica a System One. Gli alias di rotazione usano il timeout separato `rotation.first_output_seconds`. Un riferimento di ragionamento Responses obsoleto riconosciuto può attivare un singolo tentativo di correzione; la diagnostica con chiave selezionata non usa mai tale ripetizione.

Quando è configurato solo l'accesso anonimo, `/v1/models` espone esclusivamente i modelli idonei per quel canale.

I modelli gratuiti non richiedono una chiave Zen: con `anonymous: true` usano il canale pubblico e un'eventuale chiave configurata serve solo come fallback. Per conservare le chiavi ma esporre solo modelli gratuiti, imposta `models.free_only: true`. I modelli a pagamento scompaiono da `/v1/models`, le richieste dirette vengono rifiutate e gli alias di rotazione mantengono solo i membri gratuiti; questi ultimi possono comunque usare le chiavi configurate come fallback.

### Uscita WARP per i modelli gratuiti

`warp.enabled` sposta ogni richiesta di un modello gratuito sul canale gratuito — canale anonimo, fallback dei modelli gratuiti tramite chiave Zen, probe di disponibilità e aggiornamento anonimo del catalogo — su un tunnel Cloudflare WARP in userspace. Queste richieste non usano alcun proxy, quindi l'upstream vede l'indirizzo di uscita WARP invece del nodo configurato. Le chiavi del piano Go e i modelli a pagamento mantengono i loro proxy.

Il tunnel vive nel processo del gateway: registra un dispositivo WARP, parla WireGuard su UDP con Cloudflare e termina in uno stack di rete in userspace. Non richiede dispositivi TUN, root, `NET_ADMIN` né modifiche alla tabella di routing, ma l'host o il container deve poter inviare UDP verso l'endpoint di Cloudflare (porta `2408`). Registrazione e probe del trace `warp=on` avvengono all'avvio; se WARP non riesce a inizializzarsi, l'avvio fallisce invece di ripiegare silenziosamente sui proxy. Anche l'abilitazione dalla WebUI è fail-closed: un valore errato restituisce un errore e il runtime precedente continua a servire.

`warp.pool_size` (1–32, predefinito `2`) è un **limite massimo**, non un numero fisso. La prima uscita viene registrata in modo sincrono e determina la readiness; poi il pool registra in background metà del massimo (almeno una), così un picco trova uscite già calde senza attendere. Il ring attivo segue la concorrenza: un'uscita per ogni richiesta in corso più una di riserva, mai oltre il massimo; le uscite in eccesso rimaste inattive per 60 secondi vengono ritirate. La prima risposta 429 o 403 ritira subito l'uscita e una uscita pre-registrata ne prende il posto; una richiesta per modelli gratuiti continua a cambiare uscita e restituisce l'ultimo 429 al client solo dopo aver esaurito `retry.max_attempts`, mentre l'uscita ritirata viene registrata di nuovo subito in background. Le registrazioni restano serializzate; il 429/Retry-After di Cloudflare detta il ritmo al posto di un intervallo fisso. Se il colo assegna meno blocchi di uscita distinti del preload, il pool arretra invece di registrare all'infinito; le probe automatiche dei modelli gratuiti saltano un pool senza uscita di riserva e fermano il ciclo dopo che una probe ha ritirato un'uscita. `warp.stack` seleziona `ipv6` (predefinito), `ipv4` o `dual`.

La quota gratuita dell'upstream è legata al **blocco di uscita**, quindi la rotazione serve solo se la nuova uscita proviene da un blocco diverso: un'uscita IPv6 è identificata e raffreddata per 30 minuti dal suo `/64`, una IPv4 dal suo `/32`. L'uscita IPv4 di Cloudflare WARP è di solito un unico indirizzo NAT condiviso, spesso già esaurito da altri utenti; lo stack IPv6 ricava l'uscita dall'indirizzo sorgente WireGuard esterno. Imposta `warp.source_ipv6` su un prefisso IPv6 instradato verso l'host (di solito un `/64`, lo stack predefinito è già `ipv6`) perché ogni uscita associ il proprio indirizzo sorgente da quel prefisso. Su Linux l'indirizzo viene associato con `IPV6_FREEBIND` (impostato dal gateway) oppure deve essere già assegnato a un'interfaccia. La famiglia degli endpoint non viene verificata in anticipo: l'handshake prova prima gli endpoint IPv6 e ripiega su quelli IPv4 quando nessuno risponde (in quel caso l'uscita è l'indirizzo condiviso). Lasciando vuoto il campo si usa l'indirizzo sorgente predefinito dell'host. Su reti con il pool di uscite WARP già saturo anche una nuova uscita può rispondere 429: il gateway continua a ruotare e restituisce un 429 solo esaurito il budget di tentativi. `/healthz` restituisce un blocco `warp` e risponde 503 quando WARP è attivo ma nessuna uscita è operativa; `no_healthy_proxies` blocca la readiness solo con WARP disattivato o quando servono proxy per le chiavi Go. Le richieste diagnostiche acquisiscono un'uscita ma non la ruotano né la penalizzano. Disattivando WARP, i canali gratuiti tornano a usare i proxy.

### System prompt ufficiale

Ogni richiesta di un modello gratuito già normalizzata porta il system prompt della CLI OpenCode — istruzioni di base, blocco `<env>` ed elenco delle skill integrate — perché il canale gratuito verifica che il traffico abbia forma di agent. Il canale v1 `/zen` usa i nomi degli strumenti 1.x; `/inference` e `/zen/go` usano quelli 2.x. Il blocco ambiente indica il modello richiesto, la directory di lavoro del gateway, la piattaforma e la data UTC.

L'iniezione dipende dal protocollo: i messaggi system e developer di Chat vengono fusi in un unico messaggio `system` iniziale; Anthropic riceve un valore `system` (stringa, oppure un blocco di testo iniziale quando il chiamante invia blocchi); Responses riceve `instructions`. Se il contenuto system contiene già il marcatore `You are opencode, an interactive CLI tool`, la richiesta passa invariata, quindi un client che invia già il prompt ufficiale non ne riceve una seconda copia. I modelli gratuiti serviti da una chiave Zen o Go ricevono la stessa normalizzazione, ma escono comunque dal proxy di quel tier.

### Normalizzazione degli strumenti

Su ogni richiesta di un modello gratuito già normalizzata il gateway rinomina ogni strumento nominato del client in `clint_<name>` — i caratteri fuori da `A-Za-z0-9_-` diventano `_`, i nomi sono limitati a 64 byte e le collisioni ricevono il suffisso `_2`, `_3`, … — e aggiunge l'intero set ufficiale del canale: le definizioni 1.x (`bash`, `edit`, `glob`, `grep`, `read`, `skill`, `task`, `todowrite`, `webfetch`, `websearch`, `write`) su `/zen` e il set di 13 strumenti 2.x su `/inference` e `/zen/go`. Le richieste senza `tools`, con un array vuoto o con il vecchio array `functions` ricevono lo stesso set; `tool_choice`, il vecchio `function_call` e la cronologia delle tool call vengono riscritti di conseguenza. Gli strumenti integrati di Responses e quelli server di Anthropic restano invariati.

Le risposte vengono riportate ai nomi dichiarati dal client, sia in streaming sia non, incluso un nome Chat suddiviso su più chunk. Se l'upstream chiama uno strumento ufficiale che il client non ha dichiarato, il nome passa invariato: il gateway non riproduce né nasconde le chiamate ufficiali.

### Disponibilità dei modelli gratuiti

Il gateway verifica con una breve richiesta e il protocollo nativo i modelli il cui ID contiene `free` (senza distinzione tra maiuscole e minuscole) oppure i cui metadati indicano un costo input/output pari a zero e che non sono deprecati. Prova prima il canale anonimo abilitato, poi una chiave Zen configurata. Con `warp.enabled`, ogni canale di probe esce tramite WARP invece che da un proxy; se non è possibile acquisire un'uscita il risultato è `warp_unavailable` e resta inconcludente. Una risposta valida e completa su uno dei canali mantiene il modello abilitato. Il modello viene disabilitato nel rilevamento e nell'instradamento solo quando tutti i canali provati indicano esplicitamente che non esiste o è stato ritirato. Errori di autenticazione, limiti di frequenza, timeout, errori di trasporto, risposte 5xx e risposte malformate non sono conclusivi: non disabilitano nuovi modelli e vengono verificati di nuovo dopo un'ora. I controlli non alterano cooldown, stato dei proxy o statistiche delle richieste di produzione, ma raggiungono comunque l'upstream e possono consumare la quota gratuita.

I modelli System One usano una domanda nativa `noul` che chiede se 1 + 1 è uguale a 2; non ricevono mai payload chat/tool. Il rilevamento dei modelli include `native_protocol` e `supported_endpoints`; gli alias di rotazione sono elencati solo quando esistono chiavi Go e modelli idonei. Le disabilitazioni legacy della versione v1.3.6 vengono cancellate al caricamento e verificate subito, perché quei controlli usavano ID sessione non validi.

Gli intervalli sono fissi: i modelli abilitati vengono verificati ogni **1 ora**, quelli confermati non disponibili ogni **24 ore**. Una verifica inconclusiva di un modello disabilitato conserva il motivo originale e pianifica un nuovo tentativo dopo 24 ore. Una verifica riuscita riabilita il modello automaticamente. La pagina WebUI **Disponibilità modelli gratuiti** può ripristinarlo subito, mettendolo sotto controllo manuale: resta abilitato e viene escluso dai controlli automatici, così i risultati successivi non possono sovrascrivere la scelta. Usa l'interruttore per modello **manuale/automatico** nella stessa pagina per riprendere i controlli automatici; il tick successivo dello scheduler pianifica una verifica. I record `manually_enabled` delle versioni precedenti migrano al controllo manuale al caricamento. Un controllo già in corso non può annullare un ripristino manuale. I modelli senza un canale Zen/anonimo utilizzabile o senza un protocollo di inferenza supportato vengono ignorati.

Il controllo manuale sospende anche il rilevamento dei livelli per `reasoning.effort=auto`; i livelli appresi smettono di essere forzati dopo due ore senza verifica. Tornando al controllo automatico, il rilevamento può riprendere.

Lo stato persiste tra i riavvii in `config.json.<upstream-fingerprint>.availability.json`, separato per URL upstream Zen. Lo scheduler in background verifica ogni minuto i modelli in scadenza, ignora quelli sotto controllo manuale e controlla gli altri in sequenza, con un timeout di 60 secondi per canale. Se non sono disponibili canali, non avviene alcuna disabilitazione automatica.

Endpoint di amministrazione (richiede l'accesso; le scritture richiedono anche CSRF): `GET /api/models/availability`, `POST /api/models/restore` con `{"model":"MODEL_ID"}` (ripristina e attiva il controllo manuale) e `POST /api/models/manual` con `{"model":"MODEL_ID","manual":true|false}` per passare dal controllo manuale a quello automatico e viceversa.

### Sessioni e proxy

Inizialmente le chiavi vengono distribuite tra i proxy. Il traffico reale può avviare controlli dei proxy, riassegnazioni delle chiavi e cooldown. I proxy non integri vengono ricontrollati ogni 15 minuti tramite Cloudflare Trace.

Un hash di sessione stabile seleziona la chiave o il proxy anonimo preferito. Invia un `x-session-id` esplicito per separare conversazioni indipendenti. Il gateway accetta anche `x-opencode-session`, `x-session-affinity`, `conversation-id`, `conversation_id` e `metadata.session_id`.

Senza un ID esplicito, il primo messaggio dell'utente viene usato come seme. Conversazioni che iniziano con lo stesso contenuto possono quindi condividere l'affinità. Modificare i membri del pool può cambiare il nodo selezionato.

I cooldown aumentano esponenzialmente fino a otto volte `performance.failure_cooldown_seconds`; viene rispettato anche un valore `Retry-After` più lungo. Se tutte le chiavi autenticate sono in cooldown, l'instradamento può provare quella il cui cooldown termina prima. I nodi anonimi ancora in cooldown vengono ignorati.

## Riferimento della configurazione

Consulta [config.example.json](config.example.json) per una configurazione iniziale completa. Il JSON supporta i commenti `//` e `/* ... */`; i campi sconosciuti e i valori non validi vengono rifiutati.

### Chiavi, indirizzi di ascolto e instradamento

| Campo                       | Predefinito / requisito                                                       |
| --------------------------- | ----------------------------------------------------------------------------- |
| `listen`                    | `127.0.0.1:8080`.                                                             |
| `server_keys`               | È richiesta almeno una chiave locale.                                        |
| `zen_keys`, `go_keys`       | È richiesta una chiave upstream, salvo accesso anonimo abilitato.            |
| `anonymous`                 | `false`.                                                                      |
| `warp.enabled`              | `false`; il traffico dei canali gratuiti esce via Cloudflare WARP, senza proxy. |
| `warp.pool_size`            | `2`; limite massimo di uscite WARP (metà pre-registrata in background; il ring attivo segue la concorrenza), 1–32. |
| `warp.stack`                | `ipv6`; `ipv6`, `ipv4` o `dual`.                                              |
| `warp.source_ipv6`          | vuoto; prefisso IPv6 (`/64`) da cui ogni uscita associa la sorgente esterna; richiede `stack = "ipv6"`. |
| `prefer`                    | `go`; accetta `go` o `zen`.                                                   |
| `upstream.zen`              | `https://opencode.ai/inference/openai`.                                       |
| `upstream.go`               | `https://opencode.ai/zen/go`.                                                 |
| `proxies`                   | Fallback a `["direct"]` se entrambe le fonti proxy sono vuote.              |
| `proxyfile`                 | Opzionale; i percorsi relativi sono risolti accanto al file di configurazione. |
| `models.refresh_seconds`    | `300`; minimo 1.                                                              |
| `models.protocols`          | `{}`; override del protocollo nativo per modello.                             |
| `models.free_only`          | `false`; mostra e instrada solo modelli gratuiti, anche con chiavi Zen o Go.  |
| `reasoning.effort`          | Vuoto (disattivato); `minimal`, `low`, `medium`, `high`, `xhigh`, `max` o `none`. |
| `reasoning.effort_by_model` | `{}`; override di `reasoning.effort` per modello.                             |

### Livello di ragionamento forzato

`reasoning.effort` imposta il livello usato dall'upstream quando la richiesta non ne specifica uno; `reasoning.effort_by_model` lo sovrascrive per gli ID indicati. Si applica a tutti e tre i protocolli e in entrambe le direzioni di conversione. È particolarmente utile per il traffico tra protocolli: `thinking.budget_tokens` di Anthropic può essere approssimato solo dalle interfacce Chat e Responses, quindi un livello forzato può raggiungere un livello che il solo budget non esprime.

```json
{
  "reasoning": {
    "effort": "high",
    "effort_by_model": { "claude-opus-5": "max" }
  }
}
```

Le disattivazioni esplicite del client restano attive durante la conversione tra protocolli: `thinking.type="disabled"` nativo di Anthropic e l'effort `"none"` di Chat/Responses non possono essere riattivati da un valore predefinito configurato.

I livelli vengono riportati fedelmente in entrambe le direzioni. Un `output_config.effort` esplicito sopravvive alla conversione e non viene sovrascritto da un blocco `thinking`; `budget_tokens` conserva il proprio livello (`8192` corrisponde a `high`, `32000` a `xhigh`), e la destinazione Responses trasporta il livello in `reasoning.effort`.

Il Centro configurazione WebUI espone gli stessi campi predefiniti e per modello. L'API di gestione li restituisce in `reasoning` da `GET /api/config` e accetta l'oggetto con `PUT /api/config`; se il client omette il campo, il valore esistente resta invariato, mentre un oggetto vuoto lo cancella.

Le voci proxy accettano `direct`, `http://`, `https://`, `socks5://` e `socks5h://`, incluse credenziali nell'URL. L'elenco inline viene unito a `proxyfile` e deduplicato mantenendo l'ordine.

Un file proxy contiene una voce per riga e supporta righe vuote e commenti:

```text
# Proxy principale
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # Proxy di riserva
direct
```

I marcatori di commento sono `#`, `;` e `//` all'inizio della riga o dopo uno spazio.

#### Pool di proxy rotanti

Aggiungi il suffisso `#pool` (o il prefisso `pool+`) a una voce per indicare un pool di proxy rotante come [Resin](https://github.com/Resinat/Resin), in cui ogni nuova connessione TCP esce da un IP diverso:

```text
http://127.0.0.1:7890#pool
socks5://127.0.0.1:1080#pool
pool+http://127.0.0.1:7891
```

Una voce pool viene gestita diversamente da un proxy statico:

| Comportamento             | Proxy statico                                                       | Voce `#pool`                                                               |
| ------------------------- | ------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| Riutilizzo connessioni    | Keep-alive e multiplexing HTTP/2 (impostazioni `performance.*`)     | Nuovo tunnel TCP + CONNECT per richiesta (`DisableKeepAlives`, senza HTTP/2) |
| Cooldown dopo un errore   | Backoff esponenziale 15–120 s, esteso da `Retry-After`              | Nessuno: la richiesta successiva usa un nuovo IP di uscita                 |
| Esclusione per stato      | Segnalato come non integro, chiavi riassegnate, verifica ogni 15 min | Mai escluso; il controllo di integrità viene saltato                       |
| Retry del canale anonimo  | Al massimo un tentativo per proxy configurato                       | Fino a `retry.max_attempts`, ruotando la voce del pool                     |

Usalo quando l'endpoint dietro l'URL esegue già la rotazione e i singoli IP di uscita sono soggetti a limiti o instabilità. Il gateway tratterà ogni tentativo come un endpoint nuovo invece di fissare la conversazione a un solo IP. Basta una voce `#pool`: non serve elencare decine di indirizzi. Le voci statiche mantengono il comportamento del pool di connessioni descritto sotto.

### Timeout e pool di connessioni

| Campo                                      | Predefinito | Descrizione                                                |
| ------------------------------------------ | ----------- | ---------------------------------------------------------- |
| `retry.max_attempts`                       | `3`         | Tentativi per tier autenticato, incluso il primo.         |
| `retry.timeout_seconds`                    | `300`       | Timeout totale di inferenza, compreso lo stream.          |
| `performance.attempt_timeout_seconds`      | `0`         | Limite per tentativo; 0 usa il timeout della richiesta.   |
| `performance.connect_timeout_seconds`      | `5`         | Timeout per stabilire la connessione.                     |
| `performance.failure_cooldown_seconds`     | `15`        | Cooldown di errore di base.                               |
| `performance.max_idle_conns`               | `2048`      | Limite connessioni inattive per trasporto proxy.           |
| `performance.max_idle_conns_per_host`      | `256`       | Limite connessioni inattive per host e trasporto.          |
| `performance.max_conns_per_host`           | `0`         | Limite connessioni per host; 0 significa illimitato.       |
| `performance.idle_conn_timeout_seconds`    | `120`       | Durata delle connessioni inattive.                        |

Il timeout degli header per tentativo è limitato dal timeout della richiesta. Impostalo sotto il timeout totale se vuoi lasciare tempo per il fallback in caso di upstream lento. La scadenza del contesto della richiesta interrompe i tentativi successivi senza penalizzare chiavi o proxy non usati.

### Log e gestione

| Campo                         | Predefinito / requisito                                  |
| ----------------------------- | -------------------------------------------------------- |
| `logging.level`               | `info`; accetta `debug`, `info`, `warn`, `error`.        |
| `logging.ring_size`           | `2000`; intervallo 100–50.000.                           |
| `logging.dump_request_bodies` | `false`; richiede il livello `debug` per emettere i body in uscita. |
| `webui.enabled`               | `false` se omesso; la configurazione di esempio usa `true`. |
| `webui.listen`                | `0.0.0.0:8081`.                                          |
| `webui.username`              | Obbligatorio con WebUI attiva; l'esempio usa `admin`.    |
| `webui.password`              | Password iniziale, almeno 10 caratteri.                 |
| `webui.password_hash`         | Hash Argon2id generato, sostituisce la password in chiaro. |
| `webui.session_ttl_minutes`   | `720`; intervallo 5–10.080.                              |

All'avvio, la password iniziale viene sottoposta a hash e rimossa dalla configurazione. Il backup che contiene la password in chiaro viene eliminato. Proteggi i file di configurazione e i backup: le chiavi upstream e le credenziali proxy restano segreti necessari.

I dump dei body sono opzionali e limitati a 64 KiB per body preparato. I segreti configurati vengono oscurati, ma il contenuto delle conversazioni può essere sensibile.

## WebUI e diagnostica

Il listener di gestione serve la UI e le route `/api/*`. L'autenticazione usa un account amministratore, sessioni lato server, cookie HttpOnly/SameSite, controlli CSRF per le scritture e limitazione dei tentativi di accesso.

| Route di gestione                                  | Scopo                                                                  |
| -------------------------------------------------- | ---------------------------------------------------------------------- |
| `POST /api/auth/login`                             | Accesso e ottenimento del cookie di sessione e del token CSRF.         |
| `GET /api/auth/session`, `POST /api/auth/logout`   | Verifica o chiusura della sessione.                                    |
| `GET /api/config`, `PUT /api/config`               | Lettura della configurazione mascherata o applicazione delle modifiche. |
| `POST /api/config/reload`                          | Ricaricamento del file dal disco.                                      |
| `POST /api/config/reveal`                          | Visualizzazione dei segreti configurati dopo verifica della password.  |
| `PUT /api/account`                                 | Aggiornamento delle credenziali dopo la verifica; invalida le sessioni. |
| `GET /api/monitor`                                 | Statistiche su richieste, token, upstream e risorse.                    |
| `GET /api/debug/models`                            | Route dei modelli, impronte delle chiavi e diagnostica dei metadati.   |
| `POST /api/debug/inference`                        | Esecuzione di una richiesta Playground.                                |
| `GET /api/logs`, `GET /api/logs/stream`            | Log recenti o iscrizione SSE in tempo reale.                            |

Le richieste Playground richiedono una sessione e `X-CSRF-Token`; il limite è 12 al minuto per IP client:

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

Il server forza `stream: false`. Usa `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}` per provare una chiave specifica di `/api/debug/models`. Una richiesta con chiave selezionata esegue al massimo un tentativo upstream, senza accesso anonimo, rotazione delle chiavi, fallback di tier o ripetizione del ragionamento.

**Le diagnostiche automatiche e quelle con chiave selezionata preservano cooldown, conteggi di errore, stato dei proxy e associazioni delle chiavi di produzione.** Inviano comunque richieste reali all'upstream, possono consumare la quota del provider e compaiono nel monitoraggio.

Al termine, l'endpoint di gestione restituisce HTTP 200 e il risultato effettivo nei campi `ok`, `http_status`, `request_id`, `route` e `response`. I risultati con chiave selezionata includono anche `selected_key` e `key_test`: `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error` o `unavailable`. Input di gestione non validi, errori di autenticazione o CSRF e richieste limitate mantengono i rispettivi codici HTTP di errore.

### Lingua dell'interfaccia

La WebUI carica le traduzioni da `webui/js/i18n.js`. Include **简体中文** (predefinito), **English**, **Tiếng Việt** e **Italiano**. I testi originali in cinese semplificato restano in `index.html` e `webui/js/` e vengono usati come fallback per ogni stringa non tradotta; aggiungere una lingua non modifica quindi il testo originale.

- Il selettore si trova nella barra superiore e nella scheda di accesso.
- La scelta è salvata in `localStorage` con la chiave `opencode2api.ui.lang`.
- Il cambio lingua aggiorna i testi statici (`data-i18n`, `data-i18n-attr`) e quelli generati da JavaScript senza ricaricare la pagina.
- Date e orari seguono la lingua selezionata.

Per aggiungere una lingua, inserisci un blocco in `DICTIONARY` in `webui/js/i18n.js`, registra il codice in `SUPPORTED`, `LABELS`, `FLAGS` e `DATE_LOCALES` e aggiungilo alla catena di rilevamento `browserLocale`.

### Salvataggio e ricaricamento

Il server convalida una configurazione candidata e prepara un gateway sostitutivo prima di salvare e attivare le modifiche. Se la convalida o il salvataggio falliscono, il gateway attivo resta invariato. Le richieste già in corso continuano sul gateway originale.

Chiavi, proxy, URL upstream, retry, impostazioni dei modelli, log e preferenze di instradamento si applicano subito alle nuove richieste. Le modifiche a `listen`, `webui.listen` o `webui.enabled` richiedono il riavvio del processo. Il JSON salvato è normalizzato e non conserva i commenti.

## Monitoraggio e persistenza

L'esito di una richiesta descrive l'inferenza completata. Una risposta SSE non riuscita o uno stream troncato viene conteggiato come errore anche se HTTP 200 è già stato inviato; l'esito è `stream_error`. L'annullamento da parte del client viene registrato come `client_canceled`. La WebUI mostra questi esiti insieme al codice di stato HTTP.

I record dei **tentativi** upstream misurano lo scambio HTTP fino alla ricezione degli header della risposta. Esito e latenza sono separati dal completamento dell'intero body JSON o dello stream.

Le statistiche dei token usano solo i valori riportati. I token input includono letture e scritture della cache; i token cache separati contano le letture. I valori mancanti non vengono stimati; la copertura indica quante richieste di inferenza instradate riportano l'utilizzo. `usage.server_keys` suddivide richieste e token in base alla chiave server locale usata per l'autenticazione (la relativa impronta è mostrata in `/api/api-keys`); il traffico Playground è conteggiato sotto `playground`.

| Dati                                                     | Memorizzazione / conservazione                                                       |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| Sessioni, metriche, totali token, ultimo risultato Playground | Memoria del processo; cancellati al riavvio.                                    |
| Dettagli di richieste e tentativi                        | Ultima ora, massimo 10.000 richieste e 20.000 tentativi; API fino a 500 per tipo.   |
| Buffer circolare dei log                                 | Memoria del processo; dimensione impostata da `logging.ring_size`.                  |
| Log strutturati                                          | JSON su stdout; raccoglili esternamente per conservarli.                            |
| Configurazione e versione precedente                     | `config.json` e `config.json.bak`.                                                   |
| Cache del catalogo modelli                               | `config.json.models.catalog.json`.                                                  |
| Cache prezzi/deprecazioni                                | `config.json.models.dev.json`.                                                      |

I nomi dei file di cache dipendono dal percorso effettivo della configurazione. I contatori di durata si riferiscono al processo corrente. Le istanze non condividono sessioni o stato di monitoraggio.

### Health check

`/healthz` non incrementa i contatori di monitoraggio né avvia richieste di rete. Riporta il numero di risorse senza esporre chiavi o indirizzi proxy.

- HTTP 503: catalogo iniziale ancora in attesa, nessun modello instradabile con la configurazione attuale oppure nessun proxy integro.
- HTTP 200: servizio pronto, anche se una cache del catalogo utilizzabile è obsoleta. In questo caso lo stato del modello è `stale` e lo stato complessivo è `degraded`.

La soglia di obsolescenza è il doppio di `models.refresh_seconds`, con un minimo di 60 secondi. Il controllo verifica rilevamento e risorse; non verifica se una chiave upstream accetterà la prossima inferenza.

## Sviluppo

Il codice Go è organizzato in package per responsabilità. Il comando collega i componenti del servizio; i package di implementazione si trovano sotto `internal/`.

```text
cmd/
  opencode2api/main.go    Flag CLI, avvio e arresto graduale
internal/
  admin/                  API di gestione, sessioni di accesso e Playground
  buildinfo/              Versione condivisa da health check e API di gestione
  config/                 Configurazione, persistenza, password e oscuramento
  gateway/                Instradamento HTTP, retry, pool, aggiornamenti e runtime
  httpx/                  Risposte HTTP condivise, gestione dei body e header
  identity/               ID richiesta e affinità di sessione
  jsonutil/               Accesso e decodifica JSON
  models/                 Catalogo modelli, capacità, prezzi e cache
  protocol/               Conversione richieste/risposte e parsing/emissione SSE
  telemetry/              Tracciamento richieste, metriche, log e recupero
webui/
  embed.go                Incorpora le risorse statiche nell'eseguibile
  index.html              Struttura delle pagine
  css/                    Token di design, stili base, componenti e layout
  js/                      Moduli ES: shell, i18n, helper condivisi e un modulo per pagina
```

Ordine di lettura consigliato:

1. [Punto di ingresso](cmd/opencode2api/main.go) → [gestione runtime](internal/gateway/runtime.go) → [handler HTTP](internal/gateway/gateway.go).
2. [Tentativi upstream](internal/gateway/upstream.go) e [instradamento modelli](internal/models/catalog.go) spiegano come una richiesta raggiunge il provider.
3. [Conversione richieste](internal/protocol/request.go), [conversione risposte](internal/protocol/response.go) e [trasporto streaming](internal/protocol/stream.go) descrivono il comportamento dei protocolli.
4. [Route di gestione](internal/admin/server.go) e [logica WebUI](webui/js/main.js) trattano configurazione e diagnostica.

Esegui formattazione, analisi e build Go:

```bash
gofmt -w cmd internal webui
go vet ./...
go build -o opencode2api ./cmd/opencode2api
```

Per lo sviluppo locale, avvia il servizio con `go run ./cmd/opencode2api -config config.json`. Le build di release iniettano la versione con `-ldflags "-X main.version=vX.Y.Z"`.

Node.js serve solo per controllare la sintassi JavaScript della WebUI:

```bash
for f in webui/js/*.js webui/js/pages/*.js; do node --input-type=module --check < "$f"; done
```

`.editorconfig`, `.gitattributes` e la formattazione Go standardizzano il codice. La CI esegue `go vet` e build con Go 1.24 e la versione stabile su Linux/Windows, oltre ai controlli di formattazione Go, sintassi WebUI e sintassi shell dell'entrypoint. Gli archivi di release includono tutti e quattro i README.

## Risoluzione dei problemi

| Sintomo                                  | Verifica                                                                                 |
| ---------------------------------------- | ---------------------------------------------------------------------------------------- |
| L'API restituisce 401                    | Usa un valore `server_keys` locale configurato.                                         |
| Health resta `starting`                   | Controlla i log di aggiornamento del catalogo e la connettività in uscita; aggiungi override di protocollo se necessario. |
| Non vengono esposti modelli               | Verifica i tier configurati, l'idoneità anonima e il supporto del protocollo nativo.      |
| Le richieste restituiscono 502/504        | Controlla tentativi upstream, credenziali, proxy e timeout totali/per tentativo.         |
| HTTP 200 ma generazione non riuscita      | Controlla l'evento di errore SSE e l'esito della richiesta, non solo lo stato HTTP.       |
| Le modifiche host non hanno effetto in Docker | Il file attivo è nel volume di stato; importalo di nuovo o usa la WebUI.              |
| Le porte del container non sono raggiungibili | Controlla indirizzi di ascolto, porte pubblicate e configurazione del volume attivo. |
| Il monitoraggio scompare dopo il riavvio   | È conservato solo in memoria; raccogli esternamente i log stdout.                        |

## Ringraziamenti

Grazie alla community [LINUX DO](https://linux.do) per il supporto.

### Livello di ragionamento adattivo

Imposta `reasoning.effort` (o un override per modello) su `auto` per usare il livello più alto verificato con successo dal controllo dei modelli gratuiti. I livelli concreti mantengono il proprio significato e il livello specificato dal client ha la precedenza. Non viene mantenuta una lista di modelli consentiti.

Dopo il successo di un livello di base, i modelli con capacità di ragionamento vengono verificati nell'ordine `max`, `xhigh`, `high`, `medium`, `low`, `minimal`. Si passa a un livello inferiore solo se un parametro di ragionamento viene rifiutato esplicitamente. Gli errori transitori interrompono la ricerca e non vengono interpretati come un limite di capacità inferiore. Se tutti i livelli vengono rifiutati, non viene applicato alcun override di ragionamento.

Le osservazioni sono persistite per URL upstream, modello e protocollo nativo, mostrate nella pagina di disponibilità e usate per non più di due ore. I controlli attivi coprono solo modelli Zen/anonimi gratuiti; modelli Go/a pagamento, capacità sconosciute e livelli non verificati non ricevono override automatici. Impostare `auto` non amplia l'ambito dei controlli e non garantisce che gli upstream applichino ogni campo accettato. Il controllo può richiedere diverse piccole richieste per modello e consumare la quota upstream.

## Licenza

SPDX-License-Identifier: GPL-3.0-or-later

Questo progetto è software libero: puoi ridistribuirlo e modificarlo secondo la GNU General Public License pubblicata dalla Free Software Foundation, versione 3 o, a tua scelta, una versione successiva. Il software è fornito senza garanzie. Consulta [LICENSE](LICENSE) per i termini completi.
