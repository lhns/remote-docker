package tunnel

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MaxDatagram is the largest datagram that can be carried, which is the largest
// one that can exist: a UDP payload is bounded by a 16-bit length field.
const MaxDatagram = 65535

// A datagram keeps its boundary because of the length in front of it.
//
// An SSH channel is a byte STREAM: it preserves order and content and says
// nothing about where one write ended, and it may split or join them however
// it likes. UDP is the opposite, where the boundary is the message, so a plain
// copy would deliver "abc" and "de" as "abcde" or "ab" "cde" and the receiver
// would have no way to know. Two bytes of length restore what the stream took
// away.
//
// Big-endian, because that is what every other length on the wire here is.

// WriteDatagram writes one datagram with its length in front.
func WriteDatagram(w io.Writer, payload []byte) error {
	if len(payload) > MaxDatagram {
		return fmt.Errorf("tunnel: datagram of %d bytes, limit %d", len(payload), MaxDatagram)
	}

	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))

	// One Write, not two: a datagram split across two writes on a channel that
	// is also carrying others is a header that can be interleaved with somebody
	// else's payload.
	frame := make([]byte, 0, 2+len(payload))
	frame = append(frame, header[:]...)
	frame = append(frame, payload...)

	_, err := w.Write(frame)
	return err
}

// ReadDatagram reads one datagram into buf.
//
// io.EOF means the far end finished, which for a datagram flow is the only
// close there is. An EOF in the MIDDLE of a datagram is io.ErrUnexpectedEOF,
// because losing half a message quietly is how a stream pretends to be a
// datagram.
func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}

	size := int(binary.BigEndian.Uint16(header[:]))
	if size > len(buf) {
		return 0, fmt.Errorf("tunnel: datagram of %d bytes into a buffer of %d", size, len(buf))
	}
	if _, err := io.ReadFull(r, buf[:size]); err != nil {
		return 0, err
	}
	return size, nil
}
