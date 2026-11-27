# Console di gestione e guida operativa

[English](management.md) | [简体中文](management.zh-CN.md) | [Tiếng Việt](management.vi.md) | Italiano

Questa guida descrive le funzioni di gestione attualmente implementate, OAuth, quote, rotazione dei modelli, diagnostica e persistenza. Per avvio del servizio ed esempi delle API di inferenza consulta il [README in italiano](../README.it.md); il punto di partenza per la configurazione completa è [config.example.json](../config.example.json). La gestione e le API di inferenza usano listener distinti; tutte le rotte `/api/*` riportate qui appartengono alla gestione.

## Avvio e accesso

Su Linux usa Go 1.24 o successivo e compila nella directory del progetto con Bash:

```bash
if [ ! -f config.json ]; then
  cp config.example.json config.json
fi
export GOCACHE="$PWD/.local/go-cache"
export GOMODCACHE="$PWD/.local/go-modules"
go build -trimpath -o opencode2api ./cmd/opencode2api
```

Prima dell'avvio modifica la configurazione localmente: sostituisci `server_keys` e `webui.password`, poi imposta `zen_keys` o `go_keys`. Per la gestione locale è consigliato `webui.listen: "127.0.0.1:8081"`; il valore di esempio `0.0.0.0:8081` ascolta su tutte le interfacce. La password deve avere almeno 10 caratteri; il nome amministratore di esempio è `admin`.

```bash
./opencode2api -config config.json
```

Su Windows puoi compilare in modalità nativa con `go build -trimpath -o opencode2api.exe ./cmd/opencode2api`, poi eseguire `.\opencode2api.exe -config .\config.json` in PowerShell. Per inizializzazione dei volumi, importazione dei file, porte e immagini Docker/Compose consulta il README. Le operazioni di gestione devono riferirsi alla configurazione effettivamente in uso: modificare una copia sull'host senza importarla nuovamente non aggiorna il servizio.

Il canale anonimo `anonymous` è disabilitato per impostazione predefinita. Per avviare il servizio occorre almeno una chiave upstream, una connessione Go/Zen OAuth già salvata oppure l'abilitazione esplicita dell'accesso anonimo. Non è possibile avviare senza credenziali e completare soltanto dopo il primo OAuth dalla WebUI. L'upstream può bloccare il traffico anonimo; non abilitare l'inferenza anonima soltanto per controllare i proxy.

La directory della configurazione deve essere scrivibile. Al primo avvio il servizio migra `webui.password` in `password_hash` Argon2id e tenta di eliminare il backup che contiene la password iniziale in chiaro. Questa operazione non cifra le altre credenziali.

## Pagine e autenticazione amministrativa

| Pagina | Indirizzo | Funzione |
| --- | --- | --- |
| Panoramica | `/#overview` | Richieste, token, tentativi upstream e risorse operative. |
| Diagnostica | `/#diagnostics` | Instradamento dei modelli, Playground e log in tempo reale. |
| Guida all'accesso | `/#access` | Parametri di connessione dei client. |
| Gestione chiavi API | `/#api-keys` | Ciclo di vita e utilizzo delle chiavi locali. |
| Modelli e quota | `/#model-quotas` | Catalogo Go, quote condivise, due tipi di OAuth e disponibilità dei modelli gratuiti. |
| Rotazione dei modelli | `/#rotation` | Ordine dei gruppi, posizione corrente, pause e cronologia dei tentativi. |
| Configurazione | `/#config` | Chiavi, proxy, timeout, instradamento, WARP e log. |
| Sicurezza account | `/#account` | Modifica di nome e password dell'amministratore. |

L'interfaccia supporta cinese semplificato, inglese, vietnamita e italiano; lingua e tema sono memorizzati nel browser. I vecchi collegamenti `#playground` e `#logs` aprono Diagnostica; `#availability` apre Modelli e quota.

L'amministratore accede con nome utente e password e riceve un cookie di sessione `HttpOnly`, `SameSite=Strict`. Tutte le API di gestione tranne il login richiedono una sessione valida; le scritture richiedono anche `X-CSRF-Token`. Se presente, `Origin` deve corrispondere all'origine del servizio. Le risposte di login/sessione forniscono `csrf_token`. Le chiavi locali `server_keys` non sostituiscono il login amministrativo.

La sessione dura 720 minuti per impostazione predefinita, con intervallo consentito di 5–10.080 minuti. Modificare le credenziali amministrative o riavviare il processo invalida le sessioni esistenti; l'endpoint di modifica dell'account richiede nuovamente la password corrente. Il login ha un limite di frequenza; il Playground consente al massimo 12 richieste al minuto per client connesso direttamente. Il servizio non considera attendibile `X-Forwarded-For`, quindi i client dietro un reverse proxy condividono il limite del peer; con `-trust-cloudflare-client-ip`, i peer loopback (`cloudflared` locale) sono limitati per `CF-Connecting-IP`, IPv6 per /64. Un reverse proxy HTTPS deve impostare correttamente `X-Forwarded-Proto`, usato per il flag Secure del cookie e per il controllo dell'origine.

## Salvataggio e ricarica della configurazione

`GET /api/config` restituisce la configurazione operativa, ID sicuri/valori mascherati, listener effettivi e campi che richiedono un riavvio. Per aggiornare chiavi e proxy usa una delle seguenti rappresentazioni:

```json
{"id":"EXISTING_FINGERPRINT"}
```

```json
{"value":"NEW_VALUE"}
```

`PUT /api/config` accetta il modulo completo di configurazione, non un PATCH parziale; le liste di chiavi/proxy omesse possono essere considerate vuote. Per conservare un valore esistente invia il suo ID. Il salvataggio dalla pagina conserva le opzioni avanzate non modificate nel modulo. Credenziali OAuth, rotazione e password amministrativa sono gestite dagli endpoint dedicati; il salvataggio ordinario non accetta né sovrascrive token OAuth inviati dal browser.

Il salvataggio esegue, nell'ordine, validazione, costruzione del runtime candidato, persistenza e sostituzione del runtime. Se la validazione o la persistenza della configurazione falliscono, il runtime precedente continua a servire le richieste. Il file ammette commenti `//` e `/* ... */`, mentre le richieste API devono essere JSON standard. Campi sconosciuti, campi duplicati al primo livello, valori non validi e presenza simultanea dei campi OAuth vecchi e nuovi per lo stesso piano vengono rifiutati. Il salvataggio normalizza il file ed elimina i commenti.

Chiavi, proxy, upstream, timeout, modelli, preferenza, reasoning e log vengono applicati alle nuove richieste senza riavvio. `listen`, `webui.listen` e `webui.enabled` richiedono un riavvio; la pagina mostra sia i valori configurati sia i listener effettivi. Dopo una modifica manuale del file usa «Ricarica da disco» o `POST /api/config/reload`: il programma non sorveglia automaticamente il file.

`POST /api/config/reveal` richiede sessione amministrativa, CSRF e una nuova verifica della password. Può restituire chiavi manuali e valori dei proxy, ma non password, hash della password o token OAuth. Usalo soltanto quando occorre copiarli, evitando di scrivere il risultato nei log o condividerlo.

Valori predefiniti comuni per la gestione:

| Impostazione | Valore predefinito / intervallo |
| --- | --- |
| `webui.enabled` | Disabilitata nei valori incorporati; abilitata nell'esempio. |
| `webui.listen` | `0.0.0.0:8081`. |
| `logging.level` | `info`; supporta `debug`, `info`, `warn`, `error`. |
| `logging.ring_size` | `2000`; intervallo 100–50.000. |
| `logging.dump_request_bodies` | `false`; richiede anche debug, con un massimo di 64 KiB per corpo preparato. |
| `models.active_probes` | `false`; il traffico reale fornisce le osservazioni predefinite sulla disponibilità. |

## Go OAuth e Zen PAYG OAuth

«Modelli e quota» contiene due pannelli indipendenti. Go usa le rotte dell'abbonamento; Zen usa la fatturazione a consumo (PAYG) del workspace Console: **l'inferenza consuma i credits di quel workspace**. Accedere a Go non concede autorizzazioni o saldo Zen e non collega automaticamente PAYG.

Procedura:

1. Avvia il login nel pannello corrispondente; Zen richiede prima una conferma esplicita di PAYG.
2. Apri personalmente il collegamento ufficiale di verifica OpenCode Device OAuth, inserisci il `user_code` mostrato e conferma che l'autorizzazione sia stata richiesta da te. La pagina non reindirizza automaticamente.
3. Il backend effettua il polling all'intervallo restituito. Se sono presenti più organizzazioni, sospende il completamento automatico: scegli quella a cui appartengono abbonamento/credits e collega l'organizzazione selezionata. Sono ammesse soltanto le organizzazioni autorizzate da quel login.
4. Dopo il successo si aggiornano configurazione e vista dei modelli. In caso di errore o scadenza puoi annullare e ricominciare.

Non occorre incollare token, leggere file locali di credenziali o inviare URL di callback. Uscire dalla pagina o nascondere il browser interrompe il polling automatico; al ritorno viene riletto lo stato e un login non scaduto può proseguire. I login in attesa sono isolati per sessione amministrativa e piano: un ID non può essere riutilizzato tra piani. L'annullamento rimuove soltanto il login in attesa corrispondente; il logout amministrativo annulla i login in attesa di quella sessione, conservando le connessioni salvate.

| Operazione | Endpoint Go | Endpoint Zen | Corpo |
| --- | --- | --- | --- |
| Stato sicuro | `GET /api/go/auth` | `GET /api/zen/auth` | Nessuno. |
| Avvio del login | `POST /api/go/auth/start` | `POST /api/zen/auth/start` | Go: `{}`; Zen: `{"acknowledge_payg":true}`. |
| Polling / scelta organizzazione | `POST /api/go/auth/poll` | `POST /api/zen/auth/poll` | `{"id":"LOGIN_ID"}`; aggiungi `organization_id` per scegliere l'organizzazione. |
| Annullamento login in attesa | `DELETE /api/go/auth` | `DELETE /api/zen/auth` | Nessuno. |
| Disconnessione salvata | `DELETE /api/go/connection` | `DELETE /api/zen/connection` | Nessuno. |

Tutti gli endpoint richiedono una sessione, e le scritture richiedono CSRF. Login, stato, configurazione e reveal non restituiscono credenziali OAuth di accesso o rinnovo.

Le connessioni sono salvate rispettivamente in `go_oauth` e `zen_oauth`; il backend le rinnova e persiste quando necessario. Configurazione e backup contengono quindi credenziali sensibili di accesso/rinnovo: limita i permessi di lettura. I vecchi `go_subscription`/`zen_subscription` sono soltanto alias di migrazione al caricamento; il salvataggio usa i nuovi campi. La presenza dei campi vecchi e nuovi per lo stesso piano viene rifiutata.

Limiti dell'instradamento:

- Sono accettati soltanto i modelli effettivamente forniti dalla configurazione remota ufficiale autenticata e validati per host/percorso del piano e SDK supportato. Non si deduce PAYG eliminando `/go` dall'URL Go e non si accettano header di autenticazione remoti arbitrari. Le vecchie rotte `/zen` sono ammesse soltanto se fornite esplicitamente dalla configurazione remota.
- Credenziali, origine dei modelli e rinnovi di Go e Zen sono indipendenti. `prefer` decide il primo canale disponibile per lo stesso modello; scegliere `zen` consente alla connessione Zen OAuth di consumare credits.
- Una volta selezionato OAuth viene usato soltanto quel tier. Autorizzazione Go scaduta, quota esaurita, 429 o altri errori non causano un ripiego automatico su Zen PAYG, chiavi manuali o canale anonimo.
- `go_keys`/`zen_keys` manuali sono conservate ma non fungono da riserva finché esiste l'OAuth corrispondente, anche se occorre un nuovo login o non sono disponibili rotte.
- La disconnessione rimuove soltanto la connessione locale corrispondente, senza revocare la sessione ufficiale o influire sull'altra connessione. Seguono un nuovo rilevamento dei modelli manuali di quel tier e il rifiuto di dedurre rotte finché il rilevamento non è concluso. Con accesso anonimo disabilitato non puoi scollegare l'unica credenziale upstream rimasta: configura prima un'altra chiave/connessione.
- Quota residua OAuth dell'abbonamento e credits sono attualmente sconosciuti. Non vengono riutilizzate le quote delle chiavi manuali né dedotte disponibilità dal numero di modelli o dalle richieste riuscite.

L'implementazione si basa su snapshot del sorgente fissati: per Go, [v2 `4617210822bdbb31e5749751afbfb4584ccedac8`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8) e [dev `7745079647b65036fe142e09ccd8623b25be1878`](https://github.com/anomalyco/opencode/tree/7745079647b65036fe142e09ccd8623b25be1878); per Console/PAYG, [dev `03d848f7b8d71d31d3e56f2abd8b337efbc1bc32`](https://github.com/anomalyco/opencode/tree/03d848f7b8d71d31d3e56f2abd8b337efbc1bc32). La compatibilità del sorgente non certifica autorizzazione e inferenza reali. Cambiamenti di permessi, workspace o formato upstream possono richiedere un nuovo login o un aggiornamento dell'implementazione.

## Modelli, quote e disponibilità

`GET /api/model-quotas` restituisce modelli Go e riepilogo delle quote; i modelli seguono il catalogo del gateway. Per le chiavi Go manuali la query usa `upstream.go` configurato con `/v1/usage`, per impostazione predefinita `https://opencode.ai/zen/go/v1/usage`. Il browser non riceve la chiave upstream.

- Sono fornite finestre percentuali condivise su 5 ore mobili, settimana e mese, con orari di ripristino. Non sono disponibili saldi, importi o conteggi di richieste per singolo modello.
- Più chiavi possono condividere lo stesso piano: le quote non si sommano. La precisione delle percentuali dipende dall'upstream.
- I successi restano in cache 60 secondi, gli errori 15. In caso di errore si conserva l'ultimo valore riuscito, segnalato come storico; senza dati precedenti la quota resta sconosciuta.
- La pagina interroga ogni 60 secondi mentre è visibile; il polling periodico si ferma quando viene nascosta o abbandonata. Anche l'aggiornamento manuale rispetta la cache.
- Se esiste Go OAuth non vengono presi in prestito i dati delle chiavi Go manuali; quota OAuth e credits Zen restano sconosciuti.

Il formato della quota segue il [sorgente usage upstream](https://github.com/anomalyco/opencode/blob/dev/packages/console/app/src/routes/zen/go/v1/usage.ts). Non è una garanzia di interfaccia pubblica stabile: un cambiamento di formato produce uno stato sconosciuto/errore, senza inventare saldi.

La disponibilità dei modelli gratuiti deriva normalmente dalle richieste reali. Solo errori espliciti di modello inesistente o ritirato possono disabilitarlo; 401, 403, 429, errori generici 4xx/5xx, timeout, problemi di connessione e risposte malformate non bastano a dimostrare un ritiro. Il ripristino manuale mantiene il modello instradabile e lo esclude dai test automatici; la pianificazione riprende soltanto tornando al controllo automatico.

L'inferenza sintetica in background richiede `models.active_probes: true`. Lo scheduler controlla ogni minuto gli elementi da verificare ed esegue i test per modello e canale. I modelli normali/non determinabili sono in genere ricontrollati ogni ora, quelli confermati non disponibili ogni 24 ore; ogni canale ha un timeout di 60 secondi. La richiesta usa prima il canale anonimo già abilitato, poi le chiavi Zen disponibili; se WARP è attivo ne usa le uscite. Senza un canale adatto il controllo viene saltato. I test attivi consumano quota reale ma non modificano cooldown di chiavi/proxy o statistiche delle richieste di produzione.

| Endpoint di gestione | Scopo |
| --- | --- |
| `GET /api/models/availability` | Legge stato dei modelli gratuiti e risultati dei test reasoning. |
| `POST /api/models/probe` | Segna come da verificare i modelli a controllo automatico e risveglia lo scheduler; non aggira `active_probes`. |
| `POST /api/models/restore` | `{"model":"MODEL_ID"}`: ripristina il modello e lo assegna al controllo manuale. |
| `POST /api/models/manual` | `{"model":"MODEL_ID","manual":true}`; `false` ripristina il controllo automatico. |

`reasoning.effort: "auto"` usa il massimo livello per Zen gratuito/anonimo verificato recentemente con successo. Con test attivi disabilitati non apprende nuovi limiti; anche il controllo manuale sospende i test automatici. Le osservazioni scadono dopo due ore. Non vengono dedotti livelli per Go, modelli a pagamento o capacità sconosciute; un effort esplicito del client ha sempre precedenza.

## Chiavi API locali

La gestione riguarda le `server_keys` del gateway, separate dalle chiavi upstream OpenCode. L'endpoint di creazione restituisce la nuova chiave completa; la lista restituisce soltanto impronta, nome, maschera, stato, data e utilizzo. Dopo la creazione la pagina permette di copiare chiave e variabili d'ambiente del client.

| Endpoint | Richiesta e risultato |
| --- | --- |
| `GET /api/api-keys` | Lista sicura. |
| `POST /api/api-keys` | `{"name":"client-a"}` genera una chiave; `value` consente un valore personalizzato. Successo: 201. |
| `PATCH /api/api-keys/{id}` | `{"name":"client-b","enabled":false}`; puoi inviare soltanto i campi da modificare. |
| `DELETE /api/api-keys/{id}` | Elimina la chiave locale. |

Le chiavi automatiche usano 192 bit casuali. Quelle personalizzate hanno lunghezza 16–256 e ammettono lettere, cifre, `.`, `_`, `-`; i nomi hanno 1–80 caratteri e la creazione ammette al massimo 200 chiavi. Deve restare almeno una chiave abilitata. Disabilitazione/eliminazione restituiscono immediatamente 401 alle nuove richieste; quelle in corso potrebbero aver già completato l'autenticazione.

`server_key_metadata` memorizza per impronta nome, stato di abilitazione e data di creazione; le vecchie `server_keys` restano compatibili automaticamente. L'utilizzo mostra ultima ora e totale del processo corrente, classificando richieste e token per chiave usata nell'autenticazione. Il Playground è registrato separatamente come `playground`, senza addebito alla chiave locale presa in prestito. Le statistiche sopravvivono alla ricarica a caldo e si azzerano al riavvio; non forniscono quote indipendenti, autorizzazioni per modello o scadenze.

## Proxy e WARP

La configurazione supporta importazione multipla di proxy inline, controlli individuali e rimozioni in bozza. Il contenuto di `proxyfile` va gestito nel file di origine; i percorsi relativi sono risolti rispetto alla directory di configurazione. La rimozione rimane una bozza e diventa effettiva solo salvando la configurazione completa.

`POST /api/proxies/check` richiede sessione e CSRF; scegli esattamente una delle due richieste:

```json
{"id":"SAVED_PROXY_ID"}
```

```json
{"value":"https://proxy.example:8443"}
```

L'ID del proxy salvato proviene da `GET /api/config`; `value` serve per le bozze. Ogni controllo ha un timeout di 20 secondi. La pagina consente al massimo quattro controlli concorrenti, il server otto; oltre il limite restituisce 429 senza accodare. Un controllo completato restituisce HTTP 200 con `ok`, `state`, `reason`, `latency_ms` e, se disponibile, `status`; errori di autenticazione, CSRF o formato usano i rispettivi stati HTTP.

Il controllo verifica soltanto se il proxy selezionato raggiunge l'upstream Zen configurato. Un client isolato invia attraverso quel proxy una GET non autenticata a `upstream.zen` con `/v1/models`: non invia chiavi Zen/Go, `public` o OAuth, non esegue inferenza, non segue redirect, non usa WARP e non modifica salute di produzione, cooldown delle chiavi o binding. Funziona anche con accesso anonimo disabilitato.

| `state` | Interpretazione |
| --- | --- |
| `available` | Upstream 200–399 oppure 401; `authentication_required` su 401 prova solo l'arrivo a destinazione, non la validità della chiave. |
| `unavailable` | Connessione rifiutata dal proxy/DNS inesistente confermati nel controllo (`proxy_unreachable`), oppure 407 del tunnel proxy (`proxy_authentication_required`). |
| `limited` | 429: non dimostra un proxy guasto. |
| `rejected` | Altro rifiuto upstream o del tunnel; un 407 upstream non equivale a un errore di autenticazione del proxy. |
| `inconclusive` | Timeout, errore di trasporto non localizzabile o prerequisiti mancanti: non dimostra un guasto. |

Per la pulizia in blocco considera confermata soltanto l'indisponibilità `unavailable` di quel controllo; non trattare limiti, rifiuti e timeout come guasti permanenti. L'inferenza reale può comunque essere limitata da modello, autenticazione, quota o policy.

WARP è disabilitato per impostazione predefinita. Quando è attivo, richieste gratuite anonime/Zen manuali e relativi cataloghi/test usano il tunnel del processo; Go, modelli a pagamento e OAuth mantengono l'instradamento proxy. Serve UDP Cloudflare `2408`, senza TUN o modifiche alla tabella di routing. `warp.pool_size` limita le uscite concorrenti a 1–32 (predefinito 2); `warp.stack` è `ipv6` per impostazione predefinita, con alternative `ipv4`/`dual`. Un errore di inizializzazione rifiuta l'avvio o la configurazione candidata, senza passare silenziosamente ai proxy.

Un 403/429 su un'uscita può causarne ritiro e sostituzione; i tentativi restano limitati dal budget e un'altra uscita non garantisce quota aggiuntiva. Le uscite IPv6 `/64` e IPv4 `/32` vengono deduplicate e messe in cooldown per sottorete. `warp.source_ipv6` richiede stack IPv6 e prefisso `/64` o più corto; Linux usa `IPV6_FREEBIND`, mentre su Windows gli indirizzi sorgente selezionati devono essere già assegnati. Questa impostazione non configura la rete host. `/healthz` espone solo stato/conteggi WARP; `resources.warp` in `/api/monitor` include anche informazioni sulle uscite.

## Rotazione SOTA / Sweet

I client mantengono API Base URL, chiave e i tre endpoint di messaggistica, cambiando solo `model` nell'alias `sota` o `sweet`. Gli alias sono modificabili e compaiono in `/v1/models` quando disponibili; le chiamate ai modelli reali restano compatibili. La rotazione usa solo Go (chiavi manuali o Go OAuth), senza ripiego PAYG tra tier; System One non partecipa alla rotazione.

I membri fissi di SOTA sono `deepseek-v4.1-flash`, `glm-5.3`, `grok-4.6`, `kimi-k3`, `qwen3.8-max`. Sweet comprende gli altri modelli Go di messaggistica disponibili, inizialmente con precedenza a `glm-5.3-flash`, poi in ordine alfabetico; i nuovi modelli vengono aggiunti in coda. Quelli ritirati restano nella lista ma non sono selezionabili. La disponibilità effettiva dipende da catalogo e rotte, non è garantita dal nome fisso.

Ogni gruppo condivide una posizione corrente: la conserva dopo un successo e avanza dopo un errore. Un numero di versione impedisce che un errore concorrente precedente ripristini una posizione vecchia; una selezione manuale non interrompe la risposta in corso. Ogni richiesta visita un modello al massimo una volta.

- Gli errori ordinari consentono al massimo tre tentativi per modello e consumano il budget di al massimo tre modelli con errori ordinari. Un modello esaurito viene sospeso per 15 secondi.
- Un 429/errore esplicito di quota passa direttamente al modello successivo; la pausa segue `Retry-After`, altrimenti 60 secondi, senza aggiungere consumo al budget degli errori ordinari. Gli errori ordinari precedenti continuano a contare.
- Non si applicano tentativi ordinari delle chiavi tra tier o reinvii stale-reasoning; il guasto di un modello non sospende l'intera chiave Go. Una quota condivisa può rendere indisponibile tutto il gruppo.
- Il budget complessivo è `retry.timeout_seconds`; `rotation.first_output_seconds` limita l'attesa di output effettivo per tentativo, con predefinito 30 secondi e intervallo 1–300. Per risposte non streaming occorre leggere la risposta valida completa entro questa scadenza.
- Gli eventi iniziali dello stream restano in memoria fino a output effettivo/termine normale. Prima dell'invio è possibile ritentare; dopo l'invio un errore conserva il contenuto già ricevuto, invia un errore SSE nel protocollo e termina. Non si concatenano risposte alternative in uno stream. L'annullamento del client non provoca cambi di modello o pause.
- `model` nella risposta mantiene l'alias richiesto; `X-Resolved-Model` indica il modello effettivo, cui sono attribuiti i token. Le registrazioni contengono la classificazione della causa, non corpi delle richieste, chiavi o testo degli errori upstream.

| Endpoint di gestione | Contenuto |
| --- | --- |
| `GET /api/rotation` | Ordine/alias dei due gruppi, modello corrente, pause, stato di persistenza, ultimi 100 tentativi e 30 cambi per gruppo. |
| `PUT /api/rotation` | Invia `alias`, `order` e `first_output_seconds` completi per `sota`/`sweet`; si può riordinare all'interno del gruppo senza omettere membri. |
| `POST /api/rotation/current` | `{"group":"sota","model":"glm-5.3"}`: cambia modello e ne elimina la pausa. |

Gli alias ammettono 1–64 lettere, cifre, `.`, `_`, `-`, devono iniziare con una lettera/cifra e non possono coincidere tra loro o con modelli reali. Se un cambiamento del catalogo rende incompatibili i membri inviati, aggiorna la pagina prima di riordinarli.

La configurazione è in `rotation`, lo stato operativo in `config.json.rotation.json`. La ricarica a caldo condivide lo stato; l'impossibilità iniziale di leggerlo/scriverlo rifiuta l'avvio, mentre errori di scrittura successivi sono segnalati nella gestione.

## Playground, log e monitoraggio

Il Playground invoca nello stesso processo il medesimo gateway tramite `POST /api/debug/inference`, forzando risposte non streaming. Esempio di instradamento automatico:

```json
{"protocol":"chat","key":{"mode":"auto"},"request":{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}}
```

Per una chiave specifica sostituisci `key` con `{"mode":"selected","tier":"zen","id":"KEY_FINGERPRINT"}`; l'ID proviene da `GET /api/debug/models`. La chiave selezionata esegue un solo tentativo upstream, senza anonimo, cambio chiave, cambio tier o reinvio reasoning. L'opzione `relay` seleziona un proxy con un indice noto, per esempio `{"index":0}`; l'indice deve provenire dal catalogo diagnostico corrente.

La diagnostica non modifica cooldown delle chiavi, conteggi degli errori, salute dei proxy o binding di produzione, ma invia richieste reali, consuma quota/credits ed entra nel monitoraggio. La risposta di gestione valida è incapsulata in HTTP 200, con `ok`, `http_status`, `request_id`, `route`, `response` che descrivono l'esito effettivo. Le chiavi selezionate aggiungono `selected_key` e `key_test`, con valori come `usable`, `rejected`, `rate_limited`, `transport_error`, `upstream_error`, `request_error`, `unavailable`. Errori di formato, autenticazione o limite usano comunque lo stato HTTP corrispondente.

I log sono disponibili su `GET /api/logs?after=SEQUENCE&limit=COUNT` e `GET /api/logs/stream`. SSE consente la ripresa con `Last-Event-ID` o `after`; gli eventi scaduti/persi producono `gap`, ma i dati già eliminati non sono recuperabili. Il server termina lo stream dei log quando la sessione diventa invalida.

`GET /api/monitor` fornisce aggregati di richieste, token, tentativi upstream e risorse. `GET /api/debug/models` fornisce rotte, impronte delle chiavi, metadati dei modelli, proxy e ultimo risultato diagnostico. I token sono soltanto quelli dichiarati dall'upstream, senza stime dei valori mancanti; l'input include letture e scritture della cache, mentre cached indica separatamente le letture. `usage.server_keys` raggruppa per chiave locale e distingue `playground`.

Il risultato della richiesta riguarda l'inferenza completa: un errore dello stream è `stream_error` anche con HTTP 200, l'annullamento del chiamante è `client_canceled`. I tentativi upstream misurano solo lo scambio fino agli header: percentuale di successo e latenza non equivalgono al successo della risposta completa.

## Persistenza, salute e sicurezza

| Dati | Posizione e ciclo di vita |
| --- | --- |
| Configurazione / versione precedente | `config.json`, `config.json.bak`, con informazioni sensibili. |
| Cache modelli/prezzi | `config.json.models.catalog.json`, `config.json.models.dev.json`. |
| Disponibilità gratuita | `config.json.<impronta-upstream-Zen>.availability.json`, isolata per upstream. |
| Stato della rotazione | `config.json.rotation.json`. |
| Sessioni, token cumulativi, buffer circolare dei log, ultimo Playground | Memoria del processo; azzerati al riavvio. |
| Dettagli di richieste/tentativi | Ultima ora, al massimo 10.000/20.000 elementi; gli endpoint restituiscono fino a 500 elementi ciascuno. |
| Log a lungo termine | JSON su stdout, da raccogliere e conservare in sicurezza. |

Il prefisso dei file dipende dal percorso effettivo di `-config`. Più processi non condividono sessioni, monitoraggio o contatori in memoria; evita modifiche concorrenti allo stesso file di stato.

`GET /healthz` si trova sulla porta di inferenza, non richiede autenticazione e non attiva controlli upstream o conteggi di monitoraggio. Catalogo non pronto/vuoto, WARP senza uscite attive o proxy necessari senza nodi sani producono 503. Un catalogo obsoleto ma utilizzabile può restituire 200, `degraded`; la soglia di obsolescenza è due volte l'intervallo di aggiornamento, con minimo di 60 secondi. La necessità di un nuovo login OAuth compare negli issues, ma lo stato pronto non garantisce la prossima inferenza. Le informazioni pubbliche riguardano stato e conteggi, senza chiavi o indirizzi dei proxy.

Per uso locale limita l'accesso a loopback; per gestione in rete usa HTTPS e restrizioni d'accesso. Il programma non termina TLS. Configurazione/backup contengono chiavi manuali, password dei proxy e token OAuth: limita i permessi dei file o le ACL Windows ed escludili dalla condivisione del sorgente. I corpi di debug, pur mascherati e troncati, possono contenere conversazioni private: abilitali brevemente solo quando necessario. Non allegare configurazioni complete, risultati reveal, cookie, header di autorizzazione o contenuti reali dell'inferenza per la risoluzione dei problemi.

## Verifica e manutenzione

Controlli di sviluppo locale:

```bash
go vet ./...
go build -trimpath -o opencode2api ./cmd/opencode2api
for file in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$file" || exit 1
done
```

La verifica di una distribuzione normale deve coprire login amministrativo e CSRF, disabilitazione/ripristino delle chiavi, salvataggio della configurazione e riavvio dei listener, rilevamento dei modelli, tre protocolli di messaggistica ed errori SSE, stato della rotazione e funzioni reali OAuth/upstream necessarie. Inferenza reale, OAuth, WARP e query di quota possono dipendere dai permessi dell'account e consumare quota; i controlli statici non sostituiscono queste verifiche.

Prima di aggiornare salva in sicurezza programma, configurazione effettiva e stato; arresta il processo prima di sostituire l'eseguibile. Per tornare indietro ripristina configurazione e stato compatibili. Con Compose conserva i file effettivi nel volume. Non aggiungere credenziali, log, stato operativo o artefatti locali di verifica al sorgente.
