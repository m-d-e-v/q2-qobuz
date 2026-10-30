// Package dl downloads tracks into the cache one at a time, in the order asked, each tagged as it
// arrives (Qobuz's files carry no tags) and resumed after a break.
//
// A download is written to partial/<id>.part: the new start Build makes, then the source's audio
// frames as they come. partial/<id>.json keeps where the two starts end (Src in Qobuz's file, Out in
// ours), so a break resumes with a Range at Src + (the .part's length - Out). Each attempt asks
// Qobuz for a fresh address, as they expire. A finished file is renamed into its folder and the
// cache trimmed to its limit.
package dl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"q2-qobuz/internal/cache"
	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/qobuz"
)

const (
	chunk    = 512 << 10        // written to the card at a time, each opened, synced and closed
	stallFor = 60 * time.Second // no bytes this long: the transfer is given up and tried again
	reserve  = 64 << 20         // the card keeps this much free
	tries    = 4                // a download that fails this often (but for the network) is failed
	idleWait = 30 * time.Second
)

// RetryAfter is the wait before a failed download's second try, doubling each time.
var RetryAfter = 10 * time.Second

// Source is Qobuz, as the downloader needs it.
type Source interface {
	// File is a fresh address for a track's audio at a quality (qobuz.CD…HiRes192), or the best below.
	File(ctx context.Context, trackID string, quality int) (*qobuz.File, error)
	// Cover is an album's front cover as JPEG, or nil.
	Cover(ctx context.Context, a *qobuz.Album) []byte
}

type State int

const (
	Waiting State = iota
	Active
	Failed
)

// Job is a track to download into a folder under a name.
type Job struct {
	Ctx   string      `json:"ctx"`
	Name  string      `json:"name"`
	Track qobuz.Track `json:"track"` // with its album, for its tags
	State State       `json:"-"`
	Err   string      `json:"err,omitempty"`
	Done  int64       `json:"-"`
	Size  int64       `json:"-"` // -1 unknown
	Fail  bool        `json:"failed,omitempty"`

	tries     int
	notBefore time.Time
	discard   bool
	paused    bool // stopped for a stream, not by a failure
}

func (j *Job) key() string { return j.Ctx + ":" + j.Track.ID.String() }

type Options struct {
	Dir     string // partial/ and queue.json go here
	Cache   *cache.Cache
	Source  Source
	Fetch   *http.Client // no total timeout: stalls are watched instead
	Ready   func() bool  // Wi-Fi up, the card there and nothing streaming: downloads may run
	Quality func() int   // downloads' quality
	Limit   func() int64 // the cache's limit in bytes
	OnAuth  func()       // Qobuz no longer takes the sign-in
}

type Downloader struct {
	o       Options
	mu      sync.Mutex
	jobs    []*Job
	cancel  context.CancelFunc
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	started bool
}

func New(o Options) *Downloader {
	os.MkdirAll(filepath.Join(o.Dir, "partial"), 0o755)
	d := &Downloader{o: o, wake: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
	if b, err := os.ReadFile(d.queuePath()); err == nil {
		json.Unmarshal(b, &d.jobs)
	}
	for _, j := range d.jobs {
		if j.Fail {
			j.State = Failed
		}
	}
	return d
}

func (d *Downloader) queuePath() string { return filepath.Join(d.o.Dir, "queue.json") }
func (d *Downloader) partPath(id string) string {
	return filepath.Join(d.o.Dir, "partial", id+".part")
}

// save writes the queue; under d.mu.
func (d *Downloader) save() {
	var keep []*Job
	for _, j := range d.jobs {
		if !j.discard {
			j.Fail = j.State == Failed
			keep = append(keep, j)
		}
	}
	b, _ := json.Marshal(keep)
	if err := q2.WriteFile(d.queuePath(), b); err != nil {
		log.Printf("save queue: %v", err)
	}
}

func (d *Downloader) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Downloader) find(key string) (int, *Job) {
	for i, j := range d.jobs {
		if j.key() == key {
			return i, j
		}
	}
	return -1, nil
}

// Queue asks for jobs, in order; ones already queued stay where they are, failed ones are tried
// again. How many were added.
func (d *Downloader) Queue(jobs ...Job) int {
	on := map[string]map[string]string{} // each folder's files, read once, before the lock
	for _, j := range jobs {
		if on[j.Ctx] == nil {
			on[j.Ctx] = d.o.Cache.Files(j.Ctx)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, j := range jobs {
		if _, have := on[j.Ctx][j.Track.ID.String()]; have {
			continue
		}
		if _, old := d.find(j.key()); old != nil {
			if old.State == Failed {
				old.State, old.Err, old.tries, old.notBefore = Waiting, "", 0, time.Time{}
				n++
			}
			continue
		}
		j := j
		j.State, j.Size = Waiting, -1
		d.jobs = append(d.jobs, &j)
		n++
	}
	if n > 0 {
		d.save()
		d.poke()
	}
	return n
}

// drop removes job i; under d.mu.
func (d *Downloader) drop(i int) {
	j := d.jobs[i]
	if j.State == Active {
		j.discard = true
		if d.cancel != nil {
			d.cancel()
		}
		return
	}
	d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
	d.removePart(j.Track.ID.String())
}

// removePart deletes a track's .part, unless another folder's job for the track still wants it
// (an album and a playlist that share it); under d.mu.
func (d *Downloader) removePart(id string) {
	for _, j := range d.jobs {
		if j.Track.ID.String() == id && !j.discard {
			return
		}
	}
	os.Remove(d.partPath(id))
	os.Remove(d.partPath(id) + ".json")
}

// Cancel drops a track's download from a folder.
func (d *Downloader) Cancel(ctx, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, j := d.find(ctx + ":" + id)
	if j == nil {
		return false
	}
	d.drop(i)
	d.save()
	return true
}

// CancelContext drops a folder's downloads; how many.
func (d *Downloader) CancelContext(ctx string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for i := len(d.jobs) - 1; i >= 0; i-- {
		if d.jobs[i].Ctx == ctx || ctx == "" {
			d.drop(i)
			n++
		}
	}
	d.save()
	return n
}

// RetryFailed tries every failed download again; how many.
func (d *Downloader) RetryFailed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, j := range d.jobs {
		if j.State == Failed {
			j.State, j.Err, j.tries, j.notBefore = Waiting, "", 0, time.Time{}
			n++
		}
	}
	if n > 0 {
		d.save()
		d.poke()
	}
	return n
}

// Jobs is the queue, in order.
func (d *Downloader) Jobs() []Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Job
	for _, j := range d.jobs {
		if !j.discard {
			out = append(out, *j)
		}
	}
	return out
}

// Job is a track's download into a folder, nil when there is none.
func (d *Downloader) Job(ctx, id string) *Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, j := d.find(ctx + ":" + id); j != nil && !j.discard {
		c := *j
		return &c
	}
	return nil
}

// Busy is whether bytes are moving; Pending whether a download could run.
func (d *Downloader) Busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State == Active {
			return true
		}
	}
	return false
}

func (d *Downloader) Pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State != Failed {
			return true
		}
	}
	return false
}

// Start runs the queue until Stop; Wake asks it to look again (after a sign-in).
func (d *Downloader) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		d.started = true
		go d.run()
	}
}

func (d *Downloader) Wake() { d.poke() }

// Stop ends the download under way, keeping its .part to resume, and waits for it.
func (d *Downloader) Stop() {
	d.mu.Lock()
	started := d.started
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	if started {
		<-d.stopped
	}
}

func (d *Downloader) next() (*Job, context.Context, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.stop: // Stop came after run looked: nothing starts that it won't cancel
		return nil, nil, idleWait
	default:
	}
	now, wait := time.Now(), idleWait
	for _, j := range d.jobs {
		if j.State != Waiting {
			continue
		}
		if due := j.notBefore.Sub(now); due > 0 {
			wait = min(wait, due)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.State, j.Done = Active, 0
		d.cancel = cancel
		return j, ctx, 0
	}
	return nil, nil, wait
}

func (d *Downloader) run() {
	defer close(d.stopped)
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		var j *Job
		var ctx context.Context
		wait := idleWait
		if d.o.Ready() {
			j, ctx, wait = d.next()
		}
		if j == nil {
			select {
			case <-d.stop:
				return
			case <-d.wake:
			case <-time.After(wait):
			}
			continue
		}
		err := d.download(ctx, j, func(done, size int64) {
			d.mu.Lock()
			j.Done, j.Size = done, size
			d.mu.Unlock()
		})
		if errors.Is(err, errPaused) { // a stream took the connection: this one waits its turn
			err = nil
			d.mu.Lock()
			j.State, j.notBefore = Waiting, time.Now().Add(2*time.Second)
			d.cancel = nil
			d.mu.Unlock()
			continue
		}
		d.finish(j, err)
	}
}

// permanent is a failure trying again won't mend; signedOut one only a sign-in mends.
type permanent struct{ error }
type signedOut struct{ error }

func (d *Downloader) finish(j *Job, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancel = nil
	i, _ := d.find(j.key())
	stopping := false
	select {
	case <-d.stop:
		stopping = true
	default:
	}
	var so signedOut
	switch {
	case j.discard:
		if i >= 0 {
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
		}
		d.removePart(j.Track.ID.String())
	case err == nil:
		if i >= 0 {
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
		}
		log.Printf("downloaded %s into %s", j.Track.ID, j.Ctx)
	case stopping:
		j.State = Waiting
	case errors.As(err, &so):
		j.State, j.Err, j.notBefore = Waiting, "", time.Now().Add(idleWait)
		if d.o.OnAuth != nil {
			go d.o.OnAuth()
		}
	case errors.Is(err, q2.ErrNoCard):
		j.State, j.Err, j.notBefore = Waiting, "", time.Now().Add(idleWait)
	default:
		var p permanent
		if j.tries++; errors.As(err, &p) || j.tries >= tries {
			j.State, j.Err = Failed, err.Error()
		} else {
			j.State, j.Err = Waiting, err.Error()
			j.notBefore = time.Now().Add(RetryAfter << (j.tries - 1))
		}
		log.Printf("download %s: %v (try %d)", j.Track.ID, err, j.tries)
	}
	d.save()
}

func freeBytes(path string) (int64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, false
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), true
}

// sidecar is partial/<id>.json: where Qobuz's start and ours end, and the format downloading.
type sidecar struct {
	Src, Out int64
	Format   int
}

func rangeStart(h string) int64 {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "bytes"))
	if i := strings.IndexByte(h, '-'); i > 0 {
		if v, err := strconv.ParseInt(strings.TrimSpace(h[:i]), 10, 64); err == nil {
			return v
		}
	}
	return -1
}

var errPaused = errors.New("paused for a stream")

// Pause stops the download under way, if any, keeping its .part; it is tried again once Ready.
func (d *Downloader) Pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State == Active && d.cancel != nil {
			j.paused = true
			d.cancel()
		}
	}
}

// download fetches one track into its folder.
func (d *Downloader) download(ctx context.Context, j *Job, progress func(done, size int64)) error {
	id := j.Track.ID.String()
	final := filepath.Join(d.o.Cache.Folder(j.Ctx), j.Name)
	if _, have := d.o.Cache.Files(j.Ctx)[id]; have {
		return nil
	}
	var err error
	if src, ok := d.o.Cache.Anywhere(id); ok { // another list has it, as FLAC or MP3
		final = cache.Stem(final) + filepath.Ext(src)
		err = cache.Copy(ctx, src, final)
	} else {
		final, err = d.Fetch(ctx, j.Track, d.o.Quality(), d.partPath(id), final, progress)
	}
	d.mu.Lock()
	paused := j.paused
	j.paused = false
	d.mu.Unlock()
	if paused && err != nil {
		return errPaused
	}
	if err == nil {
		if lim := d.o.Limit(); lim > 0 {
			d.o.Cache.Trim(lim, final)
		}
	}
	return err
}

// Fetch downloads a track at quality to final, tagged, through part (and part+".json", where its
// two starts end): a break resumes it. final's extension becomes the file's (.flac or .mp3); the
// path written is returned. Streaming uses it too, with its own part and final.
func (d *Downloader) Fetch(ctx context.Context, t qobuz.Track, quality int, part, final string,
	progress func(done, size int64)) (string, error) {
	err := d.fetch(ctx, t, quality, part, &final, progress)
	return final, err
}

// MP3 is whether a file Qobuz sends is an MP3 (format MP3), by its MIME type.
func MP3(f *qobuz.File) bool { return strings.Contains(f.MimeType, "mpeg") || f.FormatID == qobuz.MP3 }

func (d *Downloader) fetch(ctx context.Context, t qobuz.Track, quality int, part string, final *string,
	progress func(done, size int64)) error {
	id := t.ID.String()
	f, err := d.o.Source.File(ctx, id, quality)
	var qe *qobuz.Error
	switch {
	case errors.As(err, &qe) && qe.Auth():
		return signedOut{errors.New("Qobuz needs you to sign in again")}
	case errors.As(err, &qe) && qe.Status >= 400 && qe.Status < 500 && qe.Status != 429:
		return permanent{err} // not streamable here, or gone
	case errors.As(err, &qe):
		return err // busy or failing: tried again
	case err != nil:
		return err // the network, or the web player unread: tried again
	case f.Sample:
		return permanent{errors.New("your Qobuz plan doesn't stream this track")}
	}
	mp3 := MP3(f)
	ext := ".flac"
	if mp3 {
		ext = ".mp3"
	}
	*final = cache.Stem(*final) + ext
	var side sidecar
	var have int64
	if b, err := os.ReadFile(part + ".json"); err == nil && json.Unmarshal(b, &side) == nil && side.Format == f.FormatID {
		if st, err := os.Stat(part); err == nil && st.Size() >= side.Out {
			have = st.Size()
		}
	}
	if have == 0 {
		os.Remove(part)
		os.Remove(part + ".json")
		side = sidecar{Format: f.FormatID}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return permanent{errors.New("Qobuz gave a broken address")}
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", side.Src+have-side.Out))
	}
	resp, err := d.o.Fetch.Do(req)
	if err != nil {
		return errors.New("can't reach Qobuz's servers: check the Wi-Fi")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if rangeStart(resp.Header.Get("Content-Range")) != side.Src+have-side.Out {
			os.Remove(part)
			os.Remove(part + ".json")
			return errors.New("the server resumed at the wrong place")
		}
	case http.StatusOK:
		if have > 0 {
			os.Remove(part)
			os.Remove(part + ".json")
			have, side = 0, sidecar{Format: f.FormatID}
		}
	case http.StatusRequestedRangeNotSatisfiable: // a whole .part that missed its rename: started again
		os.Remove(part)
		os.Remove(part + ".json")
		return errors.New("Qobuz's file server couldn't resume it: starting again")
	case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
		return fmt.Errorf("Qobuz's file server refused (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("Qobuz's file server failed (HTTP %d)", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	stall := time.AfterFunc(stallFor, func() { stalled.Store(true); cancel() })
	defer stall.Stop()
	go func() { <-ctx.Done(); resp.Body.Close() }()
	body := bufio.NewReaderSize(resp.Body, 64<<10)

	off := have
	if have == 0 { // the start, read and written anew
		var hdr []byte
		var srcLen int64
		if mp3 {
			n, err := qobuz.SkipID3(body)
			if err != nil {
				if stalled.Load() || ctx.Err() != nil {
					return errors.New("the download stalled")
				}
				return fmt.Errorf("Qobuz's file isn't MP3: %v", err)
			}
			hdr, srcLen = qobuz.ID3(qobuz.Tags(t, nil), d.o.Source.Cover(ctx, t.Album)), n
		} else {
			head, err := qobuz.ReadHead(body)
			if err != nil {
				if stalled.Load() || ctx.Err() != nil {
					return errors.New("the download stalled")
				}
				return fmt.Errorf("Qobuz's file isn't FLAC: %v", err)
			}
			hdr, srcLen = head.Build(qobuz.Tags(t, nil), d.o.Source.Cover(ctx, t.Album)), head.Len
		}
		if err := q2.Append(part, 0, hdr); err != nil {
			return fmt.Errorf("can't write to the card: %v", err)
		}
		side.Src, side.Out = srcLen, int64(len(hdr))
		sb, _ := json.Marshal(side)
		if err := os.WriteFile(part+".json", sb, 0o644); err != nil {
			return fmt.Errorf("can't write to the card: %v", err)
		}
		off = side.Out
	}
	size := int64(-1) // the finished file's
	if resp.ContentLength >= 0 {
		if have > 0 {
			size = have + resp.ContentLength
		} else {
			size = side.Out + resp.ContentLength - side.Src
		}
	}
	if free, ok := freeBytes(d.o.Dir); ok && size > 0 && free < size-off+reserve {
		return permanent{errors.New("the card is full")}
	}
	progress(off, size)

	buf := make([]byte, chunk)
	n := 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		if err := q2.Append(part, off, buf[:n]); err != nil {
			if errors.Is(err, q2.ErrNoCard) {
				return err
			}
			return fmt.Errorf("can't write to the card: %v", err)
		}
		off += int64(n)
		n = 0
		progress(off, size)
		return nil
	}
	for {
		k, rerr := body.Read(buf[n:])
		if k > 0 {
			stall.Reset(stallFor)
			if n += k; n == len(buf) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if err := flush(); err != nil {
				return err
			}
			if stalled.Load() {
				return errors.New("the download stalled")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("the download was cut off")
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if size >= 0 && off != size {
		return errors.New("the download was cut off")
	}
	if err := os.MkdirAll(filepath.Dir(*final), 0o755); err != nil {
		return fmt.Errorf("can't make the folder: %v", err)
	}
	if err := os.Rename(part, *final); err != nil {
		return fmt.Errorf("can't move the track into place: %v", err)
	}
	os.Remove(part + ".json")
	return nil
}
