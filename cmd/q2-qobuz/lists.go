package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/qobuz"
)

// The account's favourites and playlists are kept on the card (.qobuz/lists/<kind>.json) and shown
// at once; one older than listsStale is fetched again in the background, the Q2's page asking again
// ("refresh") until it lands. Albums, playlists' tracks, artists and searches are fetched when
// opened and kept a while in memory (loadsKeep).

const (
	pageLimit  = 500 // what Qobuz pages at most
	loadsKeep  = 10 * time.Minute
	listsRetry = 30 * time.Second
)

type favs struct {
	At        time.Time        `json:"at"`
	Albums    []qobuz.Album    `json:"albums,omitempty"`
	Tracks    []qobuz.Track    `json:"tracks,omitempty"`
	Artists   []qobuz.Artist   `json:"artists,omitempty"`
	Playlists []qobuz.Playlist `json:"playlists,omitempty"`
	busy      bool
	err       error
	tried     time.Time // the last fetch's start: a failed one is tried again after listsRetry
}

var kinds = []string{"albums", "tracks", "artists", "playlists"}

func (a *app) listPath(kind string) string { return filepath.Join(a.opts.dir, "lists", kind+".json") }

// listNow is a kind's list as kept, without fetching; nil when there is none yet.
func (a *app) listNow(kind string) *favs {
	a.mu.Lock()
	l := a.lists[kind]
	a.mu.Unlock()
	if l != nil {
		return l
	}
	var disk favs
	if !readJSON(a.listPath(kind), &disk) {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lists[kind] == nil {
		a.lists[kind] = &disk
	}
	return a.lists[kind]
}

// listOf is a kind's list, fetching it in the background when missing or stale (or force); busy
// while that runs. The list returned is a copy safe to read.
func (a *app) listOf(kind string, force bool) (l favs, busy bool, err error) {
	cur := a.listNow(kind)
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur == nil {
		cur = &favs{}
		a.lists[kind] = cur
	}
	due := force || cur.At.IsZero() || time.Since(cur.At) > listsStale
	if cur.err != nil && time.Since(cur.tried) < listsRetry {
		due = force
	}
	if !cur.busy && due && a.sess != nil {
		cur.busy, cur.tried = true, time.Now()
		go a.fetchList(kind)
	}
	return *cur, cur.busy, cur.err
}

// loading is whether any list is being fetched.
func (a *app) loading() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lists {
		if l.busy {
			return true
		}
	}
	return false
}

// fetchList reads every page of a kind's list and keeps it.
func (a *app) fetchList(kind string) {
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	fresh := favs{At: time.Now()}
	err := func() error {
		c, err := a.client(ctx)
		if err != nil {
			return err
		}
		for offset := 0; offset < 50000; {
			var n, total int
			switch kind {
			case "albums":
				p, err := c.FavoriteAlbums(ctx, pageLimit, offset)
				if err != nil {
					return err
				}
				fresh.Albums = append(fresh.Albums, p.Items...)
				n, total = len(p.Items), p.Total
			case "tracks":
				p, err := c.FavoriteTracks(ctx, pageLimit, offset)
				if err != nil {
					return err
				}
				fresh.Tracks = append(fresh.Tracks, p.Items...)
				n, total = len(p.Items), p.Total
			case "artists":
				p, err := c.FavoriteArtists(ctx, pageLimit, offset)
				if err != nil {
					return err
				}
				fresh.Artists = append(fresh.Artists, p.Items...)
				n, total = len(p.Items), p.Total
			case "playlists":
				p, err := c.Playlists(ctx, pageLimit, offset)
				if err != nil {
					return err
				}
				fresh.Playlists = append(fresh.Playlists, p.Items...)
				n, total = len(p.Items), p.Total
			}
			offset += n
			if n == 0 || offset >= total {
				if offset < total {
					return fmt.Errorf("Qobuz sent %d of %d", offset, total)
				}
				return nil
			}
		}
		return errors.New("the list didn't end")
	}()
	var qe *qobuz.Error
	if errors.As(err, &qe) && qe.Auth() {
		a.lostSignIn()
	}
	a.mu.Lock()
	cur := a.lists[kind]
	if cur == nil {
		cur = &favs{}
		a.lists[kind] = cur
	}
	cur.busy, cur.err = false, err
	if err == nil {
		fresh.tried = cur.tried
		*cur = fresh
	}
	a.mu.Unlock()
	if err != nil {
		log.Printf("list %s: %v", kind, err)
		return
	}
	os.MkdirAll(filepath.Dir(a.listPath(kind)), 0o755)
	b, _ := json.Marshal(fresh) // not indented: a big library's liked songs run to megabytes
	if err := q2.WriteFile(a.listPath(kind), b); err != nil {
		log.Printf("save list %s: %v", kind, err)
	}
}

// load is something fetched when opened (an album, a playlist's tracks, an artist, a search).
type load struct {
	done bool
	val  any
	err  error
	at   time.Time
}

// fetch starts fn for key once; done when it has ended. A failure is reported once and forgotten,
// so Retry fetches again.
func (a *app) fetch(key string, fn func(ctx context.Context, c *qobuz.Client) (any, error)) (any, error, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, l := range a.loads {
		if l.done && time.Since(l.at) > loadsKeep {
			delete(a.loads, k)
		}
	}
	l := a.loads[key]
	if l == nil {
		l = &load{at: time.Now()}
		a.loads[key] = l
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
			defer cancel()
			c, err := a.client(ctx)
			var v any
			if err == nil {
				v, err = fn(ctx, c)
			}
			var qe *qobuz.Error
			if errors.As(err, &qe) && qe.Auth() {
				a.lostSignIn()
			}
			a.mu.Lock()
			l.val, l.err, l.done, l.at = v, err, true, time.Now()
			a.mu.Unlock()
		}()
	}
	if l.done && l.err != nil {
		delete(a.loads, key)
	}
	return l.val, l.err, l.done
}

// forget drops a fetched thing, so it is fetched again.
func (a *app) forget(key string) {
	a.mu.Lock()
	delete(a.loads, key)
	a.mu.Unlock()
}

func (a *app) album(id string) (*qobuz.Album, error, bool) {
	v, err, done := a.fetch("album:"+id, func(ctx context.Context, c *qobuz.Client) (any, error) {
		al, err := c.Album(ctx, id)
		return &al, err
	})
	al, _ := v.(*qobuz.Album)
	return al, err, done
}

func (a *app) playlist(id string) (*qobuz.Playlist, error, bool) {
	v, err, done := a.fetch("playlist:"+id, func(ctx context.Context, c *qobuz.Client) (any, error) {
		var out *qobuz.Playlist
		for offset := 0; offset < 10000; {
			p, err := c.PlaylistTracks(ctx, id, pageLimit, offset)
			if err != nil {
				return nil, err
			}
			if out == nil {
				out = &p
				if out.Tracks == nil {
					out.Tracks = &qobuz.Page[qobuz.Track]{}
				}
			} else if p.Tracks != nil {
				out.Tracks.Items = append(out.Tracks.Items, p.Tracks.Items...)
			}
			if p.Tracks == nil || len(p.Tracks.Items) == 0 || len(out.Tracks.Items) >= p.Tracks.Total {
				break
			}
			offset = len(out.Tracks.Items)
		}
		return out, nil
	})
	p, _ := v.(*qobuz.Playlist)
	return p, err, done
}

type artistPage struct {
	Artist qobuz.Artist
	Albums []qobuz.Album
}

func (a *app) artist(id string) (*artistPage, error, bool) {
	v, err, done := a.fetch("artist:"+id, func(ctx context.Context, c *qobuz.Client) (any, error) {
		ar, albums, err := c.ArtistAlbums(ctx, id)
		return &artistPage{ar, albums}, err
	})
	p, _ := v.(*artistPage)
	return p, err, done
}

func (a *app) track(id string) (*qobuz.Track, error, bool) {
	v, err, done := a.fetch("track:"+id, func(ctx context.Context, c *qobuz.Client) (any, error) {
		t, err := c.Track(ctx, id)
		return &t, err
	})
	t, _ := v.(*qobuz.Track)
	return t, err, done
}

func (a *app) search(term string) (*qobuz.Found, error, bool) {
	v, err, done := a.fetch("search:"+term, func(ctx context.Context, c *qobuz.Client) (any, error) {
		f, err := c.Search(ctx, term, 30)
		return &f, err
	})
	f, _ := v.(*qobuz.Found)
	return f, err, done
}
