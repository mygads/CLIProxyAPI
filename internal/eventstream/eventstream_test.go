package eventstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// buildFrame constructs a canonical event-stream frame for a test. It is
// the inverse of Decoder.Next so round-trip tests are easy to write.
func buildFrame(t *testing.T, headers map[string]string, payload []byte) []byte {
	t.Helper()
	hdr := &bytes.Buffer{}
	for name, value := range headers {
		if len(name) > 255 {
			t.Fatalf("header name too long: %q", name)
		}
		if len(value) > 65535 {
			t.Fatalf("header value too long: %q", name)
		}
		hdr.WriteByte(byte(len(name)))
		hdr.WriteString(name)
		hdr.WriteByte(7) // value type: String
		_ = binary.Write(hdr, binary.BigEndian, uint16(len(value)))
		hdr.WriteString(value)
	}
	headersBytes := hdr.Bytes()

	totalLen := uint32(12 + len(headersBytes) + len(payload) + 4)
	headersLen := uint32(len(headersBytes))

	frame := &bytes.Buffer{}
	_ = binary.Write(frame, binary.BigEndian, totalLen)
	_ = binary.Write(frame, binary.BigEndian, headersLen)
	preludeCRC := crc32.ChecksumIEEE(frame.Bytes())
	_ = binary.Write(frame, binary.BigEndian, preludeCRC)
	frame.Write(headersBytes)
	frame.Write(payload)
	msgCRC := crc32.ChecksumIEEE(frame.Bytes())
	_ = binary.Write(frame, binary.BigEndian, msgCRC)
	return frame.Bytes()
}

func TestDecoder_singleFrameRoundTrip(t *testing.T) {
	headers := map[string]string{
		":message-type": "event",
		":event-type":   "assistantResponseEvent",
		":content-type": "application/json",
	}
	payload := []byte(`{"content":"hello"}`)
	frame := buildFrame(t, headers, payload)

	dec := NewDecoder(bytes.NewReader(frame))
	msg, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if msg.MessageType() != "event" {
		t.Errorf("message-type: got %q", msg.MessageType())
	}
	if msg.EventType() != "assistantResponseEvent" {
		t.Errorf("event-type: got %q", msg.EventType())
	}
	if string(msg.Payload) != string(payload) {
		t.Errorf("payload: got %q want %q", msg.Payload, payload)
	}

	// Next call should return EOF on a clean boundary.
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after last frame, got %v", err)
	}
}

func TestDecoder_multipleFrames(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		payload := []byte(`{"content":"chunk-` + string(rune('A'+i)) + `"}`)
		buf.Write(buildFrame(t, map[string]string{
			":message-type": "event",
			":event-type":   "assistantResponseEvent",
		}, payload))
	}

	dec := NewDecoder(&buf)
	got := []string{}
	for {
		msg, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, string(msg.Payload))
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	for i, p := range got {
		want := `{"content":"chunk-` + string(rune('A'+i)) + `"}`
		if p != want {
			t.Errorf("frame %d: got %q want %q", i, p, want)
		}
	}
}

func TestDecoder_corruptPreludeCRC(t *testing.T) {
	frame := buildFrame(t, map[string]string{":message-type": "event"}, []byte(`{}`))
	// Flip a bit in the first byte so prelude CRC no longer matches.
	frame[0] ^= 0x01
	dec := NewDecoder(bytes.NewReader(frame))
	_, err := dec.Next()
	if !errors.Is(err, ErrCorruptPrelude) {
		t.Fatalf("want ErrCorruptPrelude, got %v", err)
	}
}

func TestDecoder_corruptMessageCRC(t *testing.T) {
	frame := buildFrame(t, map[string]string{":message-type": "event"}, []byte(`{}`))
	// Flip a bit in the payload so the trailing CRC mismatches.
	frame[len(frame)-10] ^= 0x01
	dec := NewDecoder(bytes.NewReader(frame))
	_, err := dec.Next()
	if !errors.Is(err, ErrCorruptMessage) {
		t.Fatalf("want ErrCorruptMessage, got %v", err)
	}
}

func TestDecoder_eofMidPrelude(t *testing.T) {
	// Fewer than 12 bytes available — ReadFull returns ErrUnexpectedEOF.
	dec := NewDecoder(bytes.NewReader([]byte{1, 2, 3}))
	_, err := dec.Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
}

func TestDecoder_rejectsNonStringHeaders(t *testing.T) {
	// Construct a frame with a non-string-typed header (type 6 = timestamp).
	var hdr bytes.Buffer
	name := "x-stamp"
	hdr.WriteByte(byte(len(name)))
	hdr.WriteString(name)
	hdr.WriteByte(6) // unsupported
	_ = binary.Write(&hdr, binary.BigEndian, uint64(1234567890))

	headersBytes := hdr.Bytes()
	payload := []byte(`{}`)
	totalLen := uint32(12 + len(headersBytes) + len(payload) + 4)
	headersLen := uint32(len(headersBytes))

	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, totalLen)
	_ = binary.Write(&frame, binary.BigEndian, headersLen)
	preludeCRC := crc32.ChecksumIEEE(frame.Bytes())
	_ = binary.Write(&frame, binary.BigEndian, preludeCRC)
	frame.Write(headersBytes)
	frame.Write(payload)
	msgCRC := crc32.ChecksumIEEE(frame.Bytes())
	_ = binary.Write(&frame, binary.BigEndian, msgCRC)

	dec := NewDecoder(bytes.NewReader(frame.Bytes()))
	_, err := dec.Next()
	if !errors.Is(err, ErrUnsupportedValueType) {
		t.Fatalf("want ErrUnsupportedValueType, got %v", err)
	}
	// Error message should mention the header name to aid debugging.
	if !strings.Contains(err.Error(), "x-stamp") {
		t.Errorf("error missing header name: %v", err)
	}
}
