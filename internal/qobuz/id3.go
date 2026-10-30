package qobuz

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// MP3s (format MP3, 320 kbit/s) are tagged as FLACs are, with an ID3v2.4 tag in UTF-8 at their
// start, which TagLib (the Q2's player and library scan) reads, and Q2 Pod's MP3 ReplayGain too
// (TXXX REPLAYGAIN_TRACK_GAIN). A tag the file came with is left out; the MPEG frames follow as sent.

// SkipID3 reads past an ID3v2 tag at r's start, if there is one; how many bytes it took.
func SkipID3(r *bufio.Reader) (int64, error) {
	h, err := r.Peek(10)
	if len(h) < 10 { // a very short file: an MP3 only if it starts with a frame
		if len(h) >= 2 && h[0] == 0xff && h[1]&0xe0 == 0xe0 {
			return 0, nil
		}
		if err == nil {
			err = errors.New("not an MP3 file")
		}
		return 0, err
	}
	if string(h[:3]) != "ID3" {
		if h[0] != 0xff || h[1]&0xe0 != 0xe0 {
			return 0, errors.New("not an MP3 file")
		}
		return 0, nil
	}
	n := int64(syncsafe(h[6:10])) + 10
	if h[5]&0x10 != 0 { // a footer
		n += 10
	}
	if _, err := r.Discard(int(n)); err != nil {
		return 0, err
	}
	return n, nil
}

func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

func putSyncsafe(b []byte, n int) {
	b[0], b[1], b[2], b[3] = byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f)
}

func frame(id string, body []byte) []byte {
	f := make([]byte, 10, 10+len(body))
	copy(f, id)
	putSyncsafe(f[4:8], len(body))
	return append(f, body...)
}

func text(id, s string) []byte { return frame(id, append([]byte{3}, s...)) } // 3: UTF-8

func txxx(desc, s string) []byte {
	return frame("TXXX", append(append(append([]byte{3}, desc...), 0), s...))
}

// ID3 is an ID3v2.4 tag holding tags (Vorbis comment names, as Tags makes them) and the cover.
func ID3(tags []Tag, cover Picture) []byte {
	get := map[string]string{}
	for _, t := range tags {
		if strings.TrimSpace(t.Value) != "" {
			get[t.Name] = t.Value
		}
	}
	var frames bytes.Buffer
	for _, f := range []struct{ id, tag string }{{"TIT2", "TITLE"}, {"TPE1", "ARTIST"}, {"TALB", "ALBUM"},
		{"TPE2", "ALBUMARTIST"}, {"TCOM", "COMPOSER"}, {"TCON", "GENRE"}, {"TPUB", "ORGANIZATION"},
		{"TSRC", "ISRC"}, {"TCOP", "COPYRIGHT"}, {"TDRC", "DATE"}} {
		if v := get[f.tag]; v != "" {
			frames.Write(text(f.id, v))
		}
	}
	of := func(n, total string) string {
		if n != "" && total != "" {
			return n + "/" + total
		}
		return n
	}
	if v := of(get["TRACKNUMBER"], get["TRACKTOTAL"]); v != "" {
		frames.Write(text("TRCK", v))
	}
	if v := of(get["DISCNUMBER"], get["DISCTOTAL"]); v != "" {
		frames.Write(text("TPOS", v))
	}
	for _, k := range []string{"REPLAYGAIN_TRACK_GAIN", "REPLAYGAIN_TRACK_PEAK", "QOBUZ_TRACK_ID"} {
		if v := get[k]; v != "" {
			frames.Write(txxx(k, v))
		}
	}
	if len(cover) > 0 && len(cover) < 8<<20 {
		apic := append([]byte{0}, "image/jpeg"...) // 0: ISO-8859-1 for the (empty) description
		apic = append(apic, 0, 3, 0)               // the MIME's end, a front cover, an empty description
		frames.Write(frame("APIC", append(apic, cover...)))
	}
	frames.Write(make([]byte, 1024)) // padding, for a tag editor's later change
	h := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
	putSyncsafe(h[6:10], frames.Len())
	return append(h, frames.Bytes()...)
}

// ReadID3 is an ID3v2.4 tag's text frames, as ID3 writes them (TXXX by their description), and
// whether it has a picture: enough for the tests and the probe.
func ReadID3(r io.Reader) (map[string]string, bool, error) {
	h := make([]byte, 10)
	if _, err := io.ReadFull(r, h); err != nil || string(h[:3]) != "ID3" {
		return nil, false, errors.New("no ID3v2 tag")
	}
	body := make([]byte, syncsafe(h[6:10]))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, false, err
	}
	out, pic := map[string]string{}, false
	for len(body) >= 10 && body[0] != 0 {
		id, n := string(body[:4]), int(syncsafe(body[4:8]))
		if 10+n > len(body) {
			break
		}
		f := body[10 : 10+n]
		switch {
		case id == "APIC":
			pic = true
		case id == "TXXX" && len(f) > 1:
			if desc, val, ok := bytes.Cut(f[1:], []byte{0}); ok {
				out[string(desc)] = string(val)
			}
		case id[0] == 'T' && len(f) > 1:
			out[id] = string(f[1:])
		}
		body = body[10+n:]
	}
	return out, pic, nil
}
