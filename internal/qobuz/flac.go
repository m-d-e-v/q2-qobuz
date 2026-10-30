package qobuz

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// FLACInfo is what a FLAC file's metadata blocks say: its format, its tags and whether it carries
// a picture. It reads only the blocks before the audio.
type FLACInfo struct {
	Rate, Channels, Bits int
	Samples              int64
	Tags                 map[string]string // upper-case names; repeats joined with "; "
	Vendor               string
	Picture              bool
	PictureBytes         int
	Padding              int
}

var ErrNotFLAC = errors.New("not a FLAC file")

// ReadFLAC reads r's metadata blocks.
func ReadFLAC(r io.Reader) (*FLACInfo, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, ErrNotFLAC
	}
	info := &FLACInfo{Tags: map[string]string{}}
	for last := false; !last; {
		var h [4]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("FLAC block header: %w", err)
		}
		last = h[0]&0x80 != 0
		kind, n := h[0]&0x7f, int(h[1])<<16|int(h[2])<<8|int(h[3])
		if n > 16<<20 {
			return nil, fmt.Errorf("FLAC block of %d bytes", n)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("FLAC block: %w", err)
		}
		switch kind {
		case 0: // STREAMINFO
			if n < 18 {
				return nil, errors.New("short STREAMINFO")
			}
			v := binary.BigEndian.Uint64(b[10:18])
			info.Rate = int(v >> 44)
			info.Channels = int(v>>41&7) + 1
			info.Bits = int(v>>36&31) + 1
			info.Samples = int64(v & (1<<36 - 1))
		case 1:
			info.Padding += n
		case 4: // VORBIS_COMMENT, little-endian lengths
			comments(b, info)
		case 6:
			info.Picture, info.PictureBytes = true, n
		}
	}
	return info, nil
}

func comments(b []byte, info *FLACInfo) {
	next := func() (string, bool) {
		if len(b) < 4 {
			return "", false
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n > len(b)-4 {
			return "", false
		}
		s := string(b[4 : 4+n])
		b = b[4+n:]
		return s, true
	}
	info.Vendor, _ = next()
	if len(b) < 4 {
		return
	}
	count := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	for i := 0; i < count; i++ {
		c, ok := next()
		if !ok {
			return
		}
		if k, v, ok := strings.Cut(c, "="); ok {
			k = strings.ToUpper(k)
			if old, dup := info.Tags[k]; dup {
				v = old + "; " + v
			}
			info.Tags[k] = v
		}
	}
}
