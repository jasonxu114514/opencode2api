package httpx

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// BunOptions configures a BunTransport. The fields mirror their namesakes on
// http.Transport; a nil Proxy dials the upstream directly.
type BunOptions struct {
	Proxy                 *url.URL
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConnsPerHost   int
	DisableKeepAlives     bool
}

// BunTransport is an http.RoundTripper that puts requests on the wire the way
// Bun's fetch does, so they look like they come from an official OpenCode
// client (the 1.x and 2.x CLIs both run on Bun). net/http cannot do that:
// crypto/tls always sends Go's own ClientHello, and its HTTP/1.1 writer orders
// and canonicalizes headers its own way. BunTransport handshakes with Bun's
// BoringSSL ClientHello through uTLS (bunClientHello), speaks HTTP/1.1 only,
// the one ALPN protocol Bun offers, and lays requests out like Bun (see
// bunRequestHead). Like fetch it decodes gzip, deflate, br and zstd bodies.
//
// Idle connections are pooled per host, up to MaxIdleConnsPerHost, until
// IdleConnTimeout. A pooled connection the server closes is dropped while it
// is idle, and a reused connection the server drops before answering is
// retried once on a fresh one. Unlike http.Transport it does not cap the
// connections per host.
type BunTransport struct {
	proxy                 *url.URL
	dialer                *net.Dialer
	responseHeaderTimeout time.Duration
	idleTimeout           time.Duration
	maxIdlePerHost        int
	keepAlive             bool

	mu   sync.Mutex
	idle map[string][]*bunConn
}

// NewBunTransport builds a BunTransport from options. A MaxIdleConnsPerHost
// of zero keeps http.DefaultMaxIdleConnsPerHost, as http.Transport does.
func NewBunTransport(options BunOptions) (*BunTransport, error) {
	if options.Proxy != nil {
		switch options.Proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", options.Proxy.Scheme)
		}
	}
	maxIdle := options.MaxIdleConnsPerHost
	if maxIdle == 0 {
		maxIdle = http.DefaultMaxIdleConnsPerHost
	}
	return &BunTransport{
		proxy:                 options.Proxy,
		dialer:                &net.Dialer{Timeout: options.ConnectTimeout, KeepAlive: 30 * time.Second},
		responseHeaderTimeout: options.ResponseHeaderTimeout,
		idleTimeout:           options.IdleConnTimeout,
		maxIdlePerHost:        maxIdle,
		keepAlive:             !options.DisableKeepAlives,
		idle:                  map[string][]*bunConn{},
	}, nil
}

// bunConn is one upstream connection. While it is pooled, a watcher goroutine
// blocks in a one-byte Peek so a server that closes it is noticed before it
// is handed out again; watched is closed once that goroutine returns, and
// watchErr holds what ended the Peek.
type bunConn struct {
	net.Conn
	key      string
	br       *bufio.Reader
	bw       *bufio.Writer
	idleAt   time.Time
	timer    *time.Timer
	watched  chan struct{}
	watchErr error
}

// RoundTrip sends req and returns the response headers; the body is read
// straight off the connection. The request body is read fully before
// anything is sent. Header names and values must be valid HTTP fields. Once
// the request context ends, the error is its cause, as with http.Transport.
func (t *BunTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	u := req.URL
	if u == nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("bun transport: unsupported request URL %q", u)
	}
	head, decompress, err := bunRequestHead(req, body)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	trace := httptrace.ContextClientTrace(ctx)
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	key := u.Scheme + "://" + addr
	for retried := false; ; retried = true {
		c, info, err := t.getConn(ctx, u.Scheme, key, addr, u.Hostname())
		if err != nil {
			return nil, bunContextError(ctx, err)
		}
		if trace != nil && trace.GotConn != nil {
			trace.GotConn(info)
		}
		resp, dropped, err := t.exchange(ctx, c, req, head, body, decompress, trace)
		if err == nil {
			return resp, nil
		}
		if info.Reused && dropped && !retried && ctx.Err() == nil {
			continue
		}
		return nil, bunContextError(ctx, err)
	}
}

// readRequestBody reads and closes the body of req. It returns nil for a
// request without a body and a non-nil, possibly empty slice otherwise.
func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

// bunContextError returns the cause of ctx once it has ended, and err
// otherwise.
func bunContextError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// getConn returns a pooled connection for key, skipping ones that died while
// idle, or dials a new one.
func (t *BunTransport) getConn(ctx context.Context, scheme, key, addr, serverName string) (*bunConn, httptrace.GotConnInfo, error) {
	for c := t.takeIdle(key); c != nil; c = t.takeIdle(key) {
		if c.reclaim() {
			return c, httptrace.GotConnInfo{Conn: c.Conn, Reused: true, WasIdle: true, IdleTime: time.Since(c.idleAt)}, nil
		}
		c.Close()
	}
	conn, err := t.dial(ctx, scheme, addr, serverName)
	if err != nil {
		return nil, httptrace.GotConnInfo{}, err
	}
	c := &bunConn{Conn: conn, key: key, br: bufio.NewReader(conn), bw: bufio.NewWriter(conn)}
	return c, httptrace.GotConnInfo{Conn: conn}, nil
}

// exchange writes one request on c and reads the response headers. On success
// the response body owns c; on failure c is closed, and dropped reports
// whether the server dropped the connection before the first response byte,
// which a reused connection may retry.
func (t *BunTransport) exchange(ctx context.Context, c *bunConn, req *http.Request, head, body []byte, decompress bool, trace *httptrace.ClientTrace) (*http.Response, bool, error) {
	stop := context.AfterFunc(ctx, func() { c.Close() })
	fail := func(err error, dropped bool) (*http.Response, bool, error) {
		stop()
		c.Close()
		var timeout net.Error
		if t.responseHeaderTimeout > 0 && ctx.Err() == nil && errors.As(err, &timeout) && timeout.Timeout() {
			err = fmt.Errorf("bun transport: timeout awaiting response headers: %w", err)
		}
		return nil, dropped, err
	}
	_, err := c.bw.Write(head)
	if err == nil {
		_, err = c.bw.Write(body)
	}
	if err == nil {
		err = c.bw.Flush()
	}
	if trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: err})
	}
	if err != nil {
		return fail(err, isConnDropped(err))
	}
	if t.responseHeaderTimeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(t.responseHeaderTimeout))
	}
	if _, err := c.br.Peek(1); err != nil {
		return fail(err, isConnDropped(err))
	}
	if trace != nil && trace.GotFirstResponseByte != nil {
		trace.GotFirstResponseByte()
	}
	resp, err := http.ReadResponse(c.br, req)
	// Skip interim 1xx responses such as 103 Early Hints.
	for err == nil && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
		resp, err = http.ReadResponse(c.br, req)
	}
	if err != nil {
		return fail(err, false)
	}
	_ = c.SetReadDeadline(time.Time{})
	raw := &bunBody{t: t, c: c, ctx: ctx, stop: stop, src: resp.Body, reusable: t.keepAlive && !req.Close && !resp.Close}
	if resp.Body == http.NoBody {
		raw.finish(true)
		return resp, false, nil
	}
	resp.Body = raw
	if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); decompress && isBunEncoding(encoding) {
		resp.Body = &bunDecoder{body: raw, encoding: encoding}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}
	return resp, false, nil
}

// isConnDropped reports whether err says the server closed or reset the
// connection.
func isConnDropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}

// takeIdle removes and returns the most recently pooled connection for key,
// or nil when there is none.
func (t *BunTransport) takeIdle(key string) *bunConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	conns := t.idle[key]
	if len(conns) == 0 {
		return nil
	}
	c := conns[len(conns)-1]
	t.setIdle(key, conns[:len(conns)-1])
	return c
}

// putIdle pools c once its response has been read to the end, or closes it
// when the pool for its host is full, and starts its idle watcher.
func (t *BunTransport) putIdle(c *bunConn) {
	t.mu.Lock()
	if len(t.idle[c.key]) >= t.maxIdlePerHost {
		t.mu.Unlock()
		c.Close()
		return
	}
	c.idleAt = time.Now()
	c.watched = make(chan struct{})
	c.timer = nil
	if t.idleTimeout > 0 {
		c.timer = time.AfterFunc(t.idleTimeout, func() {
			if t.removeIdle(c) {
				c.Close()
			}
		})
	}
	t.idle[c.key] = append(t.idle[c.key], c)
	t.mu.Unlock()
	go func() {
		_, c.watchErr = c.br.Peek(1)
		if t.removeIdle(c) {
			c.Close()
		}
		close(c.watched)
	}()
}

// removeIdle removes c from the pool and reports whether it was still there.
func (t *BunTransport) removeIdle(c *bunConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	conns := t.idle[c.key]
	index := slices.Index(conns, c)
	if index < 0 {
		return false
	}
	t.setIdle(c.key, slices.Delete(conns, index, index+1))
	return true
}

// setIdle stores the pooled connections of key, dropping an empty entry. The
// caller must hold mu.
func (t *BunTransport) setIdle(key string, conns []*bunConn) {
	if len(conns) == 0 {
		delete(t.idle, key)
		return
	}
	t.idle[key] = conns
}

// reclaim stops the idle watcher of a connection just taken from the pool and
// reports whether the connection is still usable: the watcher's Peek must
// have ended on the deadline set here, not on data or a close from the
// server. A read timeout leaves the TLS state intact, so the connection can
// carry the next request.
func (c *bunConn) reclaim() bool {
	if c.timer != nil {
		c.timer.Stop()
	}
	_ = c.SetReadDeadline(time.Unix(1, 0))
	<-c.watched
	return errors.Is(c.watchErr, os.ErrDeadlineExceeded) && c.SetReadDeadline(time.Time{}) == nil
}

// CloseIdleConnections closes every pooled connection. http.Client calls it
// from its own CloseIdleConnections.
func (t *BunTransport) CloseIdleConnections() {
	t.mu.Lock()
	idle := t.idle
	t.idle = map[string][]*bunConn{}
	t.mu.Unlock()
	for _, conns := range idle {
		for _, c := range conns {
			c.Close()
		}
	}
}

// bunBody is a response body read straight off its connection. Reading it to
// the end hands the connection back to the pool when it is reusable; closing
// it early, a read error or the end of the request context closes the
// connection instead, since an HTTP/1.1 connection with a pending body cannot
// carry another request. src is never closed: closing an unfinished net/http
// body drains it, which would block on an event stream.
type bunBody struct {
	t        *BunTransport
	c        *bunConn
	ctx      context.Context
	stop     func() bool
	src      io.Reader
	reusable bool
	once     sync.Once
}

func (b *bunBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if err == io.EOF {
		b.finish(true)
	} else if err != nil {
		err = bunContextError(b.ctx, err)
		b.finish(false)
	}
	return n, err
}

func (b *bunBody) Close() error {
	b.finish(false)
	return nil
}

// finish releases the connection once: back to the pool when the body ended
// cleanly on a reusable connection whose context was still live, closed
// otherwise.
func (b *bunBody) finish(ended bool) {
	b.once.Do(func() {
		if b.stop() && ended && b.reusable {
			b.t.putIdle(b.c)
			return
		}
		b.c.Close()
	})
}

// isBunEncoding reports whether encoding is a Content-Encoding the transport
// decodes, the ones its Accept-Encoding offers.
func isBunEncoding(encoding string) bool {
	switch encoding {
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return true
	}
	return false
}

// bunDecoder decompresses a response body. The decoder is built on the first
// Read, so RoundTrip never waits for a compression header.
type bunDecoder struct {
	body     *bunBody
	encoding string
	reader   io.Reader
	release  func()
	err      error
}

func (d *bunDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.reader == nil {
		if d.reader, d.release, d.err = newBunDecoder(d.encoding, d.body); d.err != nil {
			return 0, d.err
		}
	}
	n, err := d.reader.Read(p)
	if err != nil {
		d.err = err
		if d.release != nil {
			d.release()
		}
		if err == io.EOF {
			// A decoder can stop at the end of its own stream and leave the
			// transfer framing, such as the last chunk, unread. Consuming it
			// lets the connection go back to the pool.
			_, _ = io.Copy(io.Discard, io.LimitReader(d.body, 4<<10))
		}
	}
	return n, err
}

func (d *bunDecoder) Close() error {
	return d.body.Close()
}

// newBunDecoder returns a reader decoding r for encoding and, when the
// decoder holds resources, a function releasing them. Bodies are capped at an
// 8 MiB zstd window, the limit RFC 9659 sets for HTTP.
func newBunDecoder(encoding string, r io.Reader) (io.Reader, func(), error) {
	switch encoding {
	case "deflate":
		// "deflate" means zlib-wrapped data (RFC 9110), but some servers send
		// a raw DEFLATE stream, so both are accepted.
		reader := bufio.NewReader(r)
		header, err := reader.Peek(2)
		if err != nil {
			return nil, nil, err
		}
		if header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
			decoder, err := zlib.NewReader(reader)
			return decoder, nil, err
		}
		return flate.NewReader(reader), nil, nil
	case "br":
		return brotli.NewReader(r), nil, nil
	case "zstd":
		// A single-threaded decoder runs on the reading goroutine and starts
		// none of its own.
		decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return nil, nil, err
		}
		return decoder, decoder.Close, nil
	default:
		decoder, err := gzip.NewReader(r)
		return decoder, nil, err
	}
}

// bunCommonHeaders lists the request headers WebKit knows by name, in the
// spelling and order Bun's fetch writes them: ahead of every other header,
// alphabetically.
var bunCommonHeaders = []string{
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language", "Authorization", "Cache-Control",
	"Connection", "Content-Encoding", "Content-Language", "Content-Length", "Content-Type", "Cookie", "DNT",
	"Expect", "Host", "If-None-Match", "Keep-Alive", "Last-Event-Id", "Origin", "Pragma",
	"Proxy-Authorization", "Range", "Referer", "Sec-Fetch-Mode", "TE", "Upgrade-Insecure-Requests",
	"User-Agent",
}

// bunRequestHead renders the request line and headers of req the way Bun's
// fetch writes them for body (nil when the request has none):
//  1. the headers of bunCommonHeaders, in that order and spelling;
//  2. every other header, lowercased as both OpenCode clients hand them to
//     fetch, and sorted as the 1.x AI SDK passes them;
//  3. the headers fetch adds itself: Connection, then User-Agent and Accept
//     unless the caller set them, Host, Accept-Encoding unless the caller set
//     it, and Content-Length when there is a body.
//
// An Accept of "*/*" is written where fetch adds its default, since that is
// where the official clients' Accept appears. Host, Connection,
// Content-Length and Transfer-Encoding belong to the transport, so caller
// values for them are ignored. decompress reports whether the transport added
// Accept-Encoding and so decodes the response.
func bunRequestHead(req *http.Request, body []byte) ([]byte, bool, error) {
	values := make(map[string]string, len(req.Header))
	for name, list := range req.Header {
		for _, value := range list {
			if !validHeaderField(name, value) {
				return nil, false, fmt.Errorf("bun transport: invalid header field %q", name)
			}
		}
		lower := strings.ToLower(name)
		if len(list) == 0 || lower == "host" || lower == "connection" || lower == "content-length" || lower == "transfer-encoding" {
			continue
		}
		value := strings.Join(list, ", ")
		if previous, ok := values[lower]; ok {
			value = previous + ", " + value
		}
		values[lower] = value
	}
	if values["accept"] == "*/*" {
		delete(values, "accept")
	}
	_, hasUserAgent := values["user-agent"]
	_, hasAccept := values["accept"]
	_, hasEncoding := values["accept-encoding"]
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var head strings.Builder
	head.WriteString(method + " " + req.URL.RequestURI() + " HTTP/1.1\r\n")
	for _, name := range bunCommonHeaders {
		lower := strings.ToLower(name)
		if value, ok := values[lower]; ok {
			head.WriteString(name + ": " + value + "\r\n")
			delete(values, lower)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		head.WriteString(name + ": " + values[name] + "\r\n")
	}
	head.WriteString("Connection: keep-alive\r\n")
	if !hasUserAgent {
		head.WriteString("User-Agent: Bun/" + v1BunVersion + "\r\n")
	}
	if !hasAccept {
		head.WriteString("Accept: */*\r\n")
	}
	// fetch takes Host from the parsed URL, which omits a default port.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if req.URL.Scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	head.WriteString("Host: " + host + "\r\n")
	if !hasEncoding {
		head.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	}
	if body != nil {
		head.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	head.WriteString("\r\n")
	return []byte(head.String()), !hasEncoding, nil
}

// validHeaderField reports whether name is an HTTP token and value carries no
// control characters other than tab, so a field cannot split the request.
func validHeaderField(name, value string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if b := name[i]; b <= ' ' || b >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, b) >= 0 {
			return false
		}
	}
	for i := 0; i < len(value); i++ {
		if b := value[i]; b < ' ' && b != '\t' || b == 0x7f {
			return false
		}
	}
	return true
}
