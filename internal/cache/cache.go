// Package cache keeps downloaded tracks on the card, under .qobuz/cache, one folder per list they
// play in, since Q2 Pod plays a card app's song as its folder, in name order, from that song:
//
//	album-<id>/     "1-03 Title [<track id>].flac"  disc and track, so the album plays in order
//	                (".mp3" for a track downloaded as MP3)
//	playlist-<id>/  "0007 Title [<track id>].flac"  its place in the playlist
//	liked/          "8270000000 Title [<id>].flac"  newest liked first, a name that never changes
//
// Each folder's .name holds what it is called on the Q2. A track's id is in its name, so the card
// itself says what is downloaded; a track another folder has is copied, not downloaded again. The
// folder holds at most Cap bytes: the oldest downloads go first.
package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Paths stay inside Q2 Pod's 256-byte songs_open buffer: /mnt/mmc/.qobuz/cache/ is 22 bytes, a
// folder at most 24 and a file at most MaxFile.
const MaxFile = 160

type Cache struct {
	Dir string
	mu  sync.Mutex
}

var fat = strings.NewReplacer(": ", " - ", ":", "-", "/", "-", "\\", "-", "|", "-", "\"", "'",
	"?", "", "*", "", "<", "", ">", "")

// Safe is s as a FAT name: the characters FAT refuses replaced, no leading or trailing dot or
// space, at most n bytes, never empty.
func Safe(s string, n int) string {
	s = fat.Replace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		for n > 0 && s[n]&0xc0 == 0x80 { // not inside a UTF-8 character
			n--
		}
		s = s[:n]
	}
	s = strings.Trim(s, ". ")
	if s == "" {
		s = "Untitled"
	}
	return s
}

// Name is a track's file name in a folder: its order prefix, its title and its id.
func Name(prefix, title, id string) string {
	room := MaxFile - len(prefix) - len(id) - len(" [].flac") - 1
	return prefix + " " + Safe(title, room) + " [" + id + "].flac"
}

// AlbumPrefix orders an album's tracks: "03", or "2-03" on an album of more than one disc, wide
// enough for its discs and tracks ("02-003" on a box of 12 discs, 140 tracks) to sort as numbers.
func AlbumPrefix(disc, track, discs, tracks int) string {
	tw := 2
	if tracks > 99 {
		tw = 3
	}
	if discs > 1 {
		return fmt.Sprintf("%0*d-%0*d", len(fmt.Sprint(discs)), max(disc, 1), tw, track)
	}
	return fmt.Sprintf("%0*d", tw, track)
}

// PlaylistPrefix orders a playlist's tracks by place, from 1.
func PlaylistPrefix(place int) string { return fmt.Sprintf("%04d", place) }

// LikedPrefix orders liked songs newest first, by when each was liked.
func LikedPrefix(likedAt int64) string { return fmt.Sprintf("%010d", 9999999999-likedAt) }

// TrackID is the id in a cached file's name, "" when it has none or is hidden (a download's .part,
// or the ._ file macOS leaves beside each file it touches on the card).
func TrackID(name string) string {
	if strings.HasPrefix(name, ".") {
		return ""
	}
	name = Stem(name)
	i := strings.LastIndex(name, " [")
	if i < 0 || !strings.HasSuffix(name, "]") {
		return ""
	}
	return name[i+2 : len(name)-1]
}

// Stem is a track's file name or path without its .flac or .mp3.
func Stem(name string) string { return strings.TrimSuffix(strings.TrimSuffix(name, ".flac"), ".mp3") }

func (c *Cache) Folder(ctx string) string { return filepath.Join(c.Dir, ctx) }

// Files is a folder's tracks, by id.
func (c *Cache) Files(ctx string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(c.Folder(ctx))
	for _, e := range ents {
		if id := TrackID(e.Name()); id != "" && e.Type().IsRegular() {
			out[id] = filepath.Join(c.Folder(ctx), e.Name())
		}
	}
	return out
}

// Anywhere is a cached copy of a track in any folder.
func (c *Cache) Anywhere(id string) (string, bool) {
	dirs, _ := os.ReadDir(c.Dir)
	for _, d := range dirs {
		if d.IsDir() {
			if p, ok := c.Files(d.Name())[id]; ok {
				return p, true
			}
		}
	}
	return "", false
}

// Label names a folder for the Q2's Downloaded list.
func (c *Cache) Label(ctx, name string) {
	os.MkdirAll(c.Folder(ctx), 0o755)
	p := filepath.Join(c.Folder(ctx), ".name")
	if b, err := os.ReadFile(p); err != nil || string(b) != name {
		os.WriteFile(p, []byte(name), 0o644)
	}
}

// Renumber renames a folder's files whose order prefix has changed (a playlist reordered): want
// is each track id's name now, as Name makes it; a file keeps its own extension (.mp3 or .flac).
func (c *Cache) Renumber(ctx string, want map[string]string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, p := range c.Files(ctx) {
		w, ok := want[id]
		w = Stem(w) + filepath.Ext(p)
		if ok && filepath.Base(p) != w {
			if os.Rename(p, filepath.Join(c.Folder(ctx), w)) == nil {
				n++
			}
		}
	}
	return n
}

// Context is a folder of downloads.
type Context struct {
	Name, Label string
	Files       int
	Bytes       int64
	Newest      time.Time
}

// Contexts is every folder with downloads, newest first.
func (c *Cache) Contexts() []Context {
	var out []Context
	dirs, _ := os.ReadDir(c.Dir)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		x := Context{Name: d.Name(), Label: d.Name()}
		if b, err := os.ReadFile(filepath.Join(c.Dir, d.Name(), ".name")); err == nil {
			x.Label = string(b)
		}
		ents, _ := os.ReadDir(filepath.Join(c.Dir, d.Name()))
		for _, e := range ents {
			if TrackID(e.Name()) == "" {
				continue
			}
			if fi, err := e.Info(); err == nil {
				x.Files++
				x.Bytes += fi.Size()
				if fi.ModTime().After(x.Newest) {
					x.Newest = fi.ModTime()
				}
			}
		}
		if x.Files > 0 {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Newest.After(out[j].Newest) })
	return out
}

// Usage is the downloads' bytes and count.
func (c *Cache) Usage() (int64, int) {
	var b int64
	n := 0
	for _, x := range c.Contexts() {
		b += x.Bytes
		n += x.Files
	}
	return b, n
}

// Trim removes the oldest downloads until the rest fit in limit bytes, never keep; how many.
func (c *Cache) Trim(limit int64, keep string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	type file struct {
		path string
		size int64
		at   time.Time
	}
	var all []file
	var total int64
	dirs, _ := os.ReadDir(c.Dir)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(c.Dir, d.Name()))
		for _, e := range ents {
			if TrackID(e.Name()) == "" {
				continue
			}
			if fi, err := e.Info(); err == nil {
				all = append(all, file{filepath.Join(c.Dir, d.Name(), e.Name()), fi.Size(), fi.ModTime()})
				total += fi.Size()
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	n := 0
	for _, f := range all {
		if total <= limit {
			break
		}
		if f.path == keep {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			n++
			c.tidy(filepath.Dir(f.path))
		}
	}
	return n
}

// tidy removes a folder left with no tracks, and its .name.
func (c *Cache) tidy(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if TrackID(e.Name()) != "" {
			return
		}
	}
	os.RemoveAll(dir)
}

// Remove deletes a track from a folder.
func (c *Cache) Remove(ctx, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.Files(ctx)[id]
	if !ok {
		return os.ErrNotExist
	}
	err := os.Remove(p)
	c.tidy(c.Folder(ctx))
	return err
}

// RemoveContext deletes a folder's downloads; how many.
func (c *Cache) RemoveContext(ctx string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.Files(ctx))
	os.RemoveAll(c.Folder(ctx))
	return n
}

// Copy puts a copy of src, a cached track, at dst, written aside and renamed; ctx's end stops it
// and leaves nothing behind.
func Copy(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, ctxReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
