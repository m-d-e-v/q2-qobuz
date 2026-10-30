package dl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"q2-qobuz/internal/cache"
	"q2-qobuz/internal/qobuz"
)

// flac is a source file as Qobuz sends one: STREAMINFO, a SEEKTABLE, padding, no tags, then frames.
func flac(frames int) []byte {
	b := []byte("fLaC")
	blk := func(kind byte, last bool, data []byte) {
		if last {
			kind |= 0x80
		}
		b = append(b, kind, byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
		b = append(b, data...)
	}
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], uint64(96000)<<44|uint64(1)<<41|uint64(23)<<36|uint64(96000*240))
	blk(0, false, si)
	blk(3, false, make([]byte, 36))
	blk(1, true, make([]byte, 8192))
	for i := 0; i < frames; i++ {
		b = append(b, 0xff, 0xf8, byte(i), byte(i>>8))
	}
	return b
}

type fake struct {
	srv    *httptest.Server
	file   []byte
	mp3    []byte      // what an MP3 request gets: a tag of its own, then frames
	cut    atomic.Bool // the next audio request sends a third and drops
	slow   atomic.Bool // audio comes 64 KB at a time
	auth   atomic.Bool // File answers 401
	ranges []string
	covers atomic.Int32
}

func (f *fake) File(ctx context.Context, id string, quality int) (*qobuz.File, error) {
	if f.auth.Load() {
		return nil, &qobuz.Error{Status: 401, Message: "User authentication is required"}
	}
	if quality == qobuz.MP3 {
		return &qobuz.File{URL: f.srv.URL + "/mp3/" + id, FormatID: 5, MimeType: "audio/mpeg"}, nil
	}
	return &qobuz.File{URL: f.srv.URL + "/audio/" + id, FormatID: 7, BitDepth: 24, SamplingRate: 96, MimeType: "audio/flac"}, nil
}

func (f *fake) Cover(ctx context.Context, a *qobuz.Album) []byte {
	f.covers.Add(1)
	return []byte{0xff, 0xd8, 0xff, 0xd9}
}

func setup(t *testing.T) (*fake, *Downloader, *cache.Cache, *atomic.Int32) {
	theirs := []byte("ID3\x04\x00\x00\x00\x00\x00\x05XXXXX") // a 5-byte tag of Qobuz's own
	f := &fake{file: flac(300000), mp3: append(theirs, bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64, 7, 7, 7, 7}, 150000)...)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		body := f.file
		if strings.HasPrefix(r.URL.Path, "/mp3/") {
			body = f.mp3
		}
		if f.cut.CompareAndSwap(true, false) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body[:len(body)/3])
			w.(http.Flusher).Flush()
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
		if f.slow.Load() {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			for i := 0; i < len(body); i += 64 << 10 {
				w.Write(body[i:min(i+64<<10, len(body))])
				w.(http.Flusher).Flush()
				time.Sleep(20 * time.Millisecond)
			}
			return
		}
		http.ServeContent(w, r, "a", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(f.srv.Close)
	dir := t.TempDir()
	c := &cache.Cache{Dir: filepath.Join(dir, "cache")}
	var auths atomic.Int32
	RetryAfter = 10 * time.Millisecond
	d := New(Options{Dir: dir, Cache: c, Source: f, Fetch: &http.Client{}, Ready: func() bool { return true },
		Limit: func() int64 { return 1 << 30 }, Quality: func() int { return qobuz.HiRes96 }, OnAuth: func() { auths.Add(1) }})
	t.Cleanup(d.Stop)
	return f, d, c, &auths
}

func track(id string) qobuz.Track {
	return qobuz.Track{ID: qobuz.ID(id), Title: "Song " + id, TrackNumber: 1, Performer: qobuz.Artist{Name: "Band"},
		Album: &qobuz.Album{ID: "al1", Title: "Record", Artist: qobuz.Artist{Name: "Band"}, TracksCount: 9}}
}

func wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func check(t *testing.T, path string, src []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := qobuz.ReadHead(bytes.NewReader(src))
	if !bytes.HasSuffix(got, src[h.Len:]) {
		t.Fatalf("frames differ: %d bytes, source %d", len(got), len(src))
	}
	info, err := qobuz.ReadFLAC(bytes.NewReader(got))
	if err != nil || info.Tags["TITLE"] == "" || info.Tags["ALBUM"] != "Record" || !info.Picture || info.Rate != 96000 {
		t.Fatalf("%v %+v", err, info)
	}
}

func TestDownloadTagged(t *testing.T) {
	f, d, c, _ := setup(t)
	name := cache.Name(cache.AlbumPrefix(1, 1, 1, 9), "Song 7", "7")
	d.Queue(Job{Ctx: "album-al1", Name: name, Track: track("7")})
	d.Start()
	wait(t, "the download", func() bool { return len(c.Files("album-al1")) == 1 })
	check(t, c.Files("album-al1")["7"], f.file)
	if left, _ := os.ReadDir(filepath.Join(d.o.Dir, "partial")); len(left) != 0 {
		t.Errorf("partial left: %v", left)
	}
	// The same track for a playlist is copied, not fetched again.
	before := len(f.ranges)
	d.Queue(Job{Ctx: "playlist-9", Name: cache.Name(cache.PlaylistPrefix(3), "Song 7", "7"), Track: track("7")})
	wait(t, "the copy", func() bool { return len(c.Files("playlist-9")) == 1 })
	if len(f.ranges) != before {
		t.Error("fetched again")
	}
}

// Cancelling a track's download into one folder leaves the .part another folder's download of the
// same track is writing.
func TestCancelSharedTrack(t *testing.T) {
	f, d, c, _ := setup(t)
	f.slow.Store(true)
	d.Queue(Job{Ctx: "album-al1", Name: cache.Name("01", "Song 7", "7"), Track: track("7")},
		Job{Ctx: "playlist-9", Name: cache.Name(cache.PlaylistPrefix(1), "Song 7", "7"), Track: track("7")})
	d.Start()
	wait(t, "progress", func() bool { j := d.Job("album-al1", "7"); return j != nil && j.Done >= chunk })
	if !d.Cancel("playlist-9", "7") {
		t.Fatal("not cancelled")
	}
	wait(t, "the album's track", func() bool { return len(c.Files("album-al1")) == 1 })
	check(t, c.Files("album-al1")["7"], f.file)
}

func TestDownloadResumes(t *testing.T) {
	f, d, c, _ := setup(t)
	f.cut.Store(true)
	d.Queue(Job{Ctx: "liked", Name: cache.Name(cache.LikedPrefix(1700000000), "Song 3", "3"), Track: track("3")})
	d.Start()
	wait(t, "the resumed download", func() bool { return len(c.Files("liked")) == 1 })
	check(t, c.Files("liked")["3"], f.file)
	if len(f.ranges) != 2 || f.ranges[0] != "" || f.ranges[1] == "" {
		t.Fatalf("ranges %q", f.ranges)
	}
	if f.covers.Load() != 1 {
		t.Errorf("cover fetched %d times", f.covers.Load())
	}
}

func TestSignedOutWaits(t *testing.T) {
	f, d, c, auths := setup(t)
	f.auth.Store(true)
	d.Queue(Job{Ctx: "album-al1", Name: cache.Name("01", "Song 1", "1"), Track: track("1")})
	d.Start()
	wait(t, "the sign-in callback", func() bool { return auths.Load() > 0 })
	if j := d.Job("album-al1", "1"); j == nil || j.State != Waiting || j.Err != "" {
		t.Fatalf("%+v", j)
	}
	f.auth.Store(false)
	d.mu.Lock()
	d.jobs[0].notBefore = time.Time{}
	d.mu.Unlock()
	d.Wake()
	wait(t, "the download after signing in", func() bool { return len(c.Files("album-al1")) == 1 })
}

func TestQueueSurvivesRestart(t *testing.T) {
	_, d, _, _ := setup(t)
	d.Queue(Job{Ctx: "album-al1", Name: "01 A [1].flac", Track: track("1")}, Job{Ctx: "album-al1", Name: "02 B [2].flac", Track: track("2")})
	d2 := New(d.o)
	if jobs := d2.Jobs(); len(jobs) != 2 || jobs[1].Track.Album.Title != "Record" {
		t.Fatalf("%+v", jobs)
	}
}

// An MP3 (format 5) is saved as .mp3 with our ID3 tag in place of its own, its frames as sent, and
// resumes after a break as a FLAC does.
func TestMP3(t *testing.T) {
	f, d, c, _ := setup(t)
	d.o.Quality = func() int { return qobuz.MP3 }
	f.cut.Store(true)
	d.Queue(Job{Ctx: "album-al1", Name: cache.Name("01", "Song 4", "4"), Track: track("4")})
	d.Start()
	wait(t, "the MP3", func() bool { return len(c.Files("album-al1")) == 1 })
	p := c.Files("album-al1")["4"]
	if !strings.HasSuffix(p, "01 Song 4 [4].mp3") {
		t.Fatal(p)
	}
	got, _ := os.ReadFile(p)
	tags, pic, err := qobuz.ReadID3(bytes.NewReader(got))
	if err != nil || tags["TIT2"] != "Song 4" || tags["TALB"] != "Record" || !pic {
		t.Fatalf("%v %v %v", err, tags, pic)
	}
	if !bytes.HasSuffix(got, f.mp3[15:]) || bytes.Contains(got, []byte("XXXXX")) {
		t.Fatal("frames changed, or Qobuz's own tag kept")
	}
	if len(f.ranges) != 2 || f.ranges[1] == "" {
		t.Fatalf("ranges %q", f.ranges)
	}
	// Copied into another folder, it is still an MP3; fetched as FLAC, a name ending .mp3 is not kept.
	d.Queue(Job{Ctx: "playlist-9", Name: cache.Name(cache.PlaylistPrefix(3), "Song 4", "4"), Track: track("4")})
	wait(t, "the copy", func() bool { return len(c.Files("playlist-9")) == 1 })
	if p := c.Files("playlist-9")["4"]; !strings.HasSuffix(p, ".mp3") {
		t.Error("copy named", p)
	}
	dir := t.TempDir()
	flacAt, err := d.Fetch(context.Background(), track("5"), qobuz.CD, filepath.Join(dir, ".5.part"), filepath.Join(dir, "001 Song 5 [5].mp3"), func(int64, int64) {})
	if err != nil || filepath.Base(flacAt) != "001 Song 5 [5].flac" {
		t.Error(err, flacAt)
	}
}

// A .part already whole, its rename missed, can't be resumed (HTTP 416): it starts again.
func TestWholePartStartsAgain(t *testing.T) {
	f, d, c, _ := setup(t)
	tmp := t.TempDir()
	whole, err := d.Fetch(context.Background(), track("8"), qobuz.HiRes96, filepath.Join(tmp, ".8.part"), filepath.Join(tmp, "8.flac"), func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := qobuz.ReadHead(bytes.NewReader(f.file))
	st, _ := os.Stat(whole)
	os.Rename(whole, d.partPath("8"))
	sb, _ := json.Marshal(sidecar{Src: h.Len, Out: st.Size() - (int64(len(f.file)) - h.Len), Format: 7})
	os.WriteFile(d.partPath("8")+".json", sb, 0o644)
	before := len(f.ranges)
	d.Queue(Job{Ctx: "album-al1", Name: cache.Name("08", "Song 8", "8"), Track: track("8")})
	d.Start()
	wait(t, "the download started again", func() bool { return len(c.Files("album-al1")) == 1 })
	check(t, c.Files("album-al1")["8"], f.file)
	if r := f.ranges[before:]; len(r) != 2 || r[0] != fmt.Sprintf("bytes=%d-", len(f.file)) || r[1] != "" {
		t.Fatalf("ranges %q", r)
	}
}
