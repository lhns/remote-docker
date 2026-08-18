package tunnel

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestDatagramRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	for _, payload := range [][]byte{[]byte("first"), []byte("second one"), {}} {
		if err := WriteDatagram(&wire, payload); err != nil {
			t.Fatalf("WriteDatagram: %v", err)
		}
	}

	buf := make([]byte, MaxDatagram)
	for _, want := range []string{"first", "second one", ""} {
		n, err := ReadDatagram(&wire, buf)
		if err != nil {
			t.Fatalf("ReadDatagram: %v", err)
		}
		if got := string(buf[:n]); got != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}
	if _, err := ReadDatagram(&wire, buf); !errors.Is(err, io.EOF) {
		t.Errorf("the end of the stream reported %v, want EOF", err)
	}
}

// The boundary is the point. A stream may deliver a datagram in as many pieces
// as it likes, and the reader has to put it back together rather than return
// half a message.
func TestADatagramSplitAcrossReads(t *testing.T) {
	var wire bytes.Buffer
	payload := bytes.Repeat([]byte("x"), 4000)
	if err := WriteDatagram(&wire, payload); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, MaxDatagram)
	n, err := ReadDatagram(oneByteAtATime{&wire}, buf)
	if err != nil {
		t.Fatalf("ReadDatagram: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Errorf("read %d bytes, want the %d written", n, len(payload))
	}
}

// Half a datagram is not a datagram. Reporting EOF here would deliver a
// truncated message as if it were whole.
func TestATruncatedDatagramIsAnError(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteDatagram(&wire, []byte("truncated")); err != nil {
		t.Fatal(err)
	}
	cut := wire.Bytes()[:6]

	buf := make([]byte, MaxDatagram)
	_, err := ReadDatagram(bytes.NewReader(cut), buf)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a truncated datagram reported %v, want ErrUnexpectedEOF", err)
	}
}

func TestADatagramTooBigToCarry(t *testing.T) {
	if err := WriteDatagram(io.Discard, make([]byte, MaxDatagram+1)); err == nil {
		t.Error("a datagram larger than the length field was written")
	}
}

// A buffer smaller than the datagram is the caller's mistake and must not be a
// silent truncation.
func TestABufferTooSmallIsAnError(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteDatagram(&wire, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDatagram(&wire, make([]byte, 4)); err == nil {
		t.Error("a datagram was read into a buffer that cannot hold it")
	}
}

// oneByteAtATime is a stream that never returns a whole datagram at once.
type oneByteAtATime struct{ r io.Reader }

func (o oneByteAtATime) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
