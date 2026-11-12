package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MaxUDPPacketSize is the maximum supported packet payload size (65535 bytes).
const MaxUDPPacketSize = 65535

// WritePacket writes a 2-byte big-endian length prefix followed by pkt bytes.
// The frame is constructed and written in a single Write call to avoid interleaving.
func WritePacket(w io.Writer, pkt []byte) error {
	if len(pkt) > MaxUDPPacketSize {
		return fmt.Errorf("packet length %d exceeds max %d", len(pkt), MaxUDPPacketSize)
	}
	frame := make([]byte, 2+len(pkt))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(pkt)))
	copy(frame[2:], pkt)
	_, err := w.Write(frame)
	return err
}

// ReadPacket reads a 2-byte big-endian length prefix followed by payload into buf.
// Returns the number of payload bytes read.
func ReadPacket(r io.Reader, buf []byte) (int, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n > len(buf) {
		return 0, fmt.Errorf("packet size %d exceeds buffer capacity %d", n, len(buf))
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}
