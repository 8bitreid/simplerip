package ripper

import (
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
)

func title(name string, h, m, s int) disc.MKVTitle {
	return disc.MKVTitle{
		Name:     name,
		Duration: time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second,
	}
}

func TestClassifyTitles(t *testing.T) {
	cfg := config.DetectionConfig{
		TVThreshold:          3,
		DurationToleranceSec: 60,
		MinFeatureMinutes:    40,
		MinExtraMinutes:      2,
	}

	tests := []struct {
		name        string
		titles      []disc.MKVTitle
		wantPattern DiscPattern
		wantMain    int
		wantExtras  int
		wantJunk    int
		// checkMain optionally asserts a specific title is in MainTitles.
		checkMain string
	}{
		{
			name: "single movie with extras and junk",
			titles: []disc.MKVTitle{
				title("The Matrix", 2, 16, 0),
				title("Making Of", 0, 25, 0),
				title("Deleted Scenes", 0, 8, 0),
				title("Junk", 0, 1, 0),
			},
			wantPattern: DiscPatternMovie,
			wantMain:    1,
			wantExtras:  2,
			wantJunk:    1,
			checkMain:   "The Matrix",
		},
		{
			name: "TV disc five identical-length episodes",
			titles: []disc.MKVTitle{
				title("S01E01", 0, 42, 0),
				title("S01E02", 0, 42, 0),
				title("S01E03", 0, 42, 0),
				title("S01E04", 0, 42, 0),
				title("S01E05", 0, 42, 0),
				title("Junk", 0, 0, 30),
			},
			wantPattern: DiscPatternTV,
			wantMain:    5,
			wantExtras:  0,
			wantJunk:    1,
		},
		{
			name: "TV disc episodes within 60s spread",
			titles: []disc.MKVTitle{
				title("S01E01", 0, 42, 0),
				title("S01E02", 0, 42, 30),
				title("S01E03", 0, 42, 45),
				title("S01E04", 0, 43, 0),
				title("S01E05", 0, 41, 30),
			},
			wantPattern: DiscPatternTV,
			wantMain:    5,
			wantExtras:  0,
			wantJunk:    0,
		},
		{
			name: "double feature two same-duration feature-length titles",
			titles: []disc.MKVTitle{
				title("Movie A", 1, 45, 0),
				title("Movie B", 1, 45, 30),
			},
			wantPattern: DiscPatternDouble,
			wantMain:    0,
			wantExtras:  2,
			wantJunk:    0,
		},
		{
			name: "double feature with short extras",
			titles: []disc.MKVTitle{
				title("Movie A", 1, 52, 0),
				title("Movie B", 1, 51, 30),
				title("Trailer", 0, 2, 30),
				title("Making Of", 0, 15, 0),
			},
			wantPattern: DiscPatternDouble,
			wantMain:    0,
			wantExtras:  4,
			wantJunk:    0,
		},
		{
			// Solo (2018): 9 titles all with the same playlist duration —
			// a common mastering artifact on some Blu-rays.
			name: "Solo-style nine identical-duration tracks",
			titles: []disc.MKVTitle{
				title("Title 0", 2, 20, 30),
				title("Title 1", 2, 20, 30),
				title("Title 2", 2, 20, 30),
				title("Title 3", 2, 20, 30),
				title("Title 4", 2, 20, 30),
				title("Title 5", 2, 20, 30),
				title("Title 6", 2, 20, 30),
				title("Title 7", 2, 20, 30),
				title("Title 8", 2, 20, 30),
			},
			wantPattern: DiscPatternTV,
			wantMain:    9,
			wantExtras:  0,
			wantJunk:    0,
		},
		{
			name: "mixed extras varied durations",
			titles: []disc.MKVTitle{
				title("Dune", 1, 56, 0),
				title("Director Commentary", 0, 25, 0),
				title("Visual Effects", 0, 18, 0),
				title("Behind the Scenes", 0, 12, 0),
				title("Deleted Scenes", 0, 8, 0),
				title("Trailer", 0, 2, 30),
				title("FBI Warning", 0, 1, 0),
				title("Logo", 0, 0, 45),
			},
			wantPattern: DiscPatternMovie,
			wantMain:    1,
			wantExtras:  5,
			wantJunk:    2,
			checkMain:   "Dune",
		},
		{
			name: "exactly at TV threshold",
			titles: []disc.MKVTitle{
				title("Ep1", 0, 44, 0),
				title("Ep2", 0, 44, 30),
				title("Ep3", 0, 44, 15),
			},
			wantPattern: DiscPatternTV,
			wantMain:    3,
			wantExtras:  0,
			wantJunk:    0,
		},
		{
			// Two short titles within 60s of each other (not feature-length)
			// should NOT trigger double-feature — the single long title is the movie.
			name: "two same-duration extras should not trigger double feature",
			titles: []disc.MKVTitle{
				title("Inception", 2, 28, 0),
				title("Extra A", 0, 10, 0),
				title("Extra B", 0, 10, 30),
			},
			wantPattern: DiscPatternMovie,
			wantMain:    1,
			wantExtras:  2,
			wantJunk:    0,
			checkMain:   "Inception",
		},
		{
			// Multiple feature-length titles with no same-duration pair → ambiguous.
			name: "multiple features no duration cluster",
			titles: []disc.MKVTitle{
				title("Feature A", 2, 0, 0),
				title("Feature B", 1, 20, 0),
			},
			wantPattern: DiscPatternAmbiguous,
			wantMain:    0,
			wantExtras:  2,
			wantJunk:    0,
		},
		{
			name: "all junk",
			titles: []disc.MKVTitle{
				title("Junk1", 0, 0, 30),
				title("Junk2", 0, 1, 0),
				title("Junk3", 0, 1, 45),
			},
			wantPattern: DiscPatternAmbiguous,
			wantMain:    0,
			wantExtras:  0,
			wantJunk:    3,
		},
		{
			name:        "empty input",
			titles:      nil,
			wantPattern: DiscPatternAmbiguous,
			wantMain:    0,
			wantExtras:  0,
			wantJunk:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyTitles(tc.titles, cfg)

			if got.Pattern != tc.wantPattern {
				t.Errorf("Pattern = %s, want %s", got.Pattern, tc.wantPattern)
			}
			if len(got.MainTitles) != tc.wantMain {
				t.Errorf("MainTitles count = %d, want %d", len(got.MainTitles), tc.wantMain)
			}
			if len(got.ExtraTitles) != tc.wantExtras {
				t.Errorf("ExtraTitles count = %d, want %d", len(got.ExtraTitles), tc.wantExtras)
			}
			if len(got.JunkTitles) != tc.wantJunk {
				t.Errorf("JunkTitles count = %d, want %d", len(got.JunkTitles), tc.wantJunk)
			}
			if tc.checkMain != "" {
				found := false
				for _, mt := range got.MainTitles {
					if mt.Name == tc.checkMain {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q in MainTitles, got %v", tc.checkMain, got.MainTitles)
				}
			}
		})
	}
}

func TestBuildClusters(t *testing.T) {
	tolerance := 60 * time.Second
	titles := []disc.MKVTitle{
		title("A", 0, 42, 0),
		title("B", 0, 42, 30),
		title("C", 0, 42, 59), // still within 59s of A
		title("D", 0, 44, 0),  // 2m from A — new cluster
		title("E", 1, 50, 0),  // isolated
	}
	clusters := buildClusters(titles, tolerance)
	if len(clusters) != 3 {
		t.Fatalf("cluster count = %d, want 3: %v", len(clusters), clusters)
	}
	if len(clusters[0]) != 3 {
		t.Errorf("cluster[0] size = %d, want 3", len(clusters[0]))
	}
	if len(clusters[1]) != 1 {
		t.Errorf("cluster[1] size = %d, want 1", len(clusters[1]))
	}
	if len(clusters[2]) != 1 {
		t.Errorf("cluster[2] size = %d, want 1", len(clusters[2]))
	}
}

func TestBuildClustersEdgeCases(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := buildClusters(nil, 60*time.Second); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
	t.Run("single title", func(t *testing.T) {
		clusters := buildClusters([]disc.MKVTitle{title("A", 1, 0, 0)}, 60*time.Second)
		if len(clusters) != 1 || len(clusters[0]) != 1 {
			t.Errorf("unexpected clusters: %v", clusters)
		}
	})
	t.Run("exactly at tolerance boundary", func(t *testing.T) {
		// 60s apart — exactly at boundary, still same cluster (≤ tolerance).
		titles := []disc.MKVTitle{title("A", 0, 42, 0), title("B", 0, 43, 0)}
		clusters := buildClusters(titles, 60*time.Second)
		if len(clusters) != 1 {
			t.Errorf("want 1 cluster at boundary, got %d", len(clusters))
		}
	})
	t.Run("one second over tolerance", func(t *testing.T) {
		titles := []disc.MKVTitle{title("A", 0, 42, 0), title("B", 0, 43, 1)}
		clusters := buildClusters(titles, 60*time.Second)
		if len(clusters) != 2 {
			t.Errorf("want 2 clusters one second over tolerance, got %d", len(clusters))
		}
	})
}

func TestDiscPatternString(t *testing.T) {
	tests := []struct {
		in   DiscPattern
		want string
	}{
		{in: DiscPatternTV, want: "TV"},
		{in: DiscPatternMovie, want: "Movie"},
		{in: DiscPatternDouble, want: "Double"},
		{in: DiscPatternAmbiguous, want: "Ambiguous"},
	}

	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Fatalf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestClassifyTitlesMultiAngle(t *testing.T) {
	cfg := config.DetectionConfig{
		TVThreshold:          3,
		DurationToleranceSec: 60,
		MinFeatureMinutes:    40,
		MinExtraMinutes:      2,
	}

	titles := []disc.MKVTitle{
		{Name: "Main Angle 1", Duration: 110 * time.Minute, ChapterCount: 20, AngleNumber: 1},
		{Name: "Main Angle 2", Duration: 110 * time.Minute, ChapterCount: 20, AngleNumber: 2},
		{Name: "Bonus", Duration: 10 * time.Minute, ChapterCount: 4},
	}

	got := ClassifyTitles(titles, cfg)
	if !got.MultiAngle {
		t.Fatal("expected MultiAngle = true")
	}
	if got.AngleCount != 2 {
		t.Fatalf("AngleCount = %d, want 2", got.AngleCount)
	}
	if got.Pattern != DiscPatternMovie {
		t.Fatalf("Pattern = %v, want %v", got.Pattern, DiscPatternMovie)
	}
	if len(got.MainTitles) != 1 || got.MainTitles[0].AngleNumber != 1 {
		t.Fatalf("MainTitles = %+v, want only angle 1", got.MainTitles)
	}
}
