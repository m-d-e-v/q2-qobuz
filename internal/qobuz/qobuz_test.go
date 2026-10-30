package qobuz

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A script shaped as the web player's 8.3.0: the app id, the sign-in key, and per-zone seeds whose
// joined strings, less 44 characters, are each zone's secret.
func script(secrets map[string]string) string {
	var b strings.Builder
	b.WriteString(`x={production:{api:{appId:"123456789",appSecret:"0123456789abcdef0123456789abcdef"}}};`)
	b.WriteString(`n.authenticate({privateKey:"k3yK3y",code:t});`)
	pad := strings.Repeat("A", 44)
	for zone, secret := range secrets {
		enc := base64.URLEncoding.EncodeToString([]byte(secret)) + pad
		seed, info, extras := enc[:10], enc[10:30], enc[30:]
		lower := strings.ToLower(zone[:1]) + zone[1:]
		b.WriteString(`f.initialSeed("` + seed + `",window.utimezone.` + strings.ToLower(lower) + `);`)
		b.WriteString(`{name:"Europe/` + zone + `",info:"` + info + `",extras:"` + extras + `"},`)
	}
	return b.String()
}

func TestParseBundle(t *testing.T) {
	want := map[string]string{"London": "aaaabbbbccccddddeeeeffff00001111", "Berlin": "99998888777766665555444433332222"}
	c, err := ParseBundle(script(want))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppID != "123456789" || c.PrivateKey != "k3yK3y" || len(c.Secrets) != 2 ||
		c.Secrets["London"] != want["London"] || c.Secrets["Berlin"] != want["Berlin"] {
		t.Fatalf("%+v", c)
	}
	if _, err := ParseBundle("nothing here"); err == nil {
		t.Error("an empty script parsed")
	}
}

func TestSign(t *testing.T) {
	// As every Qobuz client signs getFileUrl: the arguments in name order, then time and secret.
	plain := "trackgetFileUrlformat_id27intentstreamtrack_id64868955" + "1700000000" + "s3cr3t"
	sum := md5.Sum([]byte(plain))
	got := Sign("track", "getFileUrl", map[string]string{"track_id": "64868955", "intent": "stream", "format_id": "27"}, "1700000000", "s3cr3t")
	if got != hex.EncodeToString(sum[:]) {
		t.Fatal(got)
	}
}

func TestSignInURL(t *testing.T) {
	u, _ := url.Parse(SignInURL("123", "http://192.168.1.5:8200/q2-ab"))
	if u.Query().Get("ext_app_id") != "123" || u.Query().Get("redirect_url") != "http://192.168.1.5:8200/q2-ab" {
		t.Fatal(u)
	}
}

func rawBlock(kind byte, last bool, data []byte) []byte {
	h := []byte{kind, byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}
	if last {
		h[0] |= 0x80
	}
	return append(h, data...)
}

func TestReadFLAC(t *testing.T) {
	si := make([]byte, 34)
	// 96000 Hz, 2 channels, 24 bits, 96000*60 samples
	v := uint64(96000)<<44 | uint64(1)<<41 | uint64(23)<<36 | uint64(96000*60)
	binary.BigEndian.PutUint64(si[10:], v)
	var vc bytes.Buffer
	put := func(s string) { binary.Write(&vc, binary.LittleEndian, uint32(len(s))); vc.WriteString(s) }
	put("reference libFLAC")
	binary.Write(&vc, binary.LittleEndian, uint32(3))
	put("title=Song")
	put("ARTIST=One")
	put("ARTIST=Two")
	f := append([]byte("fLaC"), rawBlock(0, false, si)...)
	f = append(f, rawBlock(4, false, vc.Bytes())...)
	f = append(f, rawBlock(6, false, make([]byte, 5000))...)
	f = append(f, rawBlock(1, true, make([]byte, 100))...)
	info, err := ReadFLAC(bytes.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	if info.Rate != 96000 || info.Bits != 24 || info.Channels != 2 || info.Samples != 96000*60 ||
		info.Tags["TITLE"] != "Song" || info.Tags["ARTIST"] != "One; Two" || !info.Picture || info.Padding != 100 {
		t.Fatalf("%+v", info)
	}
	if _, err := ReadFLAC(strings.NewReader("ID3...")); err != ErrNotFLAC {
		t.Error(err)
	}
}

func TestRetag(t *testing.T) {
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], uint64(48000)<<44|uint64(1)<<41|uint64(23)<<36|uint64(48000*62))
	src := append([]byte("fLaC"), rawBlock(0, false, si)...)
	src = append(src, rawBlock(3, false, make([]byte, 18*4))...) // a SEEKTABLE, kept
	src = append(src, rawBlock(1, true, make([]byte, 50))...)    // padding, replaced
	frames := []byte{0xff, 0xf8, 1, 2, 3, 4, 5}
	r := bytes.NewReader(append(append([]byte{}, src...), frames...))
	h, err := ReadHead(r)
	if err != nil || h.Len != int64(len(src)) {
		t.Fatalf("%v %+v", err, h)
	}
	gain, peak := -7.25, 0.98
	tr := Track{ID: "42", Title: "Song", Version: "Live", TrackNumber: 3, MediaNumber: 2, Performer: Artist{Name: "Band"},
		Album: &Album{Title: "Record", Artist: Artist{Name: "Band"}, TracksCount: 12, MediaCount: 2, ReleaseDate: "2016-07-01",
			Genre: Named{"Pop"}}}
	tr.AudioInfo.Gain, tr.AudioInfo.Peak = &gain, &peak
	rest, _ := io.ReadAll(r)
	out := append(h.Build(Tags(tr, nil), Picture{0xff, 0xd8, 0xff, 0xd9}), rest...)
	if !bytes.HasSuffix(out, frames) {
		t.Fatal("frames changed")
	}
	info, err := ReadFLAC(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"TITLE": "Song (Live)", "ARTIST": "Band", "ALBUM": "Record", "ALBUMARTIST": "Band",
		"TRACKNUMBER": "3", "TRACKTOTAL": "12", "DISCNUMBER": "2", "DISCTOTAL": "2", "DATE": "2016-07-01", "GENRE": "Pop",
		"REPLAYGAIN_TRACK_GAIN": "-7.25 dB", "REPLAYGAIN_TRACK_PEAK": "0.980000", "QOBUZ_TRACK_ID": "42"}
	for k, v := range want {
		if info.Tags[k] != v {
			t.Errorf("%s = %q, want %q", k, info.Tags[k], v)
		}
	}
	if _, ok := info.Tags["COMPOSER"]; ok {
		t.Error("an empty tag written")
	}
	if !info.Picture || info.Rate != 48000 || info.Bits != 24 || info.Padding != 4096 {
		t.Errorf("%+v", info)
	}
}

func TestID3(t *testing.T) {
	gain := -3.5
	tr := Track{ID: "7", Title: "Song", Version: "Live", TrackNumber: 3, MediaNumber: 1, Performer: Artist{Name: "Band"},
		Album: &Album{Title: "Record", Artist: Artist{Name: "The Band"}, TracksCount: 12, MediaCount: 2, ReleaseDate: "2016-07-01"}}
	tr.AudioInfo.Gain = &gain
	tag := ID3(Tags(tr, nil), Picture{0xff, 0xd8, 0xff, 0xd9})
	got, pic, err := ReadID3(bytes.NewReader(tag))
	if err != nil || !pic {
		t.Fatal(err, pic)
	}
	want := map[string]string{"TIT2": "Song (Live)", "TPE1": "Band", "TALB": "Record", "TPE2": "The Band", "TRCK": "3/12",
		"TPOS": "1/2", "TDRC": "2016-07-01", "REPLAYGAIN_TRACK_GAIN": "-3.50 dB", "QOBUZ_TRACK_ID": "7"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// SkipID3: past a tag the file came with, or nothing before a frame; anything else isn't MP3.
	frames := []byte{0xff, 0xfb, 0x90, 0x64, 1, 2, 3}
	br := bufio.NewReader(bytes.NewReader(append(append([]byte{}, tag...), frames...)))
	if n, err := SkipID3(br); err != nil || n != int64(len(tag)) {
		t.Fatal(n, err)
	}
	if rest, _ := io.ReadAll(br); !bytes.Equal(rest, frames) {
		t.Fatal("frames")
	}
	if n, err := SkipID3(bufio.NewReader(bytes.NewReader(frames))); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := SkipID3(bufio.NewReader(strings.NewReader("fLaC and more bytes"))); err == nil {
		t.Fatal("FLAC taken for MP3")
	}
}

// A box set whose later page fails is an error, not an album of its first 500 tracks.
func TestAlbumPageFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		items := strings.Repeat(`{"id":1},`, 500)
		fmt.Fprintf(w, `{"id":"box","tracks":{"total":600,"offset":0,"limit":500,"items":[%s]}}`, strings.TrimSuffix(items, ","))
	}))
	defer srv.Close()
	old := API
	API = srv.URL + "/"
	defer func() { API = old }()
	c := &Client{HTTP: srv.Client(), Creds: &Creds{AppID: "1"}}
	if _, err := c.Album(context.Background(), "box"); err == nil {
		t.Fatal("a partial box set taken as whole")
	}
}

// A secret is found on a track Qobuz sends no file for (a rights holder's restriction): the answer
// is a signature taken. FileURL's answer for it is a 404, not tried again.
func TestFindSecretNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		args := map[string]string{"format_id": q.Get("format_id"), "intent": q.Get("intent"), "track_id": q.Get("track_id")}
		if q.Get("request_sig") != Sign("track", "getFileUrl", args, q.Get("request_ts"), "right") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"Invalid Request Signature parameter (request_sig)"}`)
			return
		}
		fmt.Fprint(w, `{"track_id":1,"restrictions":[{"code":"TrackRestrictedByRightHolders"}]}`)
	}))
	defer srv.Close()
	old := API
	API = srv.URL + "/"
	defer func() { API = old }()
	c := &Client{HTTP: srv.Client(), Creds: &Creds{AppID: "1", Secrets: map[string]string{"Berlin": "wrong", "London": "right"}}}
	if s, err := c.FindSecret(context.Background(), "1"); err != nil || s != "right" {
		t.Fatal(s, err)
	}
	_, err := c.FileURL(context.Background(), "1", CD, "right")
	var e *Error
	if !errors.Is(err, ErrNoFile) || !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatal(err)
	}
	// No secret taken: the last refusal, as an API error.
	c.Creds.Secrets = map[string]string{"Berlin": "wrong"}
	if _, err := c.FindSecret(context.Background(), "1"); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatal(err)
	}
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no route to host")
}

// A failure to reach Qobuz doesn't carry the request's address: a sign-in's sends its code and
// the private key in the query.
func TestNetErrorHidesQuery(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: failing{}}, Creds: &Creds{AppID: "1", PrivateKey: "pk-hidden"}}
	err := c.Exchange(context.Background(), "code-hidden")
	if err == nil || strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "http") ||
		err.Error() != "can't reach Qobuz: no route to host" {
		t.Fatal(err)
	}
	_, err = FetchCreds(context.Background(), &http.Client{Transport: failing{}}, nil)
	if !errors.Is(err, ErrCreds) || strings.Contains(err.Error(), "http") {
		t.Fatal(err)
	}
}
