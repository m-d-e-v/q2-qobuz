package cache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNames(t *testing.T) {
	if n := Name(AlbumPrefix(2, 3, 2, 20), "A: B/C?", "123"); n != "2-03 A - B-C [123].flac" || TrackID(n) != "123" {
		t.Fatal(n)
	}
	if n := Name(AlbumPrefix(1, 7, 1, 9), strings.Repeat("é", 200), "9"); len(n) > MaxFile || TrackID(n) != "9" {
		t.Fatal(len(n), n)
	}
	// A box of 12 discs and 140 tracks sorts by name as it plays.
	if a, b := AlbumPrefix(2, 105, 12, 140), AlbumPrefix(10, 1, 12, 140); a != "02-105" || b != "10-001" || a > b {
		t.Fatal(a, b)
	}
	if LikedPrefix(1700000001) >= LikedPrefix(1700000000) {
		t.Error("newer liked must sort first")
	}
	if TrackID("cover.jpg") != "" || TrackID(".name") != "" || TrackID("._02 Gallipoli [55798354].flac") != "" {
		t.Error("not a track")
	}
}

func TestTrimAndContexts(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	put := func(ctx, name string, size int, age time.Duration) string {
		p := filepath.Join(c.Folder(ctx), name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, size), 0o644)
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
		return p
	}
	c.Label("album-1", "Record")
	old := put("album-1", "01 A [1].flac", 100, 3*time.Hour)
	put("album-1", "02 B [2].flac", 100, 2*time.Hour)
	keep := put("playlist-5", "0001 C [3].flac", 100, 4*time.Hour)
	if ctxs := c.Contexts(); len(ctxs) != 2 || ctxs[0].Label != "Record" || ctxs[0].Files != 2 {
		t.Fatalf("%+v", ctxs)
	}
	if n := c.Trim(150, keep); n != 2 {
		t.Fatalf("trimmed %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("oldest kept")
	}
	if _, err := os.Stat(c.Folder("album-1")); !os.IsNotExist(err) {
		t.Error("empty folder kept")
	}
	if _, ok := c.Anywhere("3"); !ok {
		t.Error("kept file gone")
	}
	if c.Renumber("playlist-5", map[string]string{"3": "0002 C [3].flac"}) != 1 || c.Files("playlist-5")["3"] != filepath.Join(c.Folder("playlist-5"), "0002 C [3].flac") {
		t.Error("renumber")
	}
	put("playlist-5", "0001 D [4].mp3", 100, time.Hour) // an MP3 stays one
	if c.Renumber("playlist-5", map[string]string{"4": "0003 D [4].flac"}) != 1 || c.Files("playlist-5")["4"] != filepath.Join(c.Folder("playlist-5"), "0003 D [4].mp3") {
		t.Error("renumber an MP3:", c.Files("playlist-5"))
	}
}

func TestCopyCancelled(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.flac"), filepath.Join(dir, "b", "a.flac")
	os.WriteFile(src, make([]byte, 1<<20), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Copy(ctx, src, dst); err == nil {
		t.Fatal("copied after the cancel")
	}
	if left, _ := os.ReadDir(filepath.Dir(dst)); len(left) != 0 {
		t.Fatal("left behind:", left)
	}
	if err := Copy(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
}
