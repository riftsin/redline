//go:build e2e

package e2e

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
)

// wsConn is a minimal RFC 6455 client, just enough to speak the Chrome
// DevTools protocol: text messages, fragmentation, ping/pong and close.
type wsConn struct {
	conn    net.Conn
	r       *bufio.Reader
	writeMu sync.Mutex
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

func dialWS(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)

	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
		Host:   u.Host,
		Header: http.Header{
			"Upgrade":               {"websocket"},
			"Connection":            {"Upgrade"},
			"Sec-WebSocket-Key":     {key},
			"Sec-WebSocket-Version": {"13"},
		},
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket: handshake status %s", resp.Status)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	return &wsConn{conn: conn, r: r}, nil
}

// WriteText sends one masked text frame.
func (c *wsConn) WriteText(payload []byte) error {
	return c.writeFrame(opText, payload)
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)

	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := c.conn.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

// ReadMessage returns the next complete text or binary message, answering
// pings along the way. It returns io.EOF once the server closes.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var message []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return nil, io.EOF
		case opText, opBinary, opContinuation:
			message = append(message, payload...)
			if fin {
				return message, nil
			}
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %#x", op)
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.r, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close closes the underlying connection without a closing handshake; the
// browser process is about to exit anyway.
func (c *wsConn) Close() error {
	return c.conn.Close()
}
