package googlecast

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeReceiver speaks the CASTV2 framing over TLS on 127.0.0.1 and replies with
// canned JSON, so the whole engine is unit-testable without a TV.
type fakeReceiver struct {
	t  *testing.T
	ln net.Listener

	mu          sync.Mutex
	connectedTo map[string]bool  // CONNECTed destinations
	launched    bool             // media app running
	loads       []map[string]any // decoded LOAD payloads
	mediaCalls  []string         // LOAD/PLAY/PAUSE/STOP/SEEK/GET_STATUS
	dropped     []string         // media messages ignored (no CONNECT to app)
	volume      float64
	muted       bool
	gotPong     chan struct{}

	// loadFailCode != 0 makes LOAD answer LOAD_FAILED with that detailed code.
	loadFailCode int
	// status is what GET_STATUS (media) reports.
	status mediaStatus
}

const (
	fakeTransportID = "transport-1"
	fakeSessionID   = "session-1"
)

func newFakeReceiver(t *testing.T) *fakeReceiver {
	t.Helper()
	cert := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeReceiver{
		t:           t,
		ln:          ln,
		connectedTo: make(map[string]bool),
		volume:      0.5,
		gotPong:     make(chan struct{}, 4),
		status:      mediaStatus{MediaSessionID: 7, PlayerState: "PLAYING"},
	}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeReceiver) addr() string { return f.ln.Addr().String() }

func (f *fakeReceiver) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serveConn(c)
	}
}

func (f *fakeReceiver) send(c net.Conn, dest, namespace string, payload any) {
	b, _ := json.Marshal(payload)
	_ = writeFrame(c, &castMessage{
		protocolVersion: castV2_1_0,
		sourceID:        receiverID,
		destinationID:   dest,
		namespace:       namespace,
		payloadType:     payloadTypeString,
		payloadUTF8:     string(b),
	})
}

func (f *fakeReceiver) serveConn(c net.Conn) {
	defer c.Close()
	for {
		m, err := readFrame(c)
		if err != nil {
			return
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(m.payloadUTF8), &body)
		typ, _ := body["type"].(string)
		reqID, _ := body["requestId"].(float64)

		switch m.namespace {
		case nsConnection:
			if typ == "CONNECT" {
				f.mu.Lock()
				first := len(f.connectedTo) == 0
				f.connectedTo[m.destinationID] = true
				f.mu.Unlock()
				// A real receiver pings the sender; the first CONNECT triggers one
				// so the client's PONG path is exercised.
				if first {
					f.send(c, m.sourceID, nsHeartbeat, map[string]any{"type": "PING"})
				}
			}

		case nsHeartbeat:
			switch typ {
			case "PING":
				f.send(c, m.sourceID, nsHeartbeat, map[string]any{"type": "PONG"})
			case "PONG":
				select {
				case f.gotPong <- struct{}{}:
				default:
				}
			}

		case nsReceiver:
			f.handleReceiver(c, m, typ, int(reqID))

		case nsMedia:
			f.handleMedia(c, m, body, typ, int(reqID))
		}
	}
}

func (f *fakeReceiver) receiverStatusPayload(reqID int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	apps := []any{}
	if f.launched {
		apps = append(apps, map[string]any{
			"appId": defaultMediaAppID, "sessionId": fakeSessionID, "transportId": fakeTransportID,
		})
	}
	return map[string]any{
		"requestId": reqID,
		"type":      "RECEIVER_STATUS",
		"status": map[string]any{
			"applications": apps,
			"volume":       map[string]any{"level": f.volume, "muted": f.muted},
		},
	}
}

func (f *fakeReceiver) handleReceiver(c net.Conn, m *castMessage, typ string, reqID int) {
	switch typ {
	case "GET_STATUS":
		f.send(c, m.sourceID, nsReceiver, f.receiverStatusPayload(reqID))
	case "LAUNCH":
		f.mu.Lock()
		f.launched = true
		f.mu.Unlock()
		f.send(c, m.sourceID, nsReceiver, f.receiverStatusPayload(reqID))
	case "SET_VOLUME":
		var p struct {
			Volume struct {
				Level *float64 `json:"level"`
				Muted *bool    `json:"muted"`
			} `json:"volume"`
		}
		_ = json.Unmarshal([]byte(m.payloadUTF8), &p)
		f.mu.Lock()
		if p.Volume.Level != nil {
			f.volume = *p.Volume.Level
		}
		if p.Volume.Muted != nil {
			f.muted = *p.Volume.Muted
		}
		f.mu.Unlock()
		f.send(c, m.sourceID, nsReceiver, f.receiverStatusPayload(reqID))
	}
}

func (f *fakeReceiver) mediaStatusPayload(reqID int) map[string]any {
	f.mu.Lock()
	st := f.status
	f.mu.Unlock()
	entry := map[string]any{
		"mediaSessionId": st.MediaSessionID,
		"playerState":    st.PlayerState,
		"currentTime":    st.CurrentTime,
	}
	if st.IdleReason != "" {
		entry["idleReason"] = st.IdleReason
	}
	if st.Media != nil {
		entry["media"] = map[string]any{"duration": st.Media.Duration}
	}
	return map[string]any{"requestId": reqID, "type": "MEDIA_STATUS", "status": []any{entry}}
}

func (f *fakeReceiver) handleMedia(c net.Conn, m *castMessage, body map[string]any, typ string, reqID int) {
	// A real receiver silently drops media messages for a transport the sender
	// never CONNECTed to — the most common implementation mistake.
	f.mu.Lock()
	connected := f.connectedTo[m.destinationID]
	f.mu.Unlock()
	if m.destinationID != fakeTransportID || !connected {
		f.mu.Lock()
		f.dropped = append(f.dropped, typ)
		f.mu.Unlock()
		return
	}

	f.mu.Lock()
	f.mediaCalls = append(f.mediaCalls, typ)
	f.mu.Unlock()

	switch typ {
	case "LOAD":
		f.mu.Lock()
		f.loads = append(f.loads, body)
		fail := f.loadFailCode
		f.mu.Unlock()
		if fail != 0 {
			f.send(c, m.sourceID, nsMedia, map[string]any{
				"requestId": reqID, "type": "LOAD_FAILED", "detailedErrorCode": fail,
			})
			return
		}
		f.send(c, m.sourceID, nsMedia, f.mediaStatusPayload(reqID))
	case "GET_STATUS", "PLAY", "PAUSE", "STOP", "SEEK":
		f.send(c, m.sourceID, nsMedia, f.mediaStatusPayload(reqID))
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-cast-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
