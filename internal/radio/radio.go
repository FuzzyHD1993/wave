// Package radio implements the "Волна" wave picker: artist scores from
// likes/plays/skips shares with recency penalties, 80% weighted random /
// 20% exploration, track de-duplication against recent plays and skips.
package radio

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"wave/internal/store"
)

const (
	upcomingTarget   = 20 // keep this many upcoming tracks in the queue
	upcomingMin      = 5  // top-up trigger threshold
	exploreRatio     = 0.2
	recentTracks     = 50
	recentArtists    = 30
	artistPenaltySet = 0.2
	artistPenaltyMid = 0.6
	recentSkipDays   = 7
)

// Track alias for brevity.
type Track = store.Track

// Ctx is mutable pick context: exclusions and recency state.
type Ctx struct {
	// exclude — track ids that must not be picked (queue + recent plays +
	// freshly generated picks).
	exclude map[string]bool
	// recentArtists — newest first, index <10 → ×0.2, <30 → ×0.6.
	recentArtists []string
	// skipped — track ids skipped in the last 7 days.
	skipped map[string]bool
	// trackCache — artist → their tracks (lazy).
	trackCache map[string][]Track
}

// Radio picks tracks from the library using user signals.
type Radio struct {
	st *store.Store
}

// New creates a Radio.
func New(st *store.Store) *Radio { return &Radio{st: st} }

// UpcomingTarget/UpcomingMin expose queue constants for the UI layer.
const (
	UpcomingTarget = upcomingTarget
	UpcomingMin    = upcomingMin
)

// NewContext loads pick context from the DB; extra is a set of track ids to
// additionally exclude (e.g. the MPD queue contents).
func (r *Radio) NewContext(extra map[string]bool) (*Ctx, error) {
	c := &Ctx{
		exclude:    map[string]bool{},
		skipped:    map[string]bool{},
		trackCache: map[string][]Track{},
	}
	if extra != nil {
		for id := range extra {
			c.exclude[id] = true
		}
	}
	recent, err := r.st.RecentPlayTracks(recentTracks)
	if err != nil {
		return nil, err
	}
	for _, id := range recent {
		c.exclude[id] = true
	}
	last30 := recent
	if len(last30) > recentArtists {
		last30 = last30[:recentArtists]
	}
	artistOf, err := r.artistsOf(last30)
	if err != nil {
		return nil, err
	}
	for _, id := range last30 {
		if a, ok := artistOf[id]; ok && a != "" {
			c.addRecentArtist(a)
		}
	}
	since := time.Now().AddDate(0, -0, -recentSkipDays).UnixMilli()
	c.skipped, err = r.st.RecentSkips(since)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Ctx) addRecentArtist(a string) {
	// move to front, dedupe, cap at 30
	out := []string{a}
	for _, x := range c.recentArtists {
		if x != a {
			out = append(out, x)
		}
	}
	if len(out) > recentArtists {
		out = out[:recentArtists]
	}
	c.recentArtists = out
}

// Record adds a freshly picked track to the context (exclusion + recency).
func (c *Ctx) Record(t Track) {
	c.exclude[t.ID] = true
	if t.Artist != "" {
		c.addRecentArtist(t.Artist)
	}
}

func (r *Radio) artistsOf(ids []string) (map[string]string, error) {
	need := ids
	if len(need) == 0 {
		return map[string]string{}, nil
	}
	artists := map[string]string{}
	for _, id := range need {
		if _, ok := artists[id]; ok {
			continue
		}
		t, err := r.st.TrackByID(id)
		if err != nil {
			return nil, err
		}
		artists[id] = t.Artist
	}
	return artists, nil
}

// ---------- scoring ----------

type scoredArtist struct {
	name   string
	score  float64
	likes  float64
	plays  int64
	explore bool // zero plays
}

// Weights holds the ready-to-sample artist score distribution.
type Weights struct {
	all       []scoredArtist
	byName    map[string]*scoredArtist
	totalMass float64
	totalExplore float64
}

// Weights computes the artist score distribution.
//
// base = 1 + 3*likes_norm + log1p(plays_norm) - 2*skips_norm, where each
// *_norm is the artist's share of its signal; recency penalty ×0.2 (last 10
// plays) / ×0.6 (last 30). With zero play history the weights are seeded
// from likes only; with no likes either, nil is returned (caller falls back
// to uniform-over-albums).
func (r *Radio) Weights(ctx *Ctx) (*Weights, error) {
	sig, err := r.st.ArtistSignals()
	if err != nil {
		return nil, err
	}
	var totalLikes, totalPlays, totalSkips float64
	for _, a := range sig {
		totalLikes += float64(a.Likes)
		totalPlays += float64(a.Plays)
		totalSkips += float64(a.Skips)
	}

	memset := totalLikes == 0 && totalPlays == 0 && totalSkips == 0
	if memset {
		return nil, nil // no usable signal → uniform over albums
	}
	recent := ctx.recentArtistIdx()
	w := &Weights{byName: map[string]*scoredArtist{}}
	for _, a := range sig {
		var base float64
		hasAnySignal := float64(a.Likes)+float64(a.Plays) > 0 || totalPlays > 0
		if !hasAnySignal {
			base = 1 // no history, no likes handled by caller fallback
		} else {
			likesNorm := 0.0
			if totalLikes > 0 {
				likesNorm = float64(a.Likes) / totalLikes
			}
			playsNorm := 0.0
			if totalPlays > 0 {
				playsNorm = float64(a.Plays) / totalPlays
			}
			skipsNorm := 0.0
			if totalSkips > 0 {
				skipsNorm = math.Min(float64(a.Skips)/totalSkips, 1)
			}
			base = 1 + 3*likesNorm + math.Log1p(playsNorm) - 2*skipsNorm
			// recency penalty
			if pos, ok := recent[a.Artist]; ok {
				if pos < 10 {
					base *= artistPenaltySet
				} else {
					base *= artistPenaltyMid
				}
			}
		}
		if base < 0.03 {
			base = 0.03 // never fully excluded; recovers over time
		}
		sa := scoredArtist{
			name:    a.Artist,
			score:   base,
			likes:   float64(a.Likes),
			plays:   a.Plays,
			explore: a.Plays == 0,
		}
		w.all = append(w.all, sa)
		w.byName[a.Artist] = &w.all[len(w.all)-1]
		w.totalMass += base
		if a.Plays == 0 {
			// exploration weight: likes preferred, at least 1
			w.totalExplore += math.Max(1, 1+sa.likes)
		}
	}
	return w, nil
}

// recentArtistIdx maps artist → position in recent list (0 = most recent).
func (c *Ctx) recentArtistIdx() map[string]int {
	m := map[string]int{}
	for i, a := range c.recentArtists {
		if _, ok := m[a]; !ok {
			m[a] = i
		}
	}
	return m
}

// PickArtist samples an artist: 80% weighted random over all, 20%
// exploration among zero-play artists (weighted by likes when present).
func (w *Weights) PickArtist(rng *rand.Rand) (*scoredArtist, bool) {
	explore := rng.Float64() < exploreRatio
	if explore && w.totalExplore > 0 {
		target := rng.Float64() * w.totalExplore
		acc := 0.0
		for i := range w.all {
			a := &w.all[i]
			if a.explore {
				acc += math.Max(1, 1+a.likes)
				if target <= acc {
					return a, true
				}
			}
		}
	}
	target := rng.Float64() * w.totalMass
	acc := 0.0
	for i := range w.all {
		a := &w.all[i]
		acc += a.score
		if target <= acc {
			return a, false
		}
	}
	if len(w.all) == 0 {
		return nil, false
	}
	return &w.all[len(w.all)-1], false
}

// ---------- track selection ----------

// Pick returns one wave track and advances the context.
func (r *Radio) Pick(ctx *Ctx, w *Weights, rng *rand.Rand) (Track, error) {
	// First-run fallback: no likes matched and no history → uniform over albums.
	if w == nil {
		return r.pickUniformAlbum(ctx, rng)
	}
	a, _ := w.PickArtist(rng)
	if a == nil {
		return r.pickUniformAlbum(ctx, rng)
	}
	t, ok := r.pickTrackOfArtist(ctx, rng, a.name)
	if ok {
		ctx.Record(t)
		return t, nil
	}
	// artist exhausted (all tracks excluded) — try again with a fresh artist,
	// without wasting a queue slot
	for tries := 0; tries < 8; tries++ {
		a2, _ := w.PickArtist(rng)
		if a2 == nil {
			break
		}
		if t, ok := r.pickTrackOfArtist(ctx, rng, a2.name); ok {
			ctx.Record(t)
			return t, nil
		}
	}
	return r.pickUniformAlbum(ctx, rng)
}

// pickTrackOfArtist chooses a track of the artist avoiding recently played
// (excluded) and recently skipped tracks; degrades gracefully if the
// constraints leave nothing.
func (r *Radio) pickTrackOfArtist(ctx *Ctx, rng *rand.Rand, artist string) (Track, bool) {
	tracks, ok := ctx.tracksOf(r, artist)
	if !ok || len(tracks) == 0 {
		return Track{}, false
	}
	// pass 1: not excluded, not skipped
	pool := make([]Track, 0, len(tracks))
	for _, t := range tracks {
		if !ctx.exclude[t.ID] && !ctx.skipped[t.ID] {
			pool = append(pool, t)
		}
	}
	// pass 2: allow recently played, still never a recently skipped track
	if len(pool) == 0 {
		for _, t := range tracks {
			if !ctx.skipped[t.ID] {
				pool = append(pool, t)
			}
		}
	}
	// pass 3: anything of the artist
	if len(pool) == 0 {
		pool = tracks
	}
	return pool[rng.Intn(len(pool))], true
}

// pickUniformAlbum picks a random album, then a random track of it — used
// when there is no usable signal (no likes matched, no history).
func (r *Radio) pickUniformAlbum(ctx *Ctx, rng *rand.Rand) (Track, error) {
	albums, err := r.st.AllAlbums()
	if err != nil {
		return Track{}, err
	}
	if len(albums) == 0 {
		return Track{}, fmt.Errorf("library is empty")
	}
	for tries := 0; tries < 16; tries++ {
		al := albums[rng.Intn(len(albums))]
		tracks, err := r.st.AlbumTracks(al[0], al[1])
		if err != nil {
			return Track{}, err
		}
		pool := tracks[:0:0]
		for _, t := range tracks {
			if !ctx.exclude[t.ID] && !ctx.skipped[t.ID] {
				pool = append(pool, t)
			}
		}
		if len(pool) == 0 {
			pool = tracks
		}
		if len(pool) > 0 {
			t := pool[rng.Intn(len(pool))]
			ctx.Record(t)
			return t, nil
		}
	}
	// extremely unlikely: pick absolutely anything unique
	sort.Slice(albums, func(i, j int) bool { return false })
	return r.pickUniformAlbumUncached(ctx, rng, albums)
}

func (r *Radio) pickUniformAlbumUncached(ctx *Ctx, rng *rand.Rand, albums [][2]string) (Track, error) {
	for _, al := range albums {
		tracks, err := r.st.AlbumTracks(al[0], al[1])
		if err != nil {
			return Track{}, err
		}
		for _, t := range tracks {
			if !ctx.exclude[t.ID] {
				ctx.Record(t)
				return t, nil
			}
		}
	}
	return Track{}, fmt.Errorf("no unexcluded track left in library")
}

// tracksOf lazily caches artist track lists. Library-wide caching is bounded
// to the artists actually sampled.
func (c *Ctx) tracksOf(r *Radio, artist string) ([]Track, bool) {
	if ts, ok := c.trackCache[artist]; ok {
		return ts, true
	}
	ts, err := r.st.ArtistTracks(artist)
	if err != nil {
		return nil, false
	}
	c.trackCache[artist] = ts
	return ts, true
}

// label helpers -----------------------------------------------------------

// Size returns the number of scored artists.
func (w *Weights) Size() int { return len(w.all) }
