// Package client wraps gompd with a thread-safe client and attr helpers.
package client

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/fhs/gompd/v2/mpd"
)

// Attrs aliases gompd attr maps.
type Attrs = mpd.Attrs

// Client is a mutex-guarded MPD connection.
type Client struct {
	mu sync.Mutex
	c  *mpd.Client
}

// Dial connects to addr ("localhost:6600").
func Dial(addr string) (*Client, error) {
	c, err := mpd.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mpd dial %s: %w", addr, err)
	}
	return &Client{c: c}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Close()
}

// Status returns player status.
func (c *Client) Status() (mpd.Attrs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Status()
}

// CurrentSong returns current song attrs.
func (c *Client) CurrentSong() (mpd.Attrs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.CurrentSong()
}

// Queue returns the whole queue in order.
func (c *Client) Queue() ([]mpd.Attrs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.PlaylistInfo(-1, -1)
}

// PlayID starts playing the given song id.
func (c *Client) PlayID(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.PlayID(id)
}

// Play starts playback at the given queue position.
func (c *Client) Play(pos int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Play(pos)
}

// Pause pauses/resumes.
func (c *Client) Pause(pause bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Pause(pause)
}

// Next skips to the next song.
func (c *Client) Next() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Next()
}

// Prev returns to the previous song.
func (c *Client) Prev() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Previous()
}

// SetVolume sets the mixer volume (0-100).
func (c *Client) SetVolume(v int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.SetVolume(v)
}

// Append adds a URI at the end of the queue.
func (c *Client) Append(uri string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Add(uri)
}

// AppendID adds a URI and returns its new song id.
func (c *Client) AppendID(uri string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.AddID(uri, -1)
}

// Clear empties the queue.
func (c *Client) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Clear()
}

// Replace plays exactly one uri (clear + add + play).
func (c *Client) Replace(uri string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.c.Clear(); err != nil {
		return err
	}
	if err := c.c.Add(uri); err != nil {
		return err
	}
	return c.c.Play(0)
}

// ListAllInfo returns metadata for every song in the library (listallinfo).
func (c *Client) ListAllInfo() ([]mpd.Attrs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.ListAllInfo("")
}

// Attr fetches a key case-insensitively.
func Attr(a mpd.Attrs, key string) (string, bool) {
	for k, v := range a {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// AttrOr returns a[s] with a default.
func AttrOr(a mpd.Attrs, key, def string) string {
	if v, ok := Attr(a, key); ok {
		return v
	}
	return def
}

// Int parses an int attr with default.
func Int(a mpd.Attrs, key string, def int) int {
	if v, ok := Attr(a, key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// Float parses a float attr with default.
func Float(a mpd.Attrs, key string, def float64) float64 {
	if v, ok := Attr(a, key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}
