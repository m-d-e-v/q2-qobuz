package qobuz

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"strconv"
	"strings"
)

// Qobuz's files carry no tags, so the Q2 would show them by file name. A download is given its tags
// as it arrives: Head reads the source's metadata blocks, then Build writes the file's start anew,
// its STREAMINFO and SEEKTABLE kept, with tags, the cover and padding, and the audio frames follow
// unchanged.

// Head is a FLAC stream's metadata as read from its start.
type Head struct {
	blocks []block // kept: STREAMINFO first, then SEEKTABLE and CUESHEET
	Len    int64   // the source's bytes up to the first audio frame
}

type block struct {
	kind byte
	data []byte
}

// ReadHead reads r up to the first audio frame.
func ReadHead(r io.Reader) (*Head, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, ErrNotFLAC
	}
	h := &Head{Len: 4}
	for last := false; !last; {
		var hd [4]byte
		if _, err := io.ReadFull(r, hd[:]); err != nil {
			return nil, fmt.Errorf("FLAC block header: %w", err)
		}
		last = hd[0]&0x80 != 0
		kind, n := hd[0]&0x7f, int(hd[1])<<16|int(hd[2])<<8|int(hd[3])
		if n > 16<<20 {
			return nil, fmt.Errorf("FLAC block of %d bytes", n)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("FLAC block: %w", err)
		}
		h.Len += 4 + int64(n)
		switch kind {
		case 0, 3, 5: // STREAMINFO, SEEKTABLE, CUESHEET
			h.blocks = append(h.blocks, block{kind, data})
		}
	}
	if len(h.blocks) == 0 || h.blocks[0].kind != 0 {
		return nil, errors.New("FLAC without STREAMINFO first")
	}
	return h, nil
}

// Tag is one Vorbis comment.
type Tag struct{ Name, Value string }

// Picture is a front cover, as JPEG bytes.
type Picture []byte

// Build is the file's new start: "fLaC", the kept blocks, the tags, the cover and 4 KB of padding.
func (h *Head) Build(tags []Tag, cover Picture) []byte {
	blocks := []block{h.blocks[0]}
	var vc bytes.Buffer
	put := func(s string) {
		binary.Write(&vc, binary.LittleEndian, uint32(len(s)))
		vc.WriteString(s)
	}
	put("q2-qobuz")
	var kept []Tag
	for _, t := range tags {
		if strings.TrimSpace(t.Value) != "" {
			kept = append(kept, t)
		}
	}
	binary.Write(&vc, binary.LittleEndian, uint32(len(kept)))
	for _, t := range kept {
		put(strings.ToUpper(t.Name) + "=" + t.Value)
	}
	blocks = append(blocks, h.blocks[1:]...)
	blocks = append(blocks, block{4, vc.Bytes()})
	if len(cover) > 0 && len(cover) < 8<<20 {
		blocks = append(blocks, block{6, picture(cover)})
	}
	blocks = append(blocks, block{1, make([]byte, 4096)})
	var out bytes.Buffer
	out.WriteString("fLaC")
	for i, b := range blocks {
		kind := b.kind
		if i == len(blocks)-1 {
			kind |= 0x80
		}
		n := len(b.data)
		out.Write([]byte{kind, byte(n >> 16), byte(n >> 8), byte(n)})
		out.Write(b.data)
	}
	return out.Bytes()
}

// picture is a PICTURE block for a front cover (type 3).
func picture(jpg []byte) []byte {
	w, h := 0, 0
	if c, err := jpeg.DecodeConfig(bytes.NewReader(jpg)); err == nil {
		w, h = c.Width, c.Height
	}
	var b bytes.Buffer
	be := func(v uint32) { binary.Write(&b, binary.BigEndian, v) }
	be(3)
	be(uint32(len("image/jpeg")))
	b.WriteString("image/jpeg")
	be(0) // no description
	be(uint32(w))
	be(uint32(h))
	be(24)
	be(0)
	be(uint32(len(jpg)))
	b.Write(jpg)
	return b.Bytes()
}

// Tags are a track's Vorbis comments, from its catalogue entry and its album's.
func Tags(t Track, a *Album) []Tag {
	if a == nil {
		a = t.Album
	}
	if a == nil {
		a = &Album{}
	}
	tags := []Tag{
		{"TITLE", FullTitle(t.Title, t.Version)},
		{"ARTIST", t.Performer.Name},
		{"ALBUM", FullTitle(a.Title, a.Version)},
		{"ALBUMARTIST", a.Artist.Name},
		{"COMPOSER", t.Composer.Name},
		{"GENRE", a.Genre.Name},
		{"ORGANIZATION", a.Label.Name},
		{"ISRC", t.ISRC},
		{"COPYRIGHT", t.Copyright},
		{"QOBUZ_TRACK_ID", t.ID.String()},
	}
	if tags[1].Value == "" {
		tags[1].Value = a.Artist.Name
	}
	if a.ReleaseDate != "" {
		tags = append(tags, Tag{"DATE", a.ReleaseDate})
	}
	if t.TrackNumber > 0 {
		tags = append(tags, Tag{"TRACKNUMBER", strconv.Itoa(t.TrackNumber)})
	}
	if a.TracksCount > 0 {
		tags = append(tags, Tag{"TRACKTOTAL", strconv.Itoa(a.TracksCount)})
	}
	if t.MediaNumber > 0 {
		tags = append(tags, Tag{"DISCNUMBER", strconv.Itoa(t.MediaNumber)})
	}
	if a.MediaCount > 0 {
		tags = append(tags, Tag{"DISCTOTAL", strconv.Itoa(a.MediaCount)})
	}
	if g := t.AudioInfo.Gain; g != nil {
		tags = append(tags, Tag{"REPLAYGAIN_TRACK_GAIN", strconv.FormatFloat(*g, 'f', 2, 64) + " dB"})
	}
	if p := t.AudioInfo.Peak; p != nil {
		tags = append(tags, Tag{"REPLAYGAIN_TRACK_PEAK", strconv.FormatFloat(*p, 'f', 6, 64)})
	}
	return tags
}
