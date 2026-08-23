package wsclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultHandshakeTimeout = 20 * time.Second
	defaultMaxMessageBytes  = 256 * 1024
	websocketGUID           = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type Conn interface {
	ReadMessage(context.Context) ([]byte, error)
	WriteMessage(context.Context, []byte) error
	Close() error
}

type Dialer struct {
	NetDialer        *net.Dialer
	TLSConfig        *tls.Config
	HandshakeTimeout time.Duration
	MaxMessageBytes  int64
}

func (d *Dialer) Dial(ctx context.Context, rawURL string, headers http.Header) (Conn, error) {
	u, err := validateURL(rawURL)
	if err != nil {
		return nil, err
	}
	netDialer := d.NetDialer
	if netDialer == nil {
		netDialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	}
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "443")
	}
	tcpConn, err := netDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial WSS endpoint: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
	if d.TLSConfig != nil {
		tlsConfig = d.TLSConfig.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = u.Hostname()
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	if tlsConfig.InsecureSkipVerify {
		_ = tcpConn.Close()
		return nil, errors.New("WSS TLS certificate verification cannot be disabled")
	}
	tlsConn := tls.Client(tcpConn, tlsConfig)
	handshakeTimeout := d.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	deadline := time.Now().Add(handshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = tlsConn.SetDeadline(deadline)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("create websocket handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("create websocket request: %w", err)
	}
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Host", u.Host)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Key", key)
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("User-Agent", "vps-tool-agent/1")
	request.Host = u.Host
	if err := request.Write(tlsConn); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("write websocket handshake: %w", err)
	}
	reader := bufio.NewReaderSize(tlsConn, 8*1024)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("read websocket handshake: %w", err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !headerContains(response.Header.Values("Upgrade"), "websocket") || !headerContainsToken(response.Header.Values("Connection"), "upgrade") {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("WSS endpoint rejected websocket handshake with HTTP %s", response.Status)
	}
	expectedAccept := base64.StdEncoding.EncodeToString(sha1sum([]byte(key + websocketGUID)))
	if response.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		_ = tlsConn.Close()
		return nil, errors.New("WSS handshake returned an invalid accept key")
	}
	_ = tlsConn.SetDeadline(time.Time{})
	maxBytes := d.MaxMessageBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxMessageBytes
	}
	return &connection{conn: tlsConn, reader: reader, maxBytes: maxBytes}, nil
}

func validateURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("WSS URL must be wss:// with no credentials, query, or fragment")
	}
	return u, nil
}

func sha1sum(data []byte) []byte {
	hash := sha1.Sum(data)
	return hash[:]
}

func headerContains(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), expected) {
			return true
		}
	}
	return false
}

func headerContainsToken(values []string, expected string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), expected) {
				return true
			}
		}
	}
	return false
}

type connection struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
	maxBytes  int64
}

func (c *connection) ReadMessage(ctx context.Context) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	var message bytes.Buffer
	started := false
	for {
		frame, err := c.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch frame.opcode {
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := c.writeControl(0xA, frame.payload, ctx); err != nil {
				return nil, err
			}
		case 0xA:
			continue
		case 0x1:
			if started {
				return nil, errors.New("unexpected websocket text frame")
			}
			started = true
			if err := appendLimited(&message, frame.payload, c.maxBytes); err != nil {
				return nil, err
			}
			if frame.fin {
				return message.Bytes(), nil
			}
		case 0x0:
			if !started {
				return nil, errors.New("unexpected websocket continuation frame")
			}
			if err := appendLimited(&message, frame.payload, c.maxBytes); err != nil {
				return nil, err
			}
			if frame.fin {
				return message.Bytes(), nil
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode 0x%x", frame.opcode)
		}
	}
}

func (c *connection) WriteMessage(ctx context.Context, payload []byte) error {
	if int64(len(payload)) > c.maxBytes {
		return errors.New("websocket message exceeds limit")
	}
	return c.writeFrame(ctx, 0x1, payload)
}

func (c *connection) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	return err
}

type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (c *connection) readFrame() (frame, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return frame{}, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	if length == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return frame{}, err
		}
		length = int64(extended[0])<<8 | int64(extended[1])
	} else if length == 127 {
		var extended [8]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return frame{}, err
		}
		if extended[0]&0x80 != 0 {
			return frame{}, errors.New("websocket frame length is invalid")
		}
		for _, value := range extended {
			length = (length << 8) | int64(value)
		}
	}
	if length < 0 || length > c.maxBytes || length > int64(^uint(0)>>1) {
		return frame{}, errors.New("websocket frame exceeds limit")
	}
	if opcode >= 0x8 && (!fin || length > 125) {
		return frame{}, errors.New("invalid websocket control frame")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return frame{}, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return frame{}, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

func (c *connection) writeFrame(ctx context.Context, opcode byte, payload []byte) error {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetWriteDeadline(time.Now()) })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(deadline)
	} else {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	length := len(payload)
	var header bytes.Buffer
	header.WriteByte(0x80 | opcode)
	if length < 126 {
		header.WriteByte(0x80 | byte(length))
	} else if length <= int(^uint16(0)) {
		header.WriteByte(0x80 | 126)
		header.WriteByte(byte(length >> 8))
		header.WriteByte(byte(length))
	} else {
		header.WriteByte(0x80 | 127)
		for shift := uint(56); ; shift -= 8 {
			header.WriteByte(byte(uint64(length) >> shift))
			if shift == 0 {
				break
			}
		}
	}
	header.Write(mask)
	if _, err := c.conn.Write(header.Bytes()); err != nil {
		return err
	}
	maskedPayload := make([]byte, len(payload))
	for index := range payload {
		maskedPayload[index] = payload[index] ^ mask[index%4]
	}
	return writeAll(c.conn, maskedPayload)
}

func (c *connection) writeControl(opcode byte, payload []byte, ctx context.Context) error {
	return c.writeFrame(ctx, opcode, payload)
}

func appendLimited(buffer *bytes.Buffer, payload []byte, limit int64) error {
	if int64(buffer.Len())+int64(len(payload)) > limit {
		return errors.New("websocket message exceeds limit")
	}
	_, _ = buffer.Write(payload)
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}
