package googlecast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// verbose mirrors the media server's MOVCASTER_VERBOSE logging.
var verbose = os.Getenv("MOVCASTER_VERBOSE") != ""

func logf(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[cast] "+format+"\n", args...)
	}
}

// Cast protocol namespaces.
const (
	nsConnection = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver   = "urn:x-cast:com.google.cast.receiver"
	nsMedia      = "urn:x-cast:com.google.cast.media"
)

const (
	senderID          = "sender-movcaster"
	receiverID        = "receiver-0"
	defaultMediaAppID = "CC1AD845" // the Default Media Receiver
	castPort          = "8009"
)

// conn is one TLS connection to a Cast device: framing, a read-loop that
// dispatches inbound messages, request/response correlation, and the heartbeat.
type conn struct {
	tls net.Conn

	writeMu sync.Mutex // serializes frames on the wire

	mu        sync.Mutex
	nextReqID int
	waiters   map[int]chan json.RawMessage // by requestId
	// onMessage handlers by namespace, for pushed (uncorrelated) messages.
	onMessage map[string]func(dest string, payload json.RawMessage)
	closed    bool
	closeErr  error

	done chan struct{} // closed when the read loop exits
}

// dial opens the TLS connection and starts the read loop and heartbeat. addr is
// host or host:port (the Cast port 8009 is appended when missing).
func dial(ctx context.Context, addr string) (*conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, castPort)
	}
	// The receiver presents a self-signed device certificate. This is a LAN
	// device with no meaningful identity a CA could vouch for; refusing it would
	// make casting impossible. Do not "fix" this by enabling verification.
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{InsecureSkipVerify: true},
	}
	tc, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &conn{
		tls:       tc,
		nextReqID: 1,
		waiters:   make(map[int]chan json.RawMessage),
		onMessage: make(map[string]func(string, json.RawMessage)),
		done:      make(chan struct{}),
	}
	go c.readLoop()
	go c.heartbeat()
	return c, nil
}

// send writes one JSON payload to a destination on a namespace.
func (c *conn) send(dest, namespace string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	logf("-> %s %s %s", dest, namespace, b)
	m := &castMessage{
		protocolVersion: castV2_1_0,
		sourceID:        senderID,
		destinationID:   dest,
		namespace:       namespace,
		payloadType:     payloadTypeString,
		payloadUTF8:     string(b),
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.tls, m)
}

// request sends a payload carrying a fresh requestId and waits for the response
// that echoes it. The payload map is mutated to add the requestId.
func (c *conn) request(ctx context.Context, dest, namespace string, payload map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, fmt.Errorf("cast connection closed: %w", err)
	}
	id := c.nextReqID
	c.nextReqID++
	ch := make(chan json.RawMessage, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	// Always remove the waiter, or a timed-out request would leak it.
	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	payload["requestId"] = id
	if err := c.send(dest, namespace, payload); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("cast connection lost waiting for reply %d", id)
	}
}

// handle registers a handler for pushed messages on a namespace (those without
// a matching waiter). One handler per namespace.
func (c *conn) handle(namespace string, fn func(dest string, payload json.RawMessage)) {
	c.mu.Lock()
	c.onMessage[namespace] = fn
	c.mu.Unlock()
}

// readLoop dispatches every inbound frame: PING/PONG bookkeeping, requestId
// correlation, then per-namespace push handlers.
func (c *conn) readLoop() {
	defer close(c.done)
	for {
		m, err := readFrame(c.tls)
		if err != nil {
			c.close(err)
			logf("read loop ended: %v", err)
			return
		}
		logf("<- %s %s %s", m.sourceID, m.namespace, m.payloadUTF8)

		if m.namespace == nsHeartbeat {
			var hb struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(m.payloadUTF8), &hb)
			if hb.Type == "PING" {
				// Answer promptly or the receiver drops us within ~10s.
				_ = c.send(m.sourceID, nsHeartbeat, map[string]any{"type": "PONG"})
			}
			continue
		}

		payload := json.RawMessage(m.payloadUTF8)
		var env struct {
			RequestID int `json:"requestId"`
		}
		_ = json.Unmarshal(payload, &env)
		if env.RequestID != 0 {
			c.mu.Lock()
			ch := c.waiters[env.RequestID]
			c.mu.Unlock()
			if ch != nil {
				ch <- payload
				// A correlated MEDIA_STATUS is also a status push; fall through so
				// the media handler keeps its cache current.
			}
		}
		c.mu.Lock()
		fn := c.onMessage[m.namespace]
		c.mu.Unlock()
		if fn != nil {
			fn(m.sourceID, payload)
		}
	}
}

// heartbeat PINGs the receiver every 5s for the life of the connection. The
// receiver drops a sender that goes quiet for roughly 10 seconds, and a dropped
// connection mid-cast looks exactly like the TV freezing.
func (c *conn) heartbeat() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if err := c.send(receiverID, nsHeartbeat, map[string]any{"type": "PING"}); err != nil {
				logf("heartbeat send failed: %v", err)
				return
			}
		}
	}
}

// close tears the connection down once, remembering why.
func (c *conn) close(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.closeErr = err
	}
	c.mu.Unlock()
	_ = c.tls.Close()
}
