// Tests for catalog search: provider search, the degraded fallback,
// and the community-lane interleave.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/levonn-dev/vgkeep/libs/go/contract/common"
	"github.com/levonn-dev/vgkeep/libs/go/reqtest"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/gen/api"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/igdb"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/pricecharting"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/store"
)

func TestSearch_GameThroughStubAndCache(t *testing.T) {
	s := newStack(t)

	resp := s.do(http.MethodGet, "/search?type=game&q=zelda", s.userToken(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	res := decodeBody[api.SearchResults](t, resp)
	if res.Degraded || len(res.Results) != 4 {
		t.Fatalf("want 4 non-degraded zelda hits, got %d (degraded=%v)", len(res.Results), res.Degraded)
	}
	first := res.Results[0]
	if string(first.Type) != "game" || first.IgdbGameId == nil || first.Platforms == nil || first.CoverUrl == nil {
		t.Fatalf("game result shape: %+v", first)
	}
	wantOoTDate := time.Date(1998, time.November, 21, 0, 0, 0, 0, time.UTC)
	if first.FirstReleaseDate == nil || !first.FirstReleaseDate.Equal(wantOoTDate) {
		t.Fatalf("game result first_release_date: %+v", first.FirstReleaseDate)
	}

	// Second identical query must come from Valkey: poison the
	// provider and expect the same non-degraded answer.
	s.h.games = &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return nil, errors.New("provider must not be called on a cache hit")
	}}
	resp = s.do(http.MethodGet, "/search?type=game&q=ZELDA", s.userToken(), nil)
	res = decodeBody[api.SearchResults](t, resp)
	if res.Degraded || len(res.Results) != 4 {
		t.Fatalf("cache hit broken: %d (degraded=%v)", len(res.Results), res.Degraded)
	}
}

// Pins gameResult's platform refs: OoT (fixture 1001) has three dated
// rows on platform 4 (japan, north_america, europe by date), so
// release_regions must carry that order. Hardware carries no platform ref.
func TestSearch_GameResultCarriesReleaseRegions(t *testing.T) {
	s := newStack(t)

	resp := s.do(http.MethodGet, "/search?type=game&q=zelda", s.userToken(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	res := decodeBody[api.SearchResults](t, resp)
	if len(res.Results) == 0 {
		t.Fatal("want at least the OoT hit")
	}
	first := res.Results[0] // pinned to OoT by TestSearch_GameThroughStubAndCache
	if first.Platforms == nil || len(*first.Platforms) == 0 {
		t.Fatalf("want at least one platform ref, got %+v", first.Platforms)
	}
	n64 := (*first.Platforms)[0]
	if n64.IgdbPlatformId != 4 {
		t.Fatalf("want the Nintendo 64 platform ref, got %+v", n64)
	}
	if n64.ReleaseRegions == nil {
		t.Fatal("want release_regions populated on a game platform ref")
	}
	want := []common.ReleaseRegion{"japan", "north_america", "europe"}
	if !slices.Equal(*n64.ReleaseRegions, want) {
		t.Fatalf("release_regions order: got %v, want %v", *n64.ReleaseRegions, want)
	}

	hw := s.do(http.MethodGet, "/search?type=hardware&q=nintendo+64", s.userToken(), nil)
	hwRes := decodeBody[api.SearchResults](t, hw)
	if len(hwRes.Results) == 0 {
		t.Fatal("want at least one hardware hit")
	}
	for _, r := range hwRes.Results {
		if r.Platforms != nil {
			t.Fatalf("hardware results must carry no platform ref at all: %+v", r)
		}
	}
}

// A canonical-name search returns localization bundles but no
// matched_region (canonical name needs no flagging). matchedRegion's
// own containment/translit logic is unit-tested directly elsewhere.
func TestSearch_GameResultCarriesBundlesWithoutAnnotation(t *testing.T) {
	s := newStack(t)

	resp := s.do(http.MethodGet, "/search?type=game&q=Secret+of+Mana", s.userToken(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	res := decodeBody[api.SearchResults](t, resp)
	if len(res.Results) != 1 {
		t.Fatalf("want the single Secret of Mana hit, got %d: %+v", len(res.Results), res.Results)
	}
	r := res.Results[0]
	if r.MatchedRegion != nil {
		t.Fatalf("canonical-name match must not annotate a region: %+v", *r.MatchedRegion)
	}
	if r.Localizations == nil || len(*r.Localizations) != 2 {
		t.Fatalf("want the JP + EU bundles, got %+v", r.Localizations)
	}
	locs := *r.Localizations
	// Sorted by region: EU before ja-JP.
	if locs[0].Region != "EU" || locs[0].CoverUrl == nil {
		t.Fatalf("EU bundle: %+v", locs[0])
	}
	if locs[1].Region != "ja-JP" || locs[1].Name == nil || *locs[1].Name != "聖剣伝説2" ||
		locs[1].Translit == nil || *locs[1].Translit != "Seiken Densetsu 2" {
		t.Fatalf("ja-JP bundle: %+v", locs[1])
	}
}

func TestSearch_HardwareFiltersToHardware(t *testing.T) {
	s := newStack(t)
	resp := s.do(http.MethodGet, "/search?type=hardware&q=nintendo+64", s.userToken(), nil)
	res := decodeBody[api.SearchResults](t, resp)
	if len(res.Results) != 2 {
		t.Fatalf("want the N64 system + controller, got %d: %+v", len(res.Results), res.Results)
	}
	for _, r := range res.Results {
		if string(r.Type) != "hardware" || r.PcProductId == nil || r.ConsoleName == nil || r.Category == nil {
			t.Fatalf("hardware result shape: %+v", r)
		}
		// Game products on that console must not leak into hardware
		// search (Mario Kart 64 etc. carry no hardware category).
		if *r.Category != "Systems" && *r.Category != "Controllers" {
			t.Fatalf("unexpected category: %+v", r)
		}
	}
}

func TestUnitSearch_DegradedFallsBackToCatalog(t *testing.T) {
	env := newAuthEnv(t)
	releaseDate := time.Date(1995, time.March, 11, 0, 0, 0, 0, time.UTC)
	st := &stubStore{
		searchByName: func(_ context.Context, _ []string, q string, _ int) ([]store.Product, error) {
			return []store.Product{{
				ID: "11111111-1111-1111-1111-111111111111", Type: "game", Name: "Chrono Trigger",
				Platform: &store.Platform{IGDBID: 19, Name: "Super Nintendo Entertainment System"},
				IGDB:     &store.IGDBMeta{GameID: 1011, FetchedAt: time.Now(), FirstReleaseDate: releaseDate},
			}}, nil
		},
		// Empty lane: this test pins the degraded-fallback result shape.
		searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil },
	}
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return nil, errors.New("igdb down")
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded search: %d %s", rec.Code, rec.Body.String())
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || len(res.Results) != 1 || res.Results[0].Name != "Chrono Trigger" {
		t.Fatalf("degraded result: %+v", res)
	}
	if got := res.Results[0].FirstReleaseDate; got == nil || !got.Equal(releaseDate) {
		t.Fatalf("degraded result first_release_date: %+v", got)
	}
}

// Pins that the degraded fallback scopes by kind before its row
// limit, not after (mirrors the real store query's filter-then-limit
// order). A top-N window stuffed with 20 hardware matches must still
// surface the one game match once the handler passes kind-scoped types.
func TestUnitSearch_DegradedDoesNotStarveRequestedKind(t *testing.T) {
	env := newAuthEnv(t)
	hit := store.Product{
		ID: "33333333-3333-3333-3333-333333333333", Type: "game", Name: "Chrono Trigger",
		Platform: &store.Platform{IGDBID: 19, Name: "Super Nintendo Entertainment System"},
		IGDB:     &store.IGDBMeta{GameID: 1011, FetchedAt: time.Now()},
	}
	st := &stubStore{
		searchByName: func(_ context.Context, types []string, _ string, limit int) ([]store.Product, error) {
			all := make([]store.Product, 0, 21)
			for i := 0; i < 20; i++ {
				all = append(all, store.Product{
					ID: uuid.NewString(), Type: "console", Name: "Chrono Hardware",
					PriceCharting: &store.PCMeta{PCProductID: int64(i)},
				})
			}
			all = append(all, hit)
			var out []store.Product
			for _, p := range all {
				if !slices.Contains(types, p.Type) {
					continue
				}
				out = append(out, p)
				if len(out) == limit {
					break
				}
			}
			return out, nil
		},
		searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil },
	}
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return nil, errors.New("igdb down")
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded search: %d %s", rec.Code, rec.Body.String())
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || len(res.Results) != 1 || res.Results[0].Name != "Chrono Trigger" {
		t.Fatalf("the game hit must survive a hardware-heavy top-N window: %+v", res)
	}
}

func TestUnitSearch_DegradedIsNotCached(t *testing.T) {
	env := newAuthEnv(t)
	c := newStubCache()
	st := &stubStore{
		searchByName:            func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil },
		searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil },
	}
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return nil, errors.New("igdb down")
	}}
	h := newUnitHandlers(st, games, nil, c)
	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=zzz", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK || c.puts != 0 {
		t.Fatalf("degraded answers must not be cached: %d, puts=%d", rec.Code, c.puts)
	}
}

func TestUnitSearch_CacheFailureFailsOpen(t *testing.T) {
	env := newAuthEnv(t)
	c := newStubCache()
	c.err = errors.New("valkey down")
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger"}}, nil
	}}
	// Empty lane: this test is about the cache failure, not the lane.
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, c)
	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cache outage must not fail search: %d", rec.Code)
	}
}

// The provider's relevance order is loose about exactness: an exact
// name must lead, the most-rated exact first, everything else in
// provider order.
func TestUnitSearch_ExactNameRanksFirst(t *testing.T) {
	env := newAuthEnv(t)
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{
			{ID: 1, Name: "Super Mario 64 DS"},
			{ID: 2, Name: "Super Mario Odyssey"},
			{ID: 3, Name: "Super Mario 64 (Shindou Pak Taiou Version)", TotalRatingCount: 12},
			{ID: 4, Name: "Super Mario 64", TotalRatingCount: 908},
		}, nil
	}}
	// Empty lane: this test is about provider rank order, not the lane.
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=Super+Mario+64", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	got := make([]int64, 0, len(res.Results))
	for _, r := range res.Results {
		got = append(got, *r.IgdbGameId)
	}
	// 4 and 3 both normalize to the query (brackets strip); rating
	// count puts the widely known release first, 1 and 2 keep provider order.
	if want := []int64{4, 3, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("rank order: got %v, want %v", got, want)
	}
}

// The provider's tokenizer misses possessive-less listing names when
// the query carries the possessive; the outgoing query drops it (verified against the live API).
func TestUnitSearch_PCQueryDropsPossessive(t *testing.T) {
	env := newAuthEnv(t)
	var gotQ string
	prices := &stubPrices{search: func(_ context.Context, q string) ([]pricecharting.Product, error) {
		gotQ = q
		return nil, nil
	}}
	h := newUnitHandlers(nil, nil, prices, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=Michael+Jackson's+Moonwalker", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if gotQ != "Michael Jackson Moonwalker" {
		t.Fatalf("provider query must drop the possessive, got %q", gotQ)
	}
}

func TestUnitSearch_PossessiveNamesRankExact(t *testing.T) {
	env := newAuthEnv(t)
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{
			{ID: 1, Name: "Moonwalker"},
			{ID: 2, Name: "Michael Jackson's Moonwalker"},
		}, nil
	}}
	// Empty lane: this test is about provider rank order, not the lane.
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=Michael+Jackson+Moonwalker", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 || *res.Results[0].IgdbGameId != 2 {
		t.Fatalf("possessive name must rank exact for the bare query: %+v", res.Results)
	}
}

func TestMatchedRegion(t *testing.T) {
	g := igdb.Game{Name: "Trials of Mana",
		AlternativeNames:  []igdb.AlternativeName{{Name: "Seiken Densetsu 3", Comment: "Japanese title - romanization"}},
		GameLocalizations: []igdb.GameLocalization{{Name: "聖剣伝説 3", Region: igdb.LocalizationRegion{Identifier: "ja-JP"}}}}
	cases := []struct{ q, want string }{
		{"Trials of Mana", ""},         // canonical wins, no annotation
		{"trials of mana", ""},         // normalized canonical
		{"Seiken Densetsu 3", "ja-JP"}, // translit exact
		{"seiken densetsu", "ja-JP"},   // containment
		{"聖剣伝説 3", "ja-JP"},            // native exact
		{"聖剣", "ja-JP"},                // native containment, 2-rune non-latin guard
		{"se", ""},                     // below 3-rune latin guard
		{"a", ""},
		{"final fantasy", ""}, // no relation
	}
	for _, tc := range cases {
		if got := matchedRegion(tc.q, g); got != tc.want {
			t.Fatalf("matchedRegion(%q) = %q want %q", tc.q, got, tc.want)
		}
	}
}

// Pins gameResult's localization mapping: the same non-empty-only
// pointer idiom toAPIProduct uses, never a pointer to an empty string.
func TestGameResult_MapsLocalizationBundles(t *testing.T) {
	g := igdb.Game{ID: 1016, Name: "Secret of Mana",
		AlternativeNames: []igdb.AlternativeName{{Name: "Seiken Densetsu 2", Comment: "Japanese title - romanization"}},
		GameLocalizations: []igdb.GameLocalization{
			{Name: "聖剣伝説2", Region: igdb.LocalizationRegion{Identifier: "ja-JP"}},
			{Region: igdb.LocalizationRegion{Identifier: "EU"}, Cover: &igdb.Cover{ImageID: "stub-eu-cover"}},
		},
	}
	res := gameResult(g)
	if res.Localizations == nil || len(*res.Localizations) != 2 {
		t.Fatalf("want the JP + EU bundles, got %+v", res.Localizations)
	}
	// Sorted by region: EU before ja-JP.
	eu, jp := (*res.Localizations)[0], (*res.Localizations)[1]
	if eu.Region != "EU" || eu.Name != nil || eu.Translit != nil {
		t.Fatalf("EU bundle must carry no name/translit pointer: %+v", eu)
	}
	wantEUCover := "https://images.igdb.com/igdb/image/upload/t_cover_big/stub-eu-cover.jpg"
	if eu.CoverUrl == nil || *eu.CoverUrl != wantEUCover {
		t.Fatalf("EU bundle cover_url: %+v", eu.CoverUrl)
	}
	if jp.Region != "ja-JP" || jp.CoverUrl != nil {
		t.Fatalf("ja-JP bundle must carry no cover pointer: %+v", jp)
	}
	if jp.Name == nil || *jp.Name != "聖剣伝説2" {
		t.Fatalf("ja-JP bundle name: %+v", jp.Name)
	}
	if jp.Translit == nil || *jp.Translit != "Seiken Densetsu 2" {
		t.Fatalf("ja-JP bundle translit: %+v", jp.Translit)
	}
}

// Pins platformReleaseRegions's contract against the Mr. Gimmick
// NES/Famicom shape, Puyo Puyo SUN's repeated regions, dedup, and edge cases.
func TestPlatformReleaseRegions(t *testing.T) {
	day := func(y int, m time.Month, d int) int64 {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
	}
	cases := []struct {
		name       string
		game       igdb.Game
		platformID int64
		want       []string
	}{
		{
			name: "Mr Gimmick shape: NES row, not folded with its Famicom twin",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(1992, time.July, 3), Platform: 18, Region: 1},  // NES europe
				{Date: day(1992, time.April, 3), Platform: 99, Region: 5}, // Famicom japan
			}},
			platformID: 18,
			want:       []string{"europe"},
		},
		{
			name: "Mr Gimmick shape: the Famicom row, not folded onto its NES twin",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(1992, time.July, 3), Platform: 18, Region: 1},
				{Date: day(1992, time.April, 3), Platform: 99, Region: 5},
			}},
			platformID: 99,
			want:       []string{"japan"},
		},
		{
			name: "Puyo Puyo SUN shape: japan-only release repeated across platforms",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(1993, time.June, 25), Platform: 200, Region: 5},
				{Date: day(1993, time.June, 25), Platform: 201, Region: 5},
				{Date: day(1993, time.June, 25), Platform: 202, Region: 5},
			}},
			platformID: 201,
			want:       []string{"japan"},
		},
		{
			name: "worldwide served verbatim",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(2010, time.May, 1), Platform: 400, Region: 8},
			}},
			platformID: 400,
			want:       []string{"worldwide"},
		},
		{
			name: "ordering: earliest date first, dateless last, ties alphabetical",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(2000, time.January, 10), Platform: 500, Region: 2}, // north_america
				{Date: day(2000, time.January, 10), Platform: 500, Region: 1}, // europe, same date
				{Date: day(2000, time.March, 1), Platform: 500, Region: 5},    // japan, later
				{Platform: 500, Region: 9},                                    // korea, dateless
				{Platform: 500, Region: 6},                                    // china, dateless
			}},
			platformID: 500,
			want:       []string{"europe", "north_america", "japan", "china", "korea"},
		},
		{
			name: "dedupe: two rows same platform+region collapse to one, earliest governs order",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(2005, time.December, 1), Platform: 600, Region: 5}, // japan, seen first, later date
				{Date: day(2005, time.January, 1), Platform: 600, Region: 5},  // japan dup, earlier date wins
				{Date: day(2005, time.June, 1), Platform: 600, Region: 1},     // europe, in between
			}},
			platformID: 600,
			want:       []string{"japan", "europe"},
		},
		{
			name: "unknown region enum dropped",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(2010, time.May, 1), Platform: 700, Region: 999},
				{Date: day(2010, time.May, 1), Platform: 700, Region: 1},
			}},
			platformID: 700,
			want:       []string{"europe"},
		},
		{
			name: "platform-0 row dropped, even when queried as platform 0",
			game: igdb.Game{ReleaseDates: []igdb.ReleaseDate{
				{Date: day(2010, time.May, 1), Platform: 0, Region: 5},
			}},
			platformID: 0,
			want:       nil,
		},
		{
			name:       "no rows",
			game:       igdb.Game{},
			platformID: 4,
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := platformReleaseRegions(tc.game, tc.platformID)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("platformReleaseRegions() = %v, want nil", got)
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("platformReleaseRegions() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Extends the exact-match float-to-top treatment to a localized title:
// a game whose own name isn't exact but whose bundle name/translit
// matches ranks with the other exacts, ahead of provider-order matches.
func TestUnitSearch_LocalizationExactNameRanksFirst(t *testing.T) {
	env := newAuthEnv(t)
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{
			{ID: 1, Name: "Trials of Mana Collection", TotalRatingCount: 500},
			{ID: 2, Name: "Trials of Mana",
				AlternativeNames:  []igdb.AlternativeName{{Name: "Seiken Densetsu 3", Comment: "Japanese title - romanization"}},
				GameLocalizations: []igdb.GameLocalization{{Name: "聖剣伝説 3", Region: igdb.LocalizationRegion{Identifier: "ja-JP"}}},
			},
		}, nil
	}}
	// Empty lane: this test is about provider rank order, not the lane.
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=Seiken+Densetsu+3", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	got := make([]int64, 0, len(res.Results))
	for _, r := range res.Results {
		got = append(got, *r.IgdbGameId)
	}
	if want := []int64{2, 1}; !slices.Equal(got, want) {
		t.Fatalf("rank order: got %v, want %v", got, want)
	}
}

// Proves the supplementary leg end to end: the query is not a
// substring of fixture 1001's canonical Name, so only the leg's match
// against the ja-JP game_localizations row surfaces the game.
func TestSearch_NonLatinQueryReachesViaLocalizationLeg(t *testing.T) {
	s := newStack(t)
	resp := s.do(http.MethodGet, "/search?type=game&q="+url.QueryEscape("ゼルダの伝説"), s.userToken(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	res := decodeBody[api.SearchResults](t, resp)
	if res.Degraded {
		t.Fatal("leg-served results must not be degraded")
	}
	if len(res.Results) != 1 || res.Results[0].IgdbGameId == nil || *res.Results[0].IgdbGameId != 1001 {
		t.Fatalf("want game 1001 via the localization leg, got %+v", res.Results)
	}
}

// Pins the physical-catalog gate on search results: digital-only
// platforms (regionkit.DigitalOnlyPlatformIDs) drop off a game's
// platform refs, and a game listed only on such platforms drops out.
// A game IGDB lists without platforms at all is unchanged.
func TestUnitSearch_DropsDigitalOnlyPlatforms(t *testing.T) {
	env := newAuthEnv(t)
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{
			{ID: 113112, Name: "Hades", Platforms: []igdb.Named{{ID: 39, Name: "iOS"}, {ID: 48, Name: "PlayStation 4"}}},
			{ID: 266203, Name: "Hades Browser Clone", Platforms: []igdb.Named{{ID: 82, Name: "Web browser"}, {ID: 34, Name: "Android"}}},
			{ID: 300000, Name: "Hades Unannounced"},
		}, nil
	}}
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=hades", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || *out.Results[0].IgdbGameId != 113112 || *out.Results[1].IgdbGameId != 300000 {
		t.Fatalf("want Hades and the platformless game only, got %+v", out.Results)
	}
	prs := *out.Results[0].Platforms
	if len(prs) != 1 || prs[0].IgdbPlatformId != 48 {
		t.Fatalf("want only the PlayStation 4 ref to survive, got %+v", prs)
	}
	if out.Results[1].Platforms != nil {
		t.Fatalf("platformless game must stay platformless, got %+v", out.Results[1].Platforms)
	}
}

// Proves the non-latin trigger gate (hasNonLatinLetter): an all-latin
// query must cost zero SearchLocalizations calls.
func TestUnitSearch_LatinQueryNeverCallsLocalizationLeg(t *testing.T) {
	env := newAuthEnv(t)
	var legCalls int
	games := &stubGames{
		searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
			return []igdb.Game{{ID: 1011, Name: "Chrono Trigger"}}, nil
		},
		searchLocalizations: func(context.Context, string, int) ([]int64, error) {
			legCalls++
			return nil, nil
		},
	}
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if legCalls != 0 {
		t.Fatalf("a latin query must never call the localization leg, got %d calls", legCalls)
	}
}

// Proves the leg's failure mode: a SearchLocalizations error must
// never degrade or fail the request (primary results still serve,
// unflagged); legCalls pins that the leg actually ran.
func TestUnitSearch_LocalizationLegErrorServesPrimaryResults(t *testing.T) {
	env := newAuthEnv(t)
	var legCalls int
	games := &stubGames{
		searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
			return []igdb.Game{{ID: 1001, Name: "The Legend of Zelda: Ocarina of Time"}}, nil
		},
		searchLocalizations: func(context.Context, string, int) ([]int64, error) {
			legCalls++
			return nil, errors.New("igdb down")
		},
	}
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q="+url.QueryEscape("ゼルダの伝説"), env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if legCalls != 1 {
		t.Fatalf("want the leg to have run once, got %d calls", legCalls)
	}
	var res api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Degraded {
		t.Fatal("a leg failure must never degrade the answer")
	}
	if len(res.Results) != 1 || res.Results[0].IgdbGameId == nil || *res.Results[0].IgdbGameId != 1001 {
		t.Fatalf("primary results must still serve: %+v", res.Results)
	}
}

func TestUnitSearch_BadParams(t *testing.T) {
	env := newAuthEnv(t)
	h := newUnitHandlers(nil, nil, nil, newStubCache())
	tok := env.token(t, "u1", []string{"user"})

	for _, path := range []string{
		"/search?type=game&q=%20",     // blank q
		"/search?type=amiibo&q=zelda", // enum violation (binding)
		"/search?type=game",           // q missing (binding)
	} {
		rec := serveUnit(t, h, env, http.MethodGet, path, tok, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", path, rec.Code)
		}
	}

	// The accepted-kinds message must list all three wire kinds (now in
	// specval's canonical "must be one of" voice).
	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=amiibo&q=zelda", tok, nil)
	p := reqtest.AssertProblemRec(t, rec, http.StatusBadRequest, "invalid_param")
	if !strings.Contains(p.Detail, "type must be one of game, hardware, pc_listing") {
		t.Fatalf("accepted-kinds message stale: %q", p.Detail)
	}
}

func TestSearch_PCListingsAllCategoriesWithPrices(t *testing.T) {
	s := newStack(t)
	resp := s.do(http.MethodGet, "/search?type=pc_listing&q=super+mario+64", s.userToken(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	out := decodeBody[api.SearchResults](t, resp)
	if out.Degraded {
		t.Fatal("stub provider must not degrade")
	}
	if len(out.Results) == 0 {
		t.Fatal("want the fixture game listing(s)")
	}
	for _, r := range out.Results {
		if string(r.Type) != "pc_listing" {
			t.Fatalf("result type %s, want pc_listing", r.Type)
		}
		if r.PcProductId == nil || r.ConsoleName == nil {
			t.Fatal("pc_listing results carry the listing id and console")
		}
		if r.LooseCents == nil || r.CibCents == nil || r.NewCents == nil {
			t.Fatal("stub listings always price; results must pass prices through")
		}
	}
	// Game listings (no hardware category) must NOT be filtered out:
	// the fixtures' Super Mario 64 game row has no genre.
	found := false
	for _, r := range out.Results {
		if *r.PcProductId == 5005 {
			found = true
		}
	}
	if !found {
		t.Fatal("game listing 5005 missing: category filter must not apply to pc_listing search")
	}
}

func TestUnitSearch_PCListingsDegradedFallsBackToMappings(t *testing.T) {
	env := newAuthEnv(t)
	st := &stubStore{}
	prices := &stubPrices{}
	prices.search = func(ctx context.Context, q string) ([]pricecharting.Product, error) {
		return nil, errors.New("provider down")
	}
	loose := int64(1100)
	st.searchByName = func(ctx context.Context, types []string, q string, limit int) ([]store.Product, error) {
		return []store.Product{{
			ID: "p1", Type: "game", Name: "Super Mario 64",
			PriceCharting: &store.PCMeta{
				PCProductID: 5005, PCName: "Super Mario 64", ConsoleName: "Nintendo 64",
				Current: store.PriceQuote{LooseCents: &loose},
			},
		}, {
			ID: "p2", Type: "game", Name: "Unmatched Game", // no PCMeta: not a listing
		}}, nil
	}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=mario", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Degraded {
		t.Fatal("provider down + cold cache must flag degraded")
	}
	if len(out.Results) != 1 {
		t.Fatalf("only mapped products can answer a degraded pc_listing search; got %d results", len(out.Results))
	}
	r := out.Results[0]
	if string(r.Type) != "pc_listing" || *r.PcProductId != 5005 || r.Name != "Super Mario 64" ||
		r.LooseCents == nil || *r.LooseCents != 1100 {
		t.Fatalf("degraded result must map the stored PCMeta, got %+v", r)
	}
}

// Pins the degraded local-fallback dedupe: a resolved game product
// and a separate pc_listing anchor product can share the same
// pc_product_id with no identity tying them together, so the fallback
// must collapse them to one row.
func TestUnitSearch_PCListingsDegradedDedupesSharedPCProductID(t *testing.T) {
	env := newAuthEnv(t)
	st := &stubStore{}
	prices := &stubPrices{}
	prices.search = func(ctx context.Context, q string) ([]pricecharting.Product, error) {
		return nil, errors.New("provider down")
	}
	loose := int64(1100)
	st.searchByName = func(ctx context.Context, types []string, q string, limit int) ([]store.Product, error) {
		return []store.Product{
			{
				ID: "game-1", Type: "game", Name: "Super Mario 64",
				PriceCharting: &store.PCMeta{
					PCProductID: 5005, PCName: "Super Mario 64", ConsoleName: "Nintendo 64",
					Current: store.PriceQuote{LooseCents: &loose},
				},
			},
			{
				ID: "listing-1", Type: "pc_listing", Name: "Super Mario 64",
				PriceCharting: &store.PCMeta{
					PCProductID: 5005, PCName: "Super Mario 64", ConsoleName: "Nintendo 64",
					Current: store.PriceQuote{LooseCents: &loose},
				},
			},
		}, nil
	}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=mario", env.token(t, "u1", []string{"user"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("degraded pc_listing results must dedupe by pc_product_id: got %d, %+v", len(out.Results), out.Results)
	}
	if *out.Results[0].PcProductId != 5005 {
		t.Fatalf("unexpected surviving row: %+v", out.Results[0])
	}
}

func TestUnitSearch_InterleavesCommunityAndKeepsCacheProviderOnly(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	now := time.Now().UTC()
	comm := store.Product{
		ID: "c0ffee00-0000-4000-8000-000000000001", Type: "game", Name: "Chrono Trigger",
		Origin: "community", Community: &store.CommunityMeta{PlatformName: "SNES", CoverURL: "https://img.example/ct.jpg", Region: "pal"},
		CreatedAt: now, UpdatedAt: now,
	}
	st := &stubStore{searchCommunityProducts: func(_ context.Context, types []string, _ string, limit int) ([]store.Product, error) {
		if limit != 10 || len(types) != 1 || types[0] != "game" {
			t.Fatalf("lane call wrong: types=%v limit=%d", types, limit)
		}
		return []store.Product{comm}, nil
	}}
	// Provider returns a weaker-scoring name so the exact community hit
	// leads the merged order.
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 4242, Name: "Chrono Cross"}}, nil
	}}
	c := newStubCache()
	h := newUnitHandlers(st, games, &stubPrices{}, c)

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono%20trigger", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("merged results = %d, want 2", len(out.Results))
	}
	// Exact community hit outscores the provider near-match and leads.
	first := out.Results[0]
	if first.Origin == nil || string(*first.Origin) != "community" || first.Name != "Chrono Trigger" {
		t.Fatalf("community item should lead: %+v", first)
	}
	if first.ProductId == nil || first.ItemType == nil || string(*first.ItemType) != "game" ||
		first.PlatformName == nil || *first.PlatformName != "SNES" ||
		first.CoverUrl == nil || *first.CoverUrl != "https://img.example/ct.jpg" {
		t.Fatalf("community pick fields missing: %+v", first)
	}
	// Community region seeds the wizard's region field straight from
	// the search result, so a community hit carries it as its own top-level fact.
	if first.Region == nil || *first.Region != "pal" {
		t.Fatalf("community row must carry region pal, got %+v", first.Region)
	}
	if out.Results[1].Origin != nil {
		t.Fatalf("provider item must have no origin: %+v", out.Results[1])
	}
	// A provider game's region data lives in localization bundles, not
	// a top-level field (a provider result may ship several regions).
	if out.Results[1].Region != nil {
		t.Fatalf("provider row must not carry region, got %+v", out.Results[1].Region)
	}

	// The cached body is provider-only: no community item leaked into cache.
	cached := c.search["game:"+normQuery("chrono trigger")]
	if cached == nil {
		// Key scheme is opaque; assert over whatever the stub stored.
		for _, v := range c.search {
			cached = v
		}
	}
	if cached == nil {
		t.Fatal("provider body was not cached")
	}
	if strings.Contains(string(cached), "community") {
		t.Fatalf("cached body must stay provider-only: %s", cached)
	}
}

// Pins the provider-wins-on-a-tie branch in interleaveCommunityResults:
// a provider result and community product scoring exactly equal must
// still place the provider row first (the sibling test only covers a near-miss).
func TestUnitSearch_InterleaveTieBreakProviderWinsEqualScore(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	now := time.Now().UTC()
	comm := store.Product{
		ID: "c0ffee00-0000-4000-8000-000000000002", Type: "game", Name: "Chrono Trigger",
		Origin: "community", CreatedAt: now, UpdatedAt: now,
	}
	st := &stubStore{searchCommunityProducts: func(_ context.Context, types []string, _ string, limit int) ([]store.Product, error) {
		return []store.Product{comm}, nil
	}}
	// Same name as the community product, so match.Score computes an
	// identical value for both: a genuine tie, not a near-miss.
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 4242, Name: "Chrono Trigger"}}, nil
	}}
	h := newUnitHandlers(st, games, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono%20trigger", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("merged results = %d, want 2", len(out.Results))
	}
	if out.Results[0].Origin != nil {
		t.Fatalf("provider row must lead an equal-score tie: %+v", out.Results[0])
	}
	if out.Results[1].Origin == nil || string(*out.Results[1].Origin) != "community" {
		t.Fatalf("community row must trail an equal-score tie: %+v", out.Results[1])
	}
}

// Pins the community lane's fail-open contract: SearchCatalog is
// otherwise entirely fail-open, so a community store fault must
// degrade to a provider-only 200, not throw away results already in out.
func TestUnitSearch_CommunityLaneErrorFailsOpen(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	st := &stubStore{searchCommunityProducts: func(context.Context, []string, string, int) ([]store.Product, error) {
		return nil, errors.New("community store down")
	}}
	games := &stubGames{searchGames: func(context.Context, string, int) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 4242, Name: "Chrono Trigger"}}, nil
	}}
	h := newUnitHandlers(st, games, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=game&q=chrono%20trigger", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("community lane error: %d %s, want 200 (fail open)", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Name != "Chrono Trigger" {
		t.Fatalf("provider results must survive a community fault: %+v", out.Results)
	}
	if out.Results[0].Origin != nil {
		t.Fatalf("no community row when the lane errors: %+v", out.Results[0])
	}
}

// Pins interleaveCommunityResults's hardware branch: a hardware-kind
// search scopes the lane to [console accessory], never the bare "hardware" wire kind.
func TestUnitSearch_CommunityLaneHardwareTypes(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	var gotTypes []string
	st := &stubStore{searchCommunityProducts: func(_ context.Context, types []string, _ string, limit int) ([]store.Product, error) {
		gotTypes = types
		if limit != 10 {
			t.Fatalf("lane limit = %d, want 10", limit)
		}
		return nil, nil
	}}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=hardware&q=repro", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(gotTypes, []string{"console", "accessory"}) {
		t.Fatalf("hardware lane types = %v, want [console accessory]", gotTypes)
	}
}

func TestUnitSearch_NoLaneForPCListings(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	// searchCommunityProducts left nil: reaching the lane would panic.
	st := &stubStore{}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=repro", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Origin != nil {
			t.Fatalf("pc_listing search must carry no community items: %+v", r)
		}
	}
}

// The cache-hit twin of TestUnitSearch_NoLaneForPCListings: the
// pc_listing branch in interleaveCommunityResults is reached from two
// SearchCatalog call sites (cache hit vs fresh search); this pins the
// cache-hit one, proving neither the provider nor community store is touched.
func TestUnitSearch_NoLaneForPCListingsOnCacheHit(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	c := newStubCache()
	st := &stubStore{}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) { return nil, nil }}
	h := newUnitHandlers(st, &stubGames{}, prices, c)

	rec := serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=repro", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search (miss): %d %s", rec.Code, rec.Body.String())
	}
	if c.puts == 0 {
		t.Fatal("first request must have primed the cache")
	}

	// Poison the provider: a cache-hit search must never reach it.
	h.prices = &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return nil, errors.New("provider must not be called on a cache hit")
	}}
	rec = serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=repro", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search (hit): %d %s", rec.Code, rec.Body.String())
	}
	var out api.SearchResults
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Degraded {
		t.Fatal("cache-hit search must not degrade; the provider must not have been called")
	}
	for _, r := range out.Results {
		if r.Origin != nil {
			t.Fatalf("pc_listing cache-hit must carry no community items: %+v", r)
		}
	}
}
