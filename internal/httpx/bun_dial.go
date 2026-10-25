package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	utls "github.com/refraction-networking/utls"
)

// bunHandshakeTimeout bounds the proxy negotiation and the TLS handshake of a
// new connection, the TLSHandshakeTimeout the regular clients inherit from
// http.DefaultTransport.
const bunHandshakeTimeout = 10 * time.Second

// bunClientHello returns the ClientHello of Bun's fetch, captured from Bun
// 1.4.3. Bun links its own BoringSSL, so the hello is the same on every
// platform: JA4 t13d1713h1, no GREASE, no padding, http/1.1 as the only ALPN
// protocol and X25519MLKEM768 as the first key share. uTLS keeps handshake
// state in the extensions, so every connection needs a fresh spec.
func bunClientHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// dial opens a connection to addr ("host:port"), through the transport's
// proxy when it has one, and completes the Bun TLS handshake with serverName
// for https. A proxy failure comes back as a *net.OpError whose Op is
// "proxyconnect" or "socks connect", the shape net/http uses, so the gateway
// classifies proxy failures of both transports alike.
func (t *BunTransport) dial(ctx context.Context, scheme, addr, serverName string) (net.Conn, error) {
	var conn net.Conn
	var err error
	if t.dialContext != nil {
		conn, err = t.dialContext(ctx, "tcp", addr)
	} else if t.proxy == nil {
		conn, err = t.dialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = t.dialProxy(ctx, addr)
	}
	if err != nil || scheme != "https" {
		return conn, err
	}
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(bunClientHello()); err != nil {
		conn.Close()
		return nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, bunHandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// dialProxy opens a tunnel to addr through the transport's proxy: an HTTP
// CONNECT for http and https proxies (the latter over standard TLS, which
// only the proxy sees), or a SOCKS5 CONNECT. Like net/http it lets a socks5
// proxy resolve host names too, so socks5 and socks5h behave the same.
func (t *BunTransport) dialProxy(ctx context.Context, addr string) (net.Conn, error) {
	op := "proxyconnect"
	socks := t.proxy.Scheme == "socks5" || t.proxy.Scheme == "socks5h"
	if socks {
		op = "socks connect"
	}
	conn, err := t.dialer.DialContext(ctx, "tcp", proxyAddr(t.proxy))
	if err != nil {
		return nil, &net.OpError{Op: op, Net: "tcp", Err: err}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(bunHandshakeTimeout))
	if t.proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: t.proxy.Hostname()})
		if err = tlsConn.HandshakeContext(ctx); err == nil {
			conn = tlsConn
		}
	}
	if err == nil {
		if socks {
			err = socks5Connect(conn, t.proxy.User, addr)
		} else {
			conn, err = httpConnect(conn, t.proxy.User, addr)
		}
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		conn.Close()
		return nil, &net.OpError{Op: op, Net: "tcp", Err: err}
	}
	return conn, nil
}

// proxyAddr returns the "host:port" of a proxy URL, defaulting the port by
// scheme the way net/http does.
func proxyAddr(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		switch proxy.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}

// httpConnect asks an HTTP proxy on conn for a tunnel to addr, with Basic
// Proxy-Authorization when user is set. Bytes the proxy sent past its 2xx
// answer are kept in front of the returned connection.
func httpConnect(conn net.Conn, user *url.Userinfo, addr string) (net.Conn, error) {
	request := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if user != nil {
		password, _ := user.Password()
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)) + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		return conn, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return conn, err
	}
	if resp.StatusCode/100 != 2 {
		return conn, fmt.Errorf("proxy refused CONNECT: %s", resp.Status)
	}
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

// bufferedConn is a net.Conn whose reads drain reader first, for a tunnel the
// proxy already wrote into while answering CONNECT.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// socksReplies names the SOCKS5 reply codes (RFC 1928, section 6).
var socksReplies = map[byte]string{
	1: "general SOCKS server failure",
	2: "connection not allowed by ruleset",
	3: "network unreachable",
	4: "host unreachable",
	5: "connection refused",
	6: "TTL expired",
	7: "command not supported",
	8: "address type not supported",
}

// socks5Connect runs a SOCKS5 CONNECT to addr on conn (RFC 1928), offering
// username/password authentication (RFC 1929) when user is set. Host names
// are sent as is for the proxy to resolve.
func socks5Connect(conn net.Conn, user *url.Userinfo, addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port %q", portText)
	}
	methods := []byte{0x00}
	if user != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{5, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 5 {
		return fmt.Errorf("unexpected protocol version %d", reply[0])
	}
	switch {
	case reply[1] == 0x00:
	case reply[1] == 0x02 && user != nil:
		name := user.Username()
		password, _ := user.Password()
		if len(name) == 0 || len(name) > 255 || len(password) > 255 {
			return errors.New("invalid username/password")
		}
		auth := append([]byte{1, byte(len(name))}, name...)
		auth = append(append(auth, byte(len(password))), password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		if reply[1] != 0 {
			return errors.New("username/password authentication failed")
		}
	default:
		return errors.New("no acceptable authentication methods")
	}
	request := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip == nil {
		if len(host) > 255 {
			return errors.New("FQDN too long")
		}
		request = append(append(request, 3, byte(len(host))), host...)
	} else if ip4 := ip.To4(); ip4 != nil {
		request = append(append(request, 1), ip4...)
	} else {
		request = append(append(request, 4), ip.To16()...)
	}
	if _, err := conn.Write(binary.BigEndian.AppendUint16(request, uint16(port))); err != nil {
		return err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != 5 {
		return fmt.Errorf("unexpected protocol version %d", header[0])
	}
	if header[1] != 0 {
		if text, ok := socksReplies[header[1]]; ok {
			return errors.New(text)
		}
		return fmt.Errorf("unknown SOCKS reply %d", header[1])
	}
	// Skip the bound address and port the proxy reports.
	var skip int
	switch header[3] {
	case 1:
		skip = net.IPv4len
	case 4:
		skip = net.IPv6len
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		skip = int(length[0])
	default:
		return fmt.Errorf("unknown address type %d", header[3])
	}
	_, err = io.ReadFull(conn, make([]byte, skip+2))
	return err
}
