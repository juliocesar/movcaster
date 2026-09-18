package googlecast

import (
	"encoding/binary"
	"fmt"
	"io"
)

// castMessage is the CASTV2 envelope: one protobuf message, seven scalar
// fields, framed on the wire by a 4-byte big-endian length prefix.
//
// The schema is Chromium's components/cast_channel/proto/cast_channel.proto.
// The codec here is hand-rolled instead of protoc-generated: the message is
// small and flat (varint + length-delimited fields only), the generated code
// plus google.golang.org/protobuf would be the project's largest dependency,
// and the round-trip is locked down by tests. If the schema ever grows beyond
// this shape, switch to generated code rather than extending this by hand.
type castMessage struct {
	// protocolVersion is field 1; always castV2_1_0.
	protocolVersion uint64
	sourceID        string // field 2
	destinationID   string // field 3
	namespace       string // field 4
	// payloadType is field 5: 0 = STRING (payloadUTF8), 1 = BINARY.
	payloadType uint64
	payloadUTF8 string // field 6
	// payloadBinary (field 7) is unused by this sender but tolerated on decode.
	payloadBinary []byte
}

const (
	castV2_1_0         = 0
	payloadTypeString  = 0
	payloadTypeBinary  = 1
	maxCastMessageSize = 1 << 20 // a receiver frame beyond 1MB is a protocol error
)

// marshal encodes the message body (no length prefix).
func (m *castMessage) marshal() []byte {
	var b []byte
	b = appendVarintField(b, 1, m.protocolVersion)
	b = appendStringField(b, 2, m.sourceID)
	b = appendStringField(b, 3, m.destinationID)
	b = appendStringField(b, 4, m.namespace)
	b = appendVarintField(b, 5, m.payloadType)
	if m.payloadType == payloadTypeBinary {
		b = appendBytesField(b, 7, m.payloadBinary)
	} else {
		b = appendStringField(b, 6, m.payloadUTF8)
	}
	return b
}

// unmarshal decodes a message body (no length prefix). Unknown fields are
// skipped, as protobuf requires.
func (m *castMessage) unmarshal(b []byte) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return fmt.Errorf("cast message: bad field key")
		}
		b = b[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0: // varint
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return fmt.Errorf("cast message: bad varint in field %d", field)
			}
			b = b[n:]
			switch field {
			case 1:
				m.protocolVersion = v
			case 5:
				m.payloadType = v
			}
		case 2: // length-delimited
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return fmt.Errorf("cast message: bad length in field %d", field)
			}
			v := b[n : n+int(l)]
			b = b[n+int(l):]
			switch field {
			case 2:
				m.sourceID = string(v)
			case 3:
				m.destinationID = string(v)
			case 4:
				m.namespace = string(v)
			case 6:
				m.payloadUTF8 = string(v)
			case 7:
				m.payloadBinary = append([]byte(nil), v...)
			}
		case 1: // 64-bit, unused by the schema but skippable
			if len(b) < 8 {
				return fmt.Errorf("cast message: truncated fixed64")
			}
			b = b[8:]
		case 5: // 32-bit
			if len(b) < 4 {
				return fmt.Errorf("cast message: truncated fixed32")
			}
			b = b[4:]
		default:
			return fmt.Errorf("cast message: unsupported wire type %d", wire)
		}
	}
	return nil
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|0)
	return binary.AppendUvarint(b, v)
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendStringField(b []byte, field int, v string) []byte {
	return appendBytesField(b, field, []byte(v))
}

// writeFrame writes one length-prefixed message.
func writeFrame(w io.Writer, m *castMessage) error {
	body := m.marshal()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// readFrame reads one length-prefixed message.
func readFrame(r io.Reader) (*castMessage, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size > maxCastMessageSize {
		return nil, fmt.Errorf("cast message of %d bytes exceeds the protocol maximum", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	m := &castMessage{}
	if err := m.unmarshal(body); err != nil {
		return nil, err
	}
	return m, nil
}
