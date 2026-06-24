package library

import "sort"

// Random is the small part of *rand.Rand used by selection helpers.
type Random interface {
	Intn(n int) int
}

func (l Library) AvailableThemes(period Period) []string {
	seen := make(map[string]struct{})
	for _, loop := range l.Loops {
		if loop.Period == period {
			seen[loop.Theme] = struct{}{}
		}
	}
	themes := make([]string, 0, len(seen))
	for theme := range seen {
		themes = append(themes, theme)
	}
	sort.Strings(themes)
	return themes
}

func (l Library) AvailableThemesByPeriod() map[Period][]string {
	result := make(map[Period][]string)
	for _, period := range Periods() {
		if themes := l.AvailableThemes(period); len(themes) > 0 {
			result[period] = themes
		}
	}
	return result
}

func (l Library) ChooseTheme(period Period, rng Random) (string, error) {
	if rng == nil {
		return "", fileError(ErrorInvalidRandom, KindLoop, "", "random", "", nil)
	}
	themes := l.AvailableThemes(period)
	if len(themes) == 0 {
		return "", noCandidates(KindLoop, "period", string(period))
	}
	return themes[rng.Intn(len(themes))], nil
}

func (l Library) ChooseLoop(period Period, theme string, rng Random) (Loop, error) {
	if rng == nil {
		return Loop{}, fileError(ErrorInvalidRandom, KindLoop, "", "random", "", nil)
	}
	candidates := make([]Loop, 0)
	for _, loop := range l.Loops {
		if loop.Period == period && loop.Theme == theme {
			candidates = append(candidates, loop)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RelPath < candidates[j].RelPath
	})
	if len(candidates) == 0 {
		return Loop{}, noCandidates(KindLoop, "theme", theme)
	}
	return candidates[rng.Intn(len(candidates))], nil
}

func (l Library) ChooseMusic(previousID string, rng Random) (Music, error) {
	if rng == nil {
		return Music{}, fileError(ErrorInvalidRandom, KindMusic, "", "random", "", nil)
	}
	candidates := make([]Music, len(l.Music))
	copy(candidates, l.Music)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RelPath < candidates[j].RelPath
	})
	if len(candidates) == 0 {
		return Music{}, noCandidates(KindMusic, "", "")
	}
	if previousID != "" && len(candidates) > 1 {
		filtered := candidates[:0]
		for _, music := range candidates {
			if music.ID != previousID {
				filtered = append(filtered, music)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	return candidates[rng.Intn(len(candidates))], nil
}
