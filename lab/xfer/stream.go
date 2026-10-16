package main

import (
	"encoding/binary"
	"io"
	"time"

	"uulab/fakeenc"
	"uulab/transport"
)

// quic / tcp 模式里每帧前面的帧头: id u32 | ts u32 | flags u8 | size u32，后面跟 size 字节负载。
// flags: 1 = 关键帧，2 = BYE（此时 id 字段是总帧数）。
const (
	hdrLen  = 13
	hdrKey  = 1
	hdrBye  = 2
	maxSize = 4 << 20
)

var zeros = make([]byte, maxSize)

func writeFrame(w io.Writer, f fakeenc.Frame) error {
	var h [hdrLen]byte
	binary.BigEndian.PutUint32(h[0:], f.ID)
	binary.BigEndian.PutUint32(h[4:], uint32(f.At.UnixMicro()))
	if f.Key {
		h[8] = hdrKey
	}
	binary.BigEndian.PutUint32(h[9:], uint32(f.Size))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(zeros[:min(f.Size, maxSize)])
	return err
}

func writeBye(w io.Writer, frames uint32) error {
	var h [hdrLen]byte
	binary.BigEndian.PutUint32(h[0:], frames)
	h[8] = hdrBye
	_, err := w.Write(h[:])
	return err
}

// readFrame 读一帧。bye 为 true 时 id 是总帧数。
func readFrame(r io.Reader) (id uint32, key, bye bool, lat time.Duration, err error) {
	var h [hdrLen]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	id = binary.BigEndian.Uint32(h[0:])
	if h[8]&hdrBye != 0 {
		return id, false, true, 0, nil
	}
	size := int64(binary.BigEndian.Uint32(h[9:]))
	if _, err = io.CopyN(io.Discard, r, size); err != nil {
		return
	}
	return id, h[8]&hdrKey != 0, false, transport.Since32(binary.BigEndian.Uint32(h[4:])), nil
}
