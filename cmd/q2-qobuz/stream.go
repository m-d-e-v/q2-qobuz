package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"q2-qobuz/internal/cache"
	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/qobuz"
)

// Streaming, through Q2 Pod's stream mode for card apps (1.4.3, q2-pod's addon.c stream_poll): a
// session is a hidden card folder, .qobuz/stream/<seq>/, which this app fills with a list's tracks
// from the one pressed, named in play order ("001 Title [id].flac"), each whole before it appears
// (fetched through a hidden .part beside it). /tmp/q2-qobuz.play names it, "<seq> <folder>": Q2 Pod
// plays its first track as soon as it lands, as a press of a downloaded one does, and queues each
// later one as it comes, leaving the path of the one playing in /tmp/q2-qobuz.playing. The session
// keeps streamAhead tracks past that one and deletes those more than streamKeep behind it. A track
// downloaded to the Q2 is copied in, not fetched. While a track is fetched, downloads wait.

const (
	streamAhead = 2
	streamKeep  = 3
	streamMax   = 200              // tracks a session takes from its list
	streamIdle  = 45 * time.Minute // a session the Q2 hasn't played from this long (past the track playing's end) is over
	streamTries = 3
)

type stream struct {
	mu       sync.Mutex
	seq      int64
	dir      string
	label    string
	list     []entry
	names    []string // each track's file name in dir
	next     int      // the next to fetch
	playing  int      // the one the Q2 plays, -1 before it plays one
	fetching bool
	failed   int    // tracks left out: Qobuz wouldn't send them
	left     []bool // which, by index
	err      string
	seen     time.Time // the Q2 last played from it, or the session's start
	over     bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func (a *app) streamDir() string { return filepath.Join(a.opts.dir, "stream") }

// streamNow is the session's state for the pages: nil without one.
func (a *app) streamNow() *stream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stream
}

// streaming is whether a session still has work: tracks to fetch for a Q2 that is playing them.
func (a *app) streaming() bool {
	s := a.streamNow()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.over
}

// fetchingStream is whether a session is fetching a track now: downloads wait.
func (a *app) fetchingStream() bool {
	s := a.streamNow()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetching
}

// startStream plays es from index from: a new session, the one before ended and its folder deleted
// once the new one has its first track.
func (a *app) startStream(label string, es []entry, from int) {
	a.stopStream()
	if len(es)-from > streamMax {
		es = es[:from+streamMax]
	}
	seq := time.Now().UnixMilli()
	s := &stream{seq: seq, dir: filepath.Join(a.streamDir(), strconv.FormatInt(seq, 10)), label: label,
		list: es[from:], left: make([]bool, len(es)-from), playing: -1, seen: time.Now(), done: make(chan struct{})}
	mp3 := a.settings().StreamQuality == qobuz.MP3
	for i, e := range s.list {
		n := cache.Name(fmt.Sprintf("%03d", i+1), qobuz.FullTitle(e.T.Title, e.T.Version), e.T.ID.String())
		if mp3 {
			n = strings.TrimSuffix(n, ".flac") + ".mp3"
		}
		s.names = append(s.names, n)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	s.cancel = cancel
	a.mu.Lock()
	a.stream = s
	a.mu.Unlock()
	go a.runStream(ctx, s)
}

// stopStream ends the session's fetching, keeping its folder (Q2 Pod may still play from it).
func (a *app) stopStream() {
	a.mu.Lock()
	s := a.stream
	a.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

// playingPath is the path Q2 Pod last played from a session, "" when none.
func (a *app) playingPath() string {
	b, err := os.ReadFile(a.opts.playingFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// sweep deletes old sessions' folders, never the one playing now or keep.
func (a *app) sweep(keep string) {
	now := a.playingPath()
	ents, _ := os.ReadDir(a.streamDir())
	for _, e := range ents {
		p := filepath.Join(a.streamDir(), e.Name())
		if p == keep || (now != "" && strings.HasPrefix(now, p+"/")) {
			continue
		}
		os.RemoveAll(p)
	}
}

func (a *app) runStream(ctx context.Context, s *stream) {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		s.over, s.fetching = true, false
		s.mu.Unlock()
	}()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		s.mu.Lock()
		s.err = "can't write to the card"
		s.mu.Unlock()
		return
	}
	announced := false
	tries := 0
	for ctx.Err() == nil {
		s.mu.Lock()
		if p := a.playingPath(); strings.HasPrefix(p, s.dir+"/") {
			for i, n := range s.names {
				if filepath.Base(p) == n {
					if i != s.playing {
						s.seen = time.Now()
					}
					s.playing = i
				}
			}
		}
		target := s.ahead()
		next, idle := s.next, s.idle()
		s.mu.Unlock()

		if next < len(s.list) && next <= target {
			s.mu.Lock()
			s.fetching = true
			s.mu.Unlock()
			a.dl.Pause()
			err := a.streamOne(ctx, s, next)
			var ns notStreamed
			s.mu.Lock()
			s.fetching = false
			switch {
			case err == nil:
				s.next++
				s.err, tries = "", 0
			case ctx.Err() != nil:
			case tries+1 >= streamTries || errors.As(err, &ns):
				log.Printf("stream %s: %v; left out", s.list[next].T.ID, err)
				s.failed++
				s.left[next] = true
				s.next++
				s.err, tries = err.Error(), 0
			default:
				tries++
				s.err = err.Error()
			}
			s.mu.Unlock()
			if err == nil && !announced { // the first track is there: Q2 Pod plays it
				announced = true
				q2.WriteFile(a.opts.playFile, []byte(fmt.Sprintf("%d %s\n", s.seq, s.dir)))
				a.sweep(s.dir)
			}
			a.dl.Wake()
			if err != nil && ctx.Err() == nil {
				wait(ctx, time.Duration(tries)*3*time.Second)
			}
			continue
		}
		// Tracks well behind the one playing go.
		s.mu.Lock()
		for i := 0; i < s.playing-streamKeep; i++ {
			os.Remove(filepath.Join(s.dir, s.names[i]))
		}
		finished := s.next >= len(s.list)
		s.mu.Unlock()
		if finished || idle > streamIdle {
			return
		}
		wait(ctx, time.Second)
	}
}

// ahead is the last track to fetch: streamAhead past the one playing, tracks left out not counted;
// under s.mu.
func (s *stream) ahead() int {
	i := max(s.playing, 0)
	for n := 0; n < streamAhead && i < len(s.list)-1; {
		if i++; !s.left[i] {
			n++
		}
	}
	return i
}

// idle is how long the Q2 has gone without playing on in the session: since it started the track
// it plays, less that track's length (a long one is still playing); under s.mu.
func (s *stream) idle() time.Duration {
	d := time.Since(s.seen)
	if s.playing >= 0 {
		d -= time.Duration(s.list[s.playing].T.Duration) * time.Second
	}
	return d
}

func wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// notStreamed is a track Qobuz doesn't stream here, by its title: left out at once.
type notStreamed string

func (t notStreamed) Error() string { return "Qobuz doesn't stream " + string(t) }

// streamOne puts track i into the session's folder: copied when downloaded (as FLAC or MP3,
// whichever it is, though Qobuz no longer streams it), else fetched at the streaming quality. The
// name it lands under becomes the track's, its extension the file's.
func (a *app) streamOne(ctx context.Context, s *stream, i int) error {
	t := s.list[i].T
	s.mu.Lock()
	final := filepath.Join(s.dir, s.names[i])
	s.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	var err error
	if src, ok := a.cache.Anywhere(t.ID.String()); ok {
		final = cache.Stem(final) + filepath.Ext(src)
		err = cache.Copy(ctx, src, final)
	} else if !t.Streamable {
		return notStreamed(t.Title)
	} else {
		part := filepath.Join(s.dir, "."+t.ID.String()+".part")
		final, err = a.dl.Fetch(ctx, t, a.settings().StreamQuality, part, final, func(done, size int64) {})
	}
	if err == nil {
		s.mu.Lock()
		s.names[i] = filepath.Base(final)
		s.mu.Unlock()
	}
	return err
}

// streamRow is the menu's word on the session while it fetches or Q2 Pod plays from it: what it
// plays and how far it has got.
func (a *app) streamRow() (title, sub string, ok bool) {
	s := a.streamNow()
	if s == nil {
		return "", "", false
	}
	now := a.playingPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	playing := -1
	for i, n := range s.names {
		if now == filepath.Join(s.dir, n) {
			playing = i
		}
	}
	if s.over && playing < 0 && s.err == "" && s.failed == 0 {
		return "", "", false
	}
	title = "Streaming " + s.label
	switch {
	case s.err != "" && s.next == 0:
		sub = "Couldn't start: " + s.err
	case s.err != "":
		sub = "Trouble: " + s.err
	case playing >= 0:
		sub = fmt.Sprintf("Track %d of %d", playing+1, len(s.list))
	case s.next == 0:
		sub = "Starting…"
	default:
		sub = fmt.Sprintf("%d of %d ready", s.next, len(s.list))
	}
	if s.failed > 0 {
		sub = join(sub, plural(s.failed, "track")+" left out")
	}
	return title, sub, true
}
