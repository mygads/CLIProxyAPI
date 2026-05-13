// Package eventstream implements a minimal decoder for the AWS vnd.amazon.eventstream
// binary wire format. It covers exactly what Kiro / CodeWhisperer streams
// return — message-type=event frames with a JSON payload — and deliberately
// ignores the parts of the spec we never see in practice (exception frames,
// typed headers beyond string, server-sent pings).
//
// Frame layout (big-endian):
//
//	+---------------------+  0
//	| Total byte length   |  4 bytes
//	+---------------------+
//	| Headers byte length |  4 bytes
//	+---------------------+
//	| Prelude CRC32       |  4 bytes  (over the first 8 bytes)
//	+---------------------+  12
//	| Headers             |  variable
//	+---------------------+
//	| Payload             |  variable (= Total - 12 - HeadersLen - 4)
//	+---------------------+
//	| Message CRC32       |  4 bytes  (over everything except itself)
//	+---------------------+  Total
//
// Headers section is a sequence of:
//
//	[NameLen:1][Name:NameLen][ValueType:1][ValueLen:2][Value:ValueLen]
//
// Only ValueType=7 (String) is supported — that is what CodeWhisperer emits.
package eventstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Message is one decoded event frame.
type Message struct {
	// Headers collects the frame headers by name. Common ones:
	//   ":message-type" = "event" | "exception"
	//   ":event-type"   = e.g. "assistantResponseEvent"
	//   ":content-type" = e.g. "application/json"
	Headers map[string]string
	// Payload is the raw frame body. For CodeWhisperer chat events this is
	// a JSON object shaped like {"content":"..."}.
	Payload []byte
}

// MessageType returns the :message-type header, empty if absent.
func (m *Message) MessageType() string {
	if m == nil || m.Headers == nil {
		return ""
	}
	return m.Headers[":message-type"]
}

// EventType returns the :event-type header, empty if absent.
func (m *Message) EventType() string {
	if m == nil || m.Headers == nil {
		return ""
	}
	return m.Headers[":event-type"]
}

// Decoder reads one event frame at a time from a stream.
type Decoder struct {
	r   io.Reader
	buf []byte // reusable read buffer — grows as frames do
}

// NewDecoder wraps r. The reader is expected to emit frames back-to-back
// with no delimiters (exactly what Kiro's HTTP body contains).
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r, buf: make([]byte, 0, 4096)}
}

var (
	// ErrCorruptPrelude indicates the 12-byte prelude failed its CRC check.
	// This is unrecoverable — we cannot trust byte lengths we just read.
	ErrCorruptPrelude = errors.New("eventstream: prelude CRC mismatch")
	// ErrCorruptMessage indicates the trailing CRC did not match the frame
	// contents. Caller should drop the frame but may keep reading.
	ErrCorruptMessage = errors.New("eventstream: message CRC mismatch")
	// ErrUnsupportedValueType is returned when a header uses a value type
	// other than 7 (String). CodeWhisperer only uses strings.
	ErrUnsupportedValueType = errors.New("eventstream: unsupported header value type")
)

// Next reads and returns the next frame. io.EOF signals the stream ended
// cleanly on a frame boundary.
func (d *Decoder) Next() (*Message, error) {
	// Read the prelude first so we know the total length, THEN read the
	// remainder into the same buffer. We keep the buffer stable across
	// both reads (unlike an earlier version that returned sub-slices from
	// a reused shared buffer — the second read overwrote the first).
	if cap(d.buf) < 12 {
		d.buf = make([]byte, 12, 4096)
	} else {
		d.buf = d.buf[:12]
	}
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		return nil, err
	}
	totalLen := binary.BigEndian.Uint32(d.buf[0:4])
	headersLen := binary.BigEndian.Uint32(d.buf[4:8])
	preludeCRC := binary.BigEndian.Uint32(d.buf[8:12])

	if crc32.ChecksumIEEE(d.buf[0:8]) != preludeCRC {
		return nil, ErrCorruptPrelude
	}
	if totalLen < 16 || headersLen > totalLen-16 {
		// 16 = prelude (12) + trailing CRC (4). Below that no body.
		return nil, fmt.Errorf("eventstream: impossible frame size total=%d headers=%d", totalLen, headersLen)
	}
	payloadLen := totalLen - 12 - headersLen - 4 // -4 for trailing CRC

	// Grow the buffer to hold the whole frame, preserving the prelude we
	// just read into d.buf[0:12].
	if cap(d.buf) < int(totalLen) {
		grown := make([]byte, totalLen)
		copy(grown, d.buf)
		d.buf = grown
	} else {
		d.buf = d.buf[:totalLen]
	}
	if _, err := io.ReadFull(d.r, d.buf[12:totalLen]); err != nil {
		return nil, err
	}
	frame := d.buf[:totalLen]
	headersBuf := frame[12 : 12+headersLen]
	payload := frame[12+headersLen : 12+headersLen+payloadLen]
	trailingCRC := binary.BigEndian.Uint32(frame[totalLen-4 : totalLen])

	// Message CRC is over everything except the trailing CRC itself.
	if crc32.ChecksumIEEE(frame[:totalLen-4]) != trailingCRC {
		return nil, ErrCorruptMessage
	}

	headers, err := parseHeaders(headersBuf)
	if err != nil {
		return nil, err
	}
	// Copy payload so the caller can hold it past the next Next() call.
	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)
	return &Message{Headers: headers, Payload: payloadCopy}, nil
}

// parseHeaders decodes a headers section. Only string-typed values are
// supported — CodeWhisperer never uses the other types.
func parseHeaders(b []byte) (map[string]string, error) {
	out := make(map[string]string, 4)
	for i := 0; i < len(b); {
		if i+1 > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header name length")
		}
		nameLen := int(b[i])
		i++
		if i+nameLen > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header name")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		if i+1 > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header value type")
		}
		valueType := b[i]
		i++
		if valueType != 7 { // 7 = String
			return nil, fmt.Errorf("%w: type=%d name=%q", ErrUnsupportedValueType, valueType, name)
		}
		if i+2 > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header value length")
		}
		valueLen := int(binary.BigEndian.Uint16(b[i : i+2]))
		i += 2
		if i+valueLen > len(b) {
			return nil, fmt.Errorf("eventstream: truncated header value")
		}
		out[name] = string(b[i : i+valueLen])
		i += valueLen
	}
	return out, nil
}
