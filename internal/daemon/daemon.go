// Package daemon glues library sync, likes import and the play-signal
// observer fed by MPD idle events.
package daemon

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mpdclient "wave/internal/mpd"
	"wave/internal/store"
	"wave/internal/yandex"
)

// LibrarySync fetches listallinfo and upserts tracks. Returns row count.
func LibrarySync(c *mpdclient.Client, st *store.Store) (int, error) {
	attrs, err := c.ListAllInfo()
	if err != nil {
		return 0, fmt.Errorf("listallinfo: %w", err)
	}
	tracks := make([]store.Track, 0, len(attrs))
	for _, a := range attrs {
		uri, ok := mpdclient.Attr(a, "file")
		if !ok || uri == "" {
			continue
		}
		t := store.Track{
			ID:     uri,
			Path:   uri,
			Artist: mpdclient.AttrOr(a, "artist", ""),
			Title:  mpdclient.AttrOr(a, "title", ""),
			Album:  mpdclient.AttrOr(a, "album", ""),
		}
		// Time may be seconds ("214") or h:mm:ss for long items.
		t.DurationMS = parseTimeSec(mpdclient.AttrOr(a, "Time", "0")) * 1000
		tracks = append(tracks, t)
	}
	if err := st.UpsertTracks(tracks); err != nil {
		return 0, err
	}
	return len(tracks), nil
}

func parseTimeSec(s string) int64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	var sec int64
	mult := int64(1)
	for i := len(parts) - 1; i >= 0 && i >= len(parts)-3; i-- {
		n, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return sec
		}
		sec += n * mult
		mult *= 60
	}
	return sec
}

// LikesImport imports Yandex likes once (guarded by meta.likes_imported),
// matching against the library. Returns (matched, total, importedNow, error).
func LikesImport(st *store.Store, likesFile string) (int, int, bool, error) {
	imported, err := st.LikesImported()
	if err != nil {
		return 0, 0, false, err
	}
	if imported {
		n, err := st.CountLikes()
		return n, n, false, err
	}
	list, err := yandex.Load(likesFile)
	if err != nil {
		return 0, 0, false, err
	}
	all, err := st.AllTracks()
	if err != nil {
		return 0, len(list), false, err
	}
	idx := map[string]string{} // normkey → track id
	for _, t := range all {
		key := yandex.TrackKey(yandex.Norm(t.Artist), yandex.Norm(t.Title))
		if _, exists := idx[key]; !exists {
			idx[key] = t.ID
		}
	}
	var matched []string
	for _, l := range list {
		if !l.Available {
			continue
		}
		for _, key := range yandex.KeyCandidates(l) {
			if id, ok := idx[key]; ok {
				matched = append(matched, id)
				break
			}
		}
	}
	// second pass: path fallback — a noticeable part of the library has
	// either empty or diverging tags but the names live in file/dir names
	// (same trick as the parent yandex-wave project's matcher).
	if rem := len(list) - len(matched); rem > 0 && len(all) > 0 {
		gain := pathMatchFallback(all, list, matched)
		matched = append(matched, gain...)
	}
	if err := st.ImportLikes(matched, time.Now().UnixMilli()); err != nil {
		return 0, len(list), false, err
	}
	return len(matched), len(list), true, nil
}

// pathMatchFallback matches still-unmatched likes against library paths:
// normalized file stem == like title AND a compact (space-free) form of the
// like artist present in the directory part of the URI.
func pathMatchFallback(all []store.Track, list []yandex.Like, already []string) (matches []string) {
	norm := yandex.Norm
	byStem := map[string][]store.Track{}
	for _, t := range all {
		stem := pathStem(t.Path)
		if stem == "" {
			continue
		}
		key := norm(stem)
		if key == "" {
			continue
		}
		byStem[key] = append(byStem[key], t)
	}
	if len(byStem) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, id := range already {
		have[id] = true
	}
	for _, l := range list {
		if !l.Available {
			continue
		}
		compact := strings.ReplaceAll(norm(l.Artist), " ", "")
		if len([]rune(compact)) < 3 {
			continue
		}
		titleKey := norm(l.Title)
		if titleKey == "" {
			continue
		}
		for _, t := range preferNonReview(byStem[titleKey]) {
			if have[t.ID] {
				continue
			}
			dir := t.Path
			if i := strings.LastIndex(dir, "/"); i > 0 {
				dir = dir[:i]
			}
			dirCompact := strings.ReplaceAll(norm(dir), " ", "")
			if strings.Contains(dirCompact, compact) {
				matches = append(matches, t.ID)
				have[t.ID] = true
				break
			}
		}
	}
	return matches
}

// preferNonReview orders candidates: regular collections first, shortest path
// first; _Review dumps last.
func preferNonReview(ts []store.Track) []store.Track {
	out := append([]store.Track(nil), ts...)
	sort.Slice(out, func(i, j int) bool {
		ri := strings.HasPrefix(out[i].Path, "_Review")
		rj := strings.HasPrefix(out[j].Path, "_Review")
		if ri != rj {
			return rj // non-_Review first
		}
		return len(out[i].Path) < len(out[j].Path)
	})
	return out
}

var leadNumPrefixRe = regexp.MustCompile(`^(?:CD\s*\d+[-._ ]+|Disc\s*\d+[-._ ]+|\d{1,3}[-._ ]+)+`)
var cuePseudoRe = regexp.MustCompile(`\.cue/track\d+$`)

// pathStem extracts a matchable file-name stem from an MPD URI.
func pathStem(uri string) string {
	uri = strings.ReplaceAll(uri, "\\", "/")
	stem := uri
	if i := strings.LastIndex(stem, "/"); i >= 0 {
		stem = stem[i+1:]
	}
	if cuePseudoRe.MatchString(stem) || strings.HasSuffix(uri, ".cue") {
		return "" // cue pseudo-tracks have no usable stem
	}
	if k := strings.LastIndexByte(stem, '.'); k > 0 {
		stem = stem[:k]
	}
	stem = leadNumPrefixRe.ReplaceAllString(strings.TrimSpace(stem), "")
	return stem
}

// ---------- play-signal observer ----------

// PlayState is the last observed player state, needed to detect song changes
// and whether the previous song was really played through.
type PlayState struct {
	URI      string
	Elapsed  float64 // seconds at the moment of the latest observation
	Duration float64
	State    string // play/pause/stop
}

// Observer processes player state and records plays into the DB.
type Observer struct {
	st  *store.Store
	cur PlayState
}

// NewObserver creates an Observer.
func NewObserver(st *store.Store) *Observer { return &Observer{st: st} }

// Observe consumes a fresh status/current-song pair; on a song change it
// decides whether the previous song counts as a play (contract rules):
// change requires ≥50% elapsed of the previous song (or it ended) and
// ≥30 s of actual playback.
func (o *Observer) Observe(status, song mpdclient.Attrs) error {
	uri, _ := mpdclient.Attr(song, "file")
	state := mpdclient.AttrOr(status, "state", "stop")
	elapsed := mpdclient.Float(status, "elapsed", 0)
	dur := mpdclient.Float(status, "duration", 0)
	if dur == 0 {
		dur = mpdclient.Float(song, "duration", 0)
	}
	prev := o.cur
	o.cur = PlayState{URI: uri, Elapsed: elapsed, Duration: dur, State: state}
	o.recordIfChanged(prev, o.cur)
	return nil
}

// recordIfChanged decides whether the previous song counts as a play.
func (o *Observer) recordIfChanged(prev, cur PlayState) {
	if prev.URI == "" || prev.URI == cur.URI {
		return
	}
	threshold := 0.5 * prev.Duration
	reached := prev.Duration > 0 && prev.Elapsed >= threshold
	ended := prev.Duration > 0 && prev.Elapsed >= prev.Duration-1.5
	if !(reached || ended) {
		return
	}
	ms := int64(prev.Elapsed * 1000)
	if ms < 30000 {
		return // plays shorter than 30 s are not recorded
	}
	if err := o.st.RecordPlay(prev.URI, time.Now().UnixMilli(), ms); err != nil {
		log.Printf("record play: %v", err)
	}
}

// RecordSkip records a manual skip of the current song together with its
// elapsed time; the UI "s" key uses it before advancing MPD.
func (o *Observer) RecordSkip() error {
	if o.cur.URI == "" || o.cur.State == "stop" {
		return nil
	}
	return o.st.RecordSkip(o.cur.URI, time.Now().UnixMilli())
}

// CurrentURI returns the most recently observed URI.
func (o *Observer) CurrentURI() string { return o.cur.URI }
