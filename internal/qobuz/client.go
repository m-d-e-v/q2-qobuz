package qobuz

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// API is the web player's API; tests point it elsewhere.
var API = "https://www.qobuz.com/api.json/0.2/"

// SignIn is Qobuz's sign-in page for other apps.
var SignIn = "https://www.qobuz.com/signin/oauth"

// Formats a file can be asked for, by id. A track without a master that good comes in the best it
// has; FileURL's answer says what was given.
const (
	MP3      = 5  // MP3 320 kbit/s
	CD       = 6  // FLAC 16-bit 44.1 kHz
	HiRes96  = 7  // FLAC 24-bit up to 96 kHz
	HiRes192 = 27 // FLAC 24-bit up to 192 kHz
)

type Client struct {
	HTTP   *http.Client
	Creds  *Creds
	Secret string // the one of Creds.Secrets that signs this account's requests (FindSecret)
	Token  string // user_auth_token
	UserID int64
}

// Error is an API failure: its HTTP status and Qobuz's message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("Qobuz: %s (HTTP %d)", e.Message, e.Status) }

// Auth is whether the account's sign-in is no longer accepted; anything else (the network, 5xx,
// 429, a region or plan refusal) keeps the token.
func (e *Error) Auth() bool { return e.Status == http.StatusUnauthorized }

// SignInURL is the page to send someone to; it returns them to redirect with code_autorisation.
func SignInURL(appID, redirect string) string {
	return SignIn + "?" + url.Values{"ext_app_id": {appID}, "redirect_url": {redirect}}.Encode()
}

func (c *Client) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, API+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-App-Id", c.Creds.AppID)
	if c.Token != "" {
		req.Header.Set("X-User-Auth-Token", c.Token)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Q2 Pod)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Qobuz: %w", bare(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("Qobuz's %s answer: %w", endpoint, err)
	}
	return nil
}

// bare is a transport failure without its request's address, whose query can carry a sign-in's
// code and the private key.
func bare(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Exchange trades a sign-in's code for the account's token.
func (c *Client) Exchange(ctx context.Context, code string) error {
	if c.Creds.PrivateKey == "" {
		return errors.New("the web player's script had no private key for sign-ins")
	}
	var out struct {
		Token    string          `json:"token"`
		UserAuth string          `json:"user_auth_token"`
		UserID   json.RawMessage `json:"user_id"`
	}
	if err := c.get(ctx, "oauth/callback", url.Values{"code": {code}, "private_key": {c.Creds.PrivateKey}}, &out); err != nil {
		return err
	}
	c.Token = out.Token
	if c.Token == "" {
		c.Token = out.UserAuth
	}
	id, err := strconv.ParseInt(strings.Trim(string(out.UserID), `"`), 10, 64)
	if c.Token == "" || err != nil {
		return errors.New("Qobuz's sign-in answer had no token or user")
	}
	c.UserID = id
	return nil
}

// Sign is a file request's signature: MD5 of the method's name, its arguments' names and values
// in name order, the time and the secret.
func Sign(object, method string, args map[string]string, ts, secret string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(object + method)
	for _, k := range keys {
		b.WriteString(k + args[k])
	}
	b.WriteString(ts + secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// File is where a track's audio is, for a while, and what it is.
type File struct {
	URL          string  `json:"url"`
	FormatID     int     `json:"format_id"`
	MimeType     string  `json:"mime_type"`
	SamplingRate float64 `json:"sampling_rate"` // kHz
	BitDepth     int     `json:"bit_depth"`
	Sample       bool    `json:"sample"` // only 30 s: the plan doesn't stream this
	Restrictions []struct {
		Code string `json:"code"`
	} `json:"restrictions"`
}

// ErrNoFile is a file request Qobuz answered without a file (a rights holder's restriction): the
// signature was taken, and asking again won't mend it.
var ErrNoFile = &Error{Status: http.StatusNotFound, Message: "no file for this track"}

// FileURL asks for a track's file in format (or the best below it).
func (c *Client) FileURL(ctx context.Context, trackID string, format int, secret string) (*File, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	args := map[string]string{"format_id": strconv.Itoa(format), "intent": "stream", "track_id": trackID}
	params := url.Values{"request_ts": {ts}, "request_sig": {Sign("track", "getFileUrl", args, ts, secret)}}
	for k, v := range args {
		params.Set(k, v)
	}
	var f File
	if err := c.get(ctx, "track/getFileUrl", params, &f); err != nil {
		return nil, err
	}
	if f.URL == "" {
		return nil, ErrNoFile
	}
	return &f, nil
}

// FindSecret tries each candidate secret on a file request for trackID; the first Qobuz accepts
// (a file, or ErrNoFile for a track it won't send) signs this account's requests.
func (c *Client) FindSecret(ctx context.Context, trackID string) (string, error) {
	var last error
	zones := make([]string, 0, len(c.Creds.Secrets))
	for z := range c.Creds.Secrets {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	for _, z := range zones {
		if _, err := c.FileURL(ctx, trackID, MP3, c.Creds.Secrets[z]); err == nil || errors.Is(err, ErrNoFile) {
			return c.Creds.Secrets[z], nil
		} else {
			last = err
			var e *Error
			if errors.As(err, &e) && e.Auth() {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("no secret signs this account's requests: %w", last)
}

// Page is one page of a list, as Qobuz pages them.
type Page[T any] struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
	Items  []T `json:"items"`
}

// ID is an id as Qobuz sends it, a number or a string, kept as its exact text.
type ID string

func (id *ID) UnmarshalJSON(b []byte) error {
	*id = ID(strings.Trim(string(b), `"`))
	return nil
}

func (id ID) String() string { return string(id) }

type Artist struct {
	ID          ID     `json:"id"`
	Name        string `json:"name"`
	AlbumsCount int    `json:"albums_count"`
}

type Image struct {
	Large string `json:"large"` // 600 px
}

type Named struct {
	Name string `json:"name"`
}

type Album struct {
	ID              ID           `json:"id"`
	Title           string       `json:"title"`
	Version         string       `json:"version"`
	Artist          Artist       `json:"artist"`
	Image           Image        `json:"image"`
	Genre           Named        `json:"genre"`
	Label           Named        `json:"label"`
	TracksCount     int          `json:"tracks_count"`
	MediaCount      int          `json:"media_count"`
	MaximumBitDepth int          `json:"maximum_bit_depth"`
	MaximumRate     float64      `json:"maximum_sampling_rate"`
	ReleaseDate     string       `json:"release_date_original"`
	Streamable      bool         `json:"streamable"`
	FavoritedAt     int64        `json:"favorited_at"`
	Tracks          *Page[Track] `json:"tracks"`
}

type Track struct {
	ID              ID      `json:"id"`
	Title           string  `json:"title"`
	Version         string  `json:"version"`
	Duration        int     `json:"duration"`
	TrackNumber     int     `json:"track_number"`
	MediaNumber     int     `json:"media_number"`
	Performer       Artist  `json:"performer"`
	Composer        Artist  `json:"composer"`
	ISRC            string  `json:"isrc"`
	Copyright       string  `json:"copyright"`
	Album           *Album  `json:"album"`
	MaximumBitDepth int     `json:"maximum_bit_depth"`
	MaximumRate     float64 `json:"maximum_sampling_rate"`
	Streamable      bool    `json:"streamable"`
	FavoritedAt     int64   `json:"favorited_at"`
	AudioInfo       struct {
		Gain *float64 `json:"replaygain_track_gain"`
		Peak *float64 `json:"replaygain_track_peak"`
	} `json:"audio_info"`
}

// FullTitle is a title with its version, as Qobuz's apps show it: "Song (Live)".
func FullTitle(title, version string) string {
	if version != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(version)) {
		return title + " (" + version + ")"
	}
	return title
}

type Playlist struct {
	ID          ID           `json:"id"`
	Name        string       `json:"name"`
	TracksCount int          `json:"tracks_count"`
	Owner       Artist       `json:"owner"`
	Tracks      *Page[Track] `json:"tracks"`
}

func pageArgs(limit, offset int, more url.Values) url.Values {
	more.Set("limit", strconv.Itoa(limit))
	more.Set("offset", strconv.Itoa(offset))
	return more
}

// FavoriteAlbums is a page of the account's favourite albums, most recently added first.
func (c *Client) FavoriteAlbums(ctx context.Context, limit, offset int) (Page[Album], error) {
	var out struct {
		Albums Page[Album] `json:"albums"`
	}
	err := c.get(ctx, "favorite/getUserFavorites", pageArgs(limit, offset, url.Values{"type": {"albums"}}), &out)
	return out.Albums, err
}

// FavoriteTracks is a page of the account's favourite tracks, each with its album.
func (c *Client) FavoriteTracks(ctx context.Context, limit, offset int) (Page[Track], error) {
	var out struct {
		Tracks Page[Track] `json:"tracks"`
	}
	err := c.get(ctx, "favorite/getUserFavorites", pageArgs(limit, offset, url.Values{"type": {"tracks"}}), &out)
	return out.Tracks, err
}

// FavoriteArtists is a page of the account's favourite artists.
func (c *Client) FavoriteArtists(ctx context.Context, limit, offset int) (Page[Artist], error) {
	var out struct {
		Artists Page[Artist] `json:"artists"`
	}
	err := c.get(ctx, "favorite/getUserFavorites", pageArgs(limit, offset, url.Values{"type": {"artists"}}), &out)
	return out.Artists, err
}

// Playlists is a page of the account's playlists, its own and those it follows.
func (c *Client) Playlists(ctx context.Context, limit, offset int) (Page[Playlist], error) {
	var out struct {
		Playlists Page[Playlist] `json:"playlists"`
	}
	err := c.get(ctx, "playlist/getUserPlaylists", pageArgs(limit, offset, url.Values{}), &out)
	return out.Playlists, err
}

// PlaylistTracks is a playlist with a page of its tracks, in its order, repeats kept.
func (c *Client) PlaylistTracks(ctx context.Context, id string, limit, offset int) (Playlist, error) {
	var out Playlist
	err := c.get(ctx, "playlist/get", pageArgs(limit, offset, url.Values{"playlist_id": {id}, "extra": {"tracks"}}), &out)
	return out, err
}

// Album is an album with all its tracks.
func (c *Client) Album(ctx context.Context, id string) (Album, error) {
	var out Album
	if err := c.get(ctx, "album/get", pageArgs(500, 0, url.Values{"album_id": {id}}), &out); err != nil {
		return out, err
	}
	for out.Tracks != nil && len(out.Tracks.Items) < out.Tracks.Total { // a box set past 500 tracks
		var more Album
		if err := c.get(ctx, "album/get", pageArgs(500, len(out.Tracks.Items), url.Values{"album_id": {id}}), &more); err != nil {
			return out, err // not taken for the whole album
		}
		if more.Tracks == nil || len(more.Tracks.Items) == 0 {
			break
		}
		out.Tracks.Items = append(out.Tracks.Items, more.Tracks.Items...)
	}
	return out, nil
}

// Track is one track, with its album.
func (c *Client) Track(ctx context.Context, id string) (Track, error) {
	var t Track
	err := c.get(ctx, "track/get", url.Values{"track_id": {id}}, &t)
	return t, err
}

// ArtistAlbums is an artist with their albums.
func (c *Client) ArtistAlbums(ctx context.Context, id string) (Artist, []Album, error) {
	var out struct {
		Artist
		Albums Page[Album] `json:"albums"`
	}
	err := c.get(ctx, "artist/get", url.Values{"artist_id": {id}, "extra": {"albums"}, "limit": {"200"}}, &out)
	return out.Artist, out.Albums.Items, err
}

// Found is a search's results.
type Found struct {
	Albums    Page[Album]    `json:"albums"`
	Tracks    Page[Track]    `json:"tracks"`
	Artists   Page[Artist]   `json:"artists"`
	Playlists Page[Playlist] `json:"playlists"`
}

// Search finds albums, tracks, artists and playlists in the catalogue.
func (c *Client) Search(ctx context.Context, query string, limit int) (Found, error) {
	var out Found
	err := c.get(ctx, "catalog/search", url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}}, &out)
	return out, err
}

// User is the signed-in account.
type User struct {
	ID          ID     `json:"id"`
	DisplayName string `json:"display_name"`
	Credential  struct {
		Label      string `json:"label"`
		Parameters struct {
			HiRes    bool   `json:"hires_streaming"`
			Lossless bool   `json:"lossless_streaming"`
			Label    string `json:"short_label"`
		} `json:"parameters"`
	} `json:"credential"`
}

// User is the account and its plan.
func (c *Client) User(ctx context.Context) (User, error) {
	var out User
	err := c.get(ctx, "user/get", url.Values{}, &out)
	return out, err
}
