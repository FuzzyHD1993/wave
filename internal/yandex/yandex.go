// Package yandex imports likes from a Yandex Music likes.json export into
// the wave database, matching tracks by normalized artist + title.
package yandex

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// Like is one row of the Yandex likes.json export.
type Like struct {
	Artist     string   `json:"artist"`
	Artists    []string `json:"artists"`
	Title      string   `json:"title"`
	Album      string   `json:"album"`
	DurationMS int64    `json:"duration_ms"`
	Available  bool     `json:"available"`
}

// Load reads likes.json.
func Load(path string) ([]Like, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reads %s: %w", path, err)
	}
	var list []Like
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return list, nil
}

var (
	bracketRe = regexp.MustCompile(`\([^)]*\)|\[[^\]]*\]|\{[^}]*\}|【[^】]*】|〔[^〕]*〕`)
	featRe    = regexp.MustCompile(`(?i)\b(feat|ft|featuring|vs|vs\.|x)\b.*`)
	mixRe     = regexp.MustCompile(`(?i)\b(mix|cover|remix|version|edition|remaster(ed)?|deluxe|bonus)\b.*`)
)

// Norm normalizes a name for matching: casefold, unify ё/ø and accents,
// cut feat/bracket segments, strip punctuation, collapse whitespace.
func Norm(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if r == 'ё' {
			return 'е'
		}
		if r == 'ø' {
			return 'o'
		}
		return r
	}, s)
	// strip combining accent marks (unicode Mn after decomposition is
	// expensive; direct removal covers most library spellings)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
	s = bracketRe.ReplaceAllString(s, " ")
	if i := strings.Index(s, "feat."); i >= 0 {
		s = s[:i]
	}
	s = featRe.ReplaceAllString(s, " ")
	s = mixRe.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return ' '
		}
		return r
	}, s)
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// KeyCandidates returns artist/title key candidates for a like entry:
// full artist first, then comma/&-split first artist.
func KeyCandidates(l Like) []string {
	var arts []string
	if strings.TrimSpace(l.Artist) != "" {
		arts = append(arts, l.Artist)
	}
	arts = append(arts, l.Artists...)
	seen := map[string]bool{}
	var out []string
	for _, a := range arts {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		for _, cand := range artistSplits(a) {
			n := Norm(cand)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, TrackKey(n, Norm(l.Title)))
		}
	}
	return out
}

// artistSplits splits artist strings like "A, B & C" into candidates.
func artistSplits(a string) []string {
	out := []string{a}
	for _, sep := range []string{", ", ",", " & ", "&", " × ", " x ", " feat. ", " feat ", " ft. "} {
		if i := strings.Index(a, sep); i > 0 {
			out = append(out, a[:i])
		}
	}
	return out
}

// TrackKey builds the normalized lookup key.
func TrackKey(artist, title string) string {
	return artist + "¦" + title
}
