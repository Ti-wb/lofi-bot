package library

import (
	"errors"
	"reflect"
	"testing"
)

func TestAvailableThemes(t *testing.T) {
	lib := selectionLibrary()

	got := lib.AvailableThemes(PeriodMorning)
	want := []string{"calm", "focus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableThemes() = %#v, want %#v", got, want)
	}

	byPeriod := lib.AvailableThemesByPeriod()
	if !reflect.DeepEqual(byPeriod[PeriodMorning], want) {
		t.Fatalf("AvailableThemesByPeriod()[morning] = %#v, want %#v", byPeriod[PeriodMorning], want)
	}
	if _, ok := byPeriod[PeriodEvening]; ok {
		t.Fatalf("did not expect empty evening themes in map: %#v", byPeriod)
	}
}

func TestChooseThemeUsesInjectedRandom(t *testing.T) {
	lib := selectionLibrary()
	rng := &fakeRandom{values: []int{1}}

	got, err := lib.ChooseTheme(PeriodMorning, rng)
	if err != nil {
		t.Fatalf("ChooseTheme() error = %v", err)
	}
	if got != "focus" {
		t.Fatalf("ChooseTheme() = %q, want focus", got)
	}
	if !reflect.DeepEqual(rng.bounds, []int{2}) {
		t.Fatalf("random bounds = %#v, want [2]", rng.bounds)
	}
}

func TestChooseLoopUsesInjectedRandom(t *testing.T) {
	lib := selectionLibrary()
	rng := &fakeRandom{values: []int{1}}

	got, err := lib.ChooseLoop(PeriodMorning, "calm", rng)
	if err != nil {
		t.Fatalf("ChooseLoop() error = %v", err)
	}
	if got.ID != "loop-b" {
		t.Fatalf("ChooseLoop() = %#v, want loop-b", got)
	}
	if !reflect.DeepEqual(rng.bounds, []int{2}) {
		t.Fatalf("random bounds = %#v, want [2]", rng.bounds)
	}
}

func TestChooseMusicAvoidsPreviousWhenPossible(t *testing.T) {
	lib := selectionLibrary()
	rng := &fakeRandom{values: []int{0}}

	got, err := lib.ChooseMusic("music-a", rng)
	if err != nil {
		t.Fatalf("ChooseMusic() error = %v", err)
	}
	if got.ID != "music-b" {
		t.Fatalf("ChooseMusic() = %#v, want music-b", got)
	}
	if !reflect.DeepEqual(rng.bounds, []int{1}) {
		t.Fatalf("random bounds = %#v, want [1]", rng.bounds)
	}
}

func TestChooseMusicKeepsPreviousWhenOnlyCandidate(t *testing.T) {
	lib := Library{Music: []Music{{ID: "music-a", RelPath: "music/music_a.mp3"}}}
	rng := &fakeRandom{values: []int{0}}

	got, err := lib.ChooseMusic("music-a", rng)
	if err != nil {
		t.Fatalf("ChooseMusic() error = %v", err)
	}
	if got.ID != "music-a" {
		t.Fatalf("ChooseMusic() = %#v, want music-a", got)
	}
	if !reflect.DeepEqual(rng.bounds, []int{1}) {
		t.Fatalf("random bounds = %#v, want [1]", rng.bounds)
	}
}

func TestSelectionNoCandidateErrors(t *testing.T) {
	lib := selectionLibrary()

	_, err := lib.ChooseTheme(PeriodNight, &fakeRandom{values: []int{0}})
	requireSelectionError(t, err, KindLoop)

	_, err = lib.ChooseLoop(PeriodMorning, "missing", &fakeRandom{values: []int{0}})
	requireSelectionError(t, err, KindLoop)

	_, err = (Library{}).ChooseMusic("", &fakeRandom{values: []int{0}})
	requireSelectionError(t, err, KindMusic)
}

func TestSelectionRejectsNilRandom(t *testing.T) {
	lib := selectionLibrary()

	if _, err := lib.ChooseTheme(PeriodMorning, nil); err == nil {
		t.Fatal("expected nil random error")
	}
	if _, err := lib.ChooseLoop(PeriodMorning, "calm", nil); err == nil {
		t.Fatal("expected nil random error")
	}
	if _, err := lib.ChooseMusic("", nil); err == nil {
		t.Fatal("expected nil random error")
	}
}

func selectionLibrary() Library {
	return Library{
		Loops: []Loop{
			{ID: "loop-c", RelPath: "loops/loop_morning_focus_a.mp4", Period: PeriodMorning, Theme: "focus"},
			{ID: "loop-b", RelPath: "loops/loop_morning_calm_b.mp4", Period: PeriodMorning, Theme: "calm"},
			{ID: "loop-a", RelPath: "loops/loop_morning_calm_a.mp4", Period: PeriodMorning, Theme: "calm"},
			{ID: "loop-d", RelPath: "loops/loop_day_focus_a.mp4", Period: PeriodDay, Theme: "focus"},
		},
		Music: []Music{
			{ID: "music-b", RelPath: "music/music_b.mp3"},
			{ID: "music-a", RelPath: "music/music_a.mp3"},
		},
	}
}

func requireSelectionError(t *testing.T, err error, kind Kind) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var libErr *Error
	if !errors.As(err, &libErr) {
		t.Fatalf("expected library Error, got %T", err)
	}
	if libErr.Code != ErrorNoCandidates || libErr.Kind != kind {
		t.Fatalf("error = %#v, want no candidates for %s", libErr, kind)
	}
}

type fakeRandom struct {
	values []int
	bounds []int
}

func (r *fakeRandom) Intn(n int) int {
	r.bounds = append(r.bounds, n)
	if len(r.values) == 0 {
		return 0
	}
	value := r.values[0]
	r.values = r.values[1:]
	return value
}
