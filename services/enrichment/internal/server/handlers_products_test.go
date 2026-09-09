// Tests for product identity: the read path, resolve, and the admin
// mapping and delete levers.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/levonn-dev/vgkeep/libs/go/reqtest"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/gen/api"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/igdb"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/pricecharting"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/store"
)

func TestUnitGetProduct_NotFoundAndCacheHit(t *testing.T) {
	env := newAuthEnv(t)
	c := newStubCache()
	st := &stubStore{getProduct: func(_ context.Context, id string) (store.Product, error) {
		return store.Product{}, store.ErrNotFound
	}}
	h := newUnitHandlers(st, nil, nil, c)
	tok := env.token(t, "u1", []string{"user"})

	rec := serveUnit(t, h, env, http.MethodGet, "/products/33333333-3333-3333-3333-333333333333", tok, nil)
	reqtest.AssertProblemRec(t, rec, http.StatusNotFound, "product_not_found")

	// A cached body short-circuits the store entirely.
	c.prods["44444444-4444-4444-4444-444444444444"] = []byte(`{"id":"44444444-4444-4444-4444-444444444444"}`)
	st.getProduct = func(context.Context, string) (store.Product, error) { panic("store must not be hit on a cache hit") }
	rec = serveUnit(t, h, env, http.MethodGet, "/products/44444444-4444-4444-4444-444444444444", tok, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("44444444")) {
		t.Fatalf("cache hit: %d %s", rec.Code, rec.Body.String())
	}

	// Malformed uuid is a binding 400, not a store call.
	rec = serveUnit(t, h, env, http.MethodGet, "/products/not-a-uuid", tok, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: %d", rec.Code)
	}
}

func TestUnitGetProduct_StaleIGDBRefetchAndStaleServeOnError(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	old := time.Now().Add(-1000 * time.Hour)
	prod := store.Product{
		ID: "55555555-5555-5555-5555-555555555555", Type: "game", Name: "Chrono Trigger",
		IGDB: &store.IGDBMeta{GameID: 1011, Name: "Chrono Trigger", FetchedAt: old,
			Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}},
	}

	// Provider answers: the projection refreshes and is persisted.
	var setCalled bool
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return prod, nil },
		upsertRaw:  func(context.Context, []igdb.Game, time.Time) error { return nil },
		setIGDB: func(_ context.Context, id string, m store.IGDBMeta) error {
			setCalled = true
			if m.Name != "Chrono Trigger DS" {
				t.Fatalf("refetched projection: %+v", m)
			}
			return nil
		},
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger DS"}}, nil
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())
	rec := serveUnit(t, h, env, http.MethodGet, "/products/"+prod.ID, tok, nil)
	if rec.Code != http.StatusOK || !setCalled || !bytes.Contains(rec.Body.Bytes(), []byte("Chrono Trigger DS")) {
		t.Fatalf("stale refetch: %d setCalled=%v %s", rec.Code, setCalled, rec.Body.String())
	}

	// Provider down: the stale copy serves.
	games.gamesByIDs = func(context.Context, []int64) ([]igdb.Game, error) { return nil, errors.New("igdb down") }
	h2 := newUnitHandlers(st, games, nil, newStubCache())
	rec = serveUnit(t, h2, env, http.MethodGet, "/products/"+prod.ID, tok, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("Chrono Trigger")) {
		t.Fatalf("stale serve: %d %s", rec.Code, rec.Body.String())
	}
}

// Pins refreshIGDBIfStale's defensive nil-Platform branch: a stale
// product missing its platform ref (not reachable today, but
// documented) still refetches without a nil dereference, scoping the
// platform-id-0 release table to nothing and falling back to the game-level scalar.
func TestUnitGetProduct_StaleRefetchNilPlatformRebuildsAtPlatformZero(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	old := time.Now().Add(-1000 * time.Hour)
	scalar := time.Date(1995, time.March, 11, 0, 0, 0, 0, time.UTC)
	prod := store.Product{
		ID: "99999999-9999-9999-9999-999999999999", Type: "game", Name: "Chrono Trigger",
		// Platform deliberately nil.
		IGDB: &store.IGDBMeta{GameID: 1011, Name: "Chrono Trigger (stale)", FetchedAt: old},
	}

	var setCalled bool
	var setMeta store.IGDBMeta
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return prod, nil },
		upsertRaw:  func(context.Context, []igdb.Game, time.Time) error { return nil },
		setIGDB: func(_ context.Context, id string, m store.IGDBMeta) error {
			setCalled = true
			setMeta = m
			return nil
		},
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{
			ID: 1011, Name: "Chrono Trigger", FirstReleaseDate: scalar.Unix(),
			// A dated row for a real platform (19); platform id 0 (the
			// nil-Platform fallback) matches none of it.
			ReleaseDates: []igdb.ReleaseDate{{Date: scalar.Unix(), Platform: 19, Region: 5}},
		}}, nil
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/products/"+prod.ID, tok, nil)
	if rec.Code != http.StatusOK || !setCalled {
		t.Fatalf("nil-platform stale refetch: %d setCalled=%v %s", rec.Code, setCalled, rec.Body.String())
	}
	if setMeta.Name != "Chrono Trigger" {
		t.Fatalf("refetched projection must land: %+v", setMeta)
	}
	if setMeta.ReleaseDates == nil || len(setMeta.ReleaseDates) != 0 {
		t.Fatalf("platform id 0 must scope to an empty, non-nil release table: %#v", setMeta.ReleaseDates)
	}
	if !setMeta.FirstReleaseDate.Equal(scalar) {
		t.Fatalf("game-level scalar must survive with no platform rows: %v", setMeta.FirstReleaseDate)
	}
}

func TestUnitGetProduct_FreshIGDBSkipsProviderRefetch(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	fresh := time.Now().Add(-1 * time.Hour) // well inside the 720h window
	prod := store.Product{
		ID: "66666666-6666-6666-6666-666666666666", Type: "game", Name: "Chrono Trigger",
		IGDB: &store.IGDBMeta{GameID: 1011, Name: "Chrono Trigger", FetchedAt: fresh,
			Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}},
	}
	st := &stubStore{getProduct: func(context.Context, string) (store.Product, error) { return prod, nil }}
	// gamesByIDs/upsertRaw/setIGDB left nil: a fresh product must not
	// touch the provider or store writes; unset stubs panic if called.
	games := &stubGames{}
	h := newUnitHandlers(st, games, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/products/"+prod.ID, tok, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("Chrono Trigger")) {
		t.Fatalf("fresh serve: %d %s", rec.Code, rec.Body.String())
	}
}

// Pins toAPIProduct's release_dates projection: a populated table
// serves as rows; an empty (fetched-none) table serves the field absent, not [].
func TestUnitGetProduct_ReleaseDatesMapped(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	fresh := time.Now().Add(-1 * time.Hour) // well inside the 720h window
	day := time.Date(2003, time.September, 12, 0, 0, 0, 0, time.UTC)

	dated := store.Product{
		ID: "77777777-7777-7777-7777-777777777777", Type: "game", Name: "Chrono Trigger",
		IGDB: &store.IGDBMeta{GameID: 1011, Name: "Chrono Trigger", FetchedAt: fresh,
			ReleaseDates: []store.MetaReleaseDate{{Region: "japan", Date: day}}},
	}
	st := &stubStore{getProduct: func(context.Context, string) (store.Product, error) { return dated, nil }}
	// gamesByIDs is left nil: FetchedAt is fresh, so a correct mapping
	// test never touches the provider.
	h := newUnitHandlers(st, &stubGames{}, nil, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/products/"+dated.ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get product: %d %s", rec.Code, rec.Body.String())
	}
	var p api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Igdb == nil || p.Igdb.ReleaseDates == nil || len(*p.Igdb.ReleaseDates) != 1 {
		t.Fatalf("release_dates not mapped: %+v", p.Igdb)
	}
	rd := (*p.Igdb.ReleaseDates)[0]
	if rd.Region != "japan" || !rd.Date.Equal(day) {
		t.Fatalf("release_dates row: %+v", rd)
	}

	// An empty table (fetched, IGDB lists no dated rows for the
	// platform) must serve the field absent, not an empty array.
	none := dated
	none.ID = "88888888-8888-8888-8888-888888888888"
	none.IGDB = &store.IGDBMeta{GameID: 1011, Name: "Chrono Trigger", FetchedAt: fresh, ReleaseDates: []store.MetaReleaseDate{}}
	st.getProduct = func(context.Context, string) (store.Product, error) { return none, nil }
	rec = serveUnit(t, h, env, http.MethodGet, "/products/"+none.ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get product: %d %s", rec.Code, rec.Body.String())
	}
	var p2 api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &p2); err != nil {
		t.Fatal(err)
	}
	if p2.Igdb == nil {
		t.Fatal("igdb projection missing entirely")
	}
	if p2.Igdb.ReleaseDates != nil {
		t.Fatalf("empty release_dates must serve absent, got %+v", *p2.Igdb.ReleaseDates)
	}
}

func TestResolve_GameCreatesMatchedProductIdempotently(t *testing.T) {
	s := newStack(t)

	// Chrono Trigger (fixture 1011) on SNES (19).
	p := s.resolveGame(1011, 19)
	if string(p.Type) != "game" || p.Name != "Chrono Trigger" {
		t.Fatalf("product: %+v", p)
	}
	if p.Igdb == nil || p.Igdb.GameId != 1011 || len(p.Igdb.SimilarGames) == 0 {
		t.Fatalf("igdb projection: %+v", p.Igdb)
	}
	wantDate := time.Date(1995, time.March, 11, 0, 0, 0, 0, time.UTC)
	if p.Igdb.FirstReleaseDate == nil || !p.Igdb.FirstReleaseDate.Equal(wantDate) {
		t.Fatalf("igdb first_release_date: %+v", p.Igdb.FirstReleaseDate)
	}
	if p.Platform == nil || p.Platform.IgdbPlatformId != 19 {
		t.Fatalf("platform: %+v", p.Platform)
	}
	if p.Pricecharting == nil || p.Pricecharting.PcProductId != 5011 || p.Pricecharting.Verified {
		t.Fatalf("auto-match must map to 5011 unverified: %+v", p.Pricecharting)
	}
	if p.Pricecharting.MatchConfidence < 0.75 || p.Pricecharting.LooseCents == nil {
		t.Fatalf("match confidence/prices: %+v", p.Pricecharting)
	}

	// Same identity resolves to the same product (found path).
	again := s.resolveGame(1011, 19)
	if again.Id != p.Id {
		t.Fatalf("resolve must be idempotent: %s vs %s", again.Id, p.Id)
	}

	// The initial snapshot landed.
	ctx := context.Background()
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM price_snapshots WHERE product_id = $1", p.Id.String()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("initial snapshot: %d, %v", n, err)
	}
	// And the raw payload is shared state for recommendations.
	raws, err := s.store.RawByIDs(ctx, []int64{1011})
	if err != nil || len(raws) != 1 {
		t.Fatalf("igdb_raw: %d, %v", len(raws), err)
	}
}

// Pins toAPIProduct's localizations projection: fixture game 1001
// (Ocarina of Time) has a ja-JP game_localizations row merged with an
// alternative_names romanization tag; the bundle must serve unchanged.
func TestResolve_GameLocalizationsMapped(t *testing.T) {
	s := newStack(t)

	// Ocarina of Time (fixture 1001) on Nintendo 64 (4).
	p := s.resolveGame(1001, 4)
	if p.Igdb == nil || p.Igdb.Localizations == nil || len(*p.Igdb.Localizations) != 1 {
		t.Fatalf("localizations not mapped: %+v", p.Igdb)
	}
	loc := (*p.Igdb.Localizations)[0]
	if loc.Region != "ja-JP" {
		t.Fatalf("localization region: %+v", loc)
	}
	if loc.Name == nil || *loc.Name != "ゼルダの伝説 時のオカリナ" {
		t.Fatalf("localization name: %+v", loc)
	}
	if loc.Translit == nil || *loc.Translit != "Zelda no Densetsu: Toki no Ocarina" {
		t.Fatalf("localization translit: %+v", loc)
	}
	wantCover := "https://images.igdb.com/igdb/image/upload/t_cover_big/stub-oot-jp.jpg"
	if loc.CoverUrl == nil || *loc.CoverUrl != wantCover {
		t.Fatalf("localization cover_url: %+v", loc)
	}
}

// A JP-region no-pick resolve queries by translit and lands the Super
// Famicom listing for a SNES pick (JP twin gate), forking a sibling member.
func TestResolve_RegionJPLandsJPListing(t *testing.T) {
	s := newStack(t)

	// Secret of Mana (fixture 1016/SNES 19): ja-JP name "Seiken Densetsu
	// 2" aligns with the Super Famicom fixture listing 5101.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1016, "platform_igdb_id": 19, "region": "ntsc_j",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("ntsc_j resolve: %d %s", resp.StatusCode, b)
	}
	jp := decodeBody[api.Product](t, resp)
	if jp.Pricecharting == nil || jp.Pricecharting.PcProductId != 5101 {
		t.Fatalf("ntsc_j must land the Super Famicom listing 5101: %+v", jp.Pricecharting)
	}
	if jp.Pricecharting.ConsoleName != "Super Famicom" {
		t.Fatalf("ntsc_j console_name: %q", jp.Pricecharting.ConsoleName)
	}

	resp = s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1016, "platform_igdb_id": 19, "region": "ntsc_u",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("ntsc_u resolve: %d %s", resp.StatusCode, b)
	}
	base := decodeBody[api.Product](t, resp)
	if base.Pricecharting == nil {
		t.Fatalf("ntsc_u must land the base listing: %+v", base.Pricecharting)
	}
	if base.Pricecharting.ConsoleName != "Super Nintendo" {
		t.Fatalf("ntsc_u console_name: %q", base.Pricecharting.ConsoleName)
	}
	if base.Pricecharting.PcProductId == jp.Pricecharting.PcProductId || base.Id == jp.Id {
		t.Fatalf("ntsc_j and ntsc_u must fork distinct sibling members: %+v vs %+v", base.Pricecharting, jp.Pricecharting)
	}
}

// A pal resolve with no PAL listing in the provider stays unmatched -
// strict class acceptance never falls back to the NA listing.
func TestResolve_RegionPALWithoutListingStaysUnmatched(t *testing.T) {
	s := newStack(t)

	// Chrono Trigger (fixture 1011) on SNES (19) has a base listing
	// (5011) but no PAL row anywhere in the fixtures.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19, "region": "pal",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pal resolve: %d %s", resp.StatusCode, b)
	}
	p := decodeBody[api.Product](t, resp)
	if p.Pricecharting != nil {
		t.Fatalf("pal resolve without a PAL listing must stay unmatched: %+v", p.Pricecharting)
	}
}

// Region is a matching input only: the picker path ignores it, and an
// unknown region value behaves as base.
func TestResolve_RegionIgnoredOnPickerPathAndUnknownIsBase(t *testing.T) {
	s := newStack(t)

	// Picker path: the chosen listing wins regardless of region.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19, "pc_product_id": 5011, "region": "ntsc_j",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("picker resolve: %d %s", resp.StatusCode, b)
	}
	picked := decodeBody[api.Product](t, resp)
	if picked.Pricecharting == nil || picked.Pricecharting.PcProductId != 5011 {
		t.Fatalf("the picked listing must win regardless of region: %+v", picked.Pricecharting)
	}

	// Unknown region: byte-equal to a regionless resolve. A throwaway
	// resolve creates the product first so both calls below take the
	// find path (a fresh create's timestamps never byte-match a found doc's).
	warm := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1016, "platform_igdb_id": 19,
	})
	if warm.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(warm.Body)
		t.Fatalf("warm resolve: %d %s", warm.StatusCode, b)
	}
	_, _ = io.ReadAll(warm.Body)
	_ = warm.Body.Close()

	respNone := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1016, "platform_igdb_id": 19,
	})
	if respNone.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respNone.Body)
		t.Fatalf("regionless resolve: %d %s", respNone.StatusCode, b)
	}
	noneBody, err := io.ReadAll(respNone.Body)
	_ = respNone.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	respUnknown := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1016, "platform_igdb_id": 19, "region": "someday_region",
	})
	if respUnknown.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respUnknown.Body)
		t.Fatalf("unknown-region resolve: %d %s", respUnknown.StatusCode, b)
	}
	unknownBody, err := io.ReadAll(respUnknown.Body)
	_ = respUnknown.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(noneBody, unknownBody) {
		t.Fatalf("unknown region must behave byte-identically to no region:\n%s\nvs\n%s", noneBody, unknownBody)
	}
}

// The fallback leg: a JP resolve whose translit query surfaces
// nothing admitted re-searches by canonical name and hits the hybrid JP listing.
func TestResolve_RegionFallbackSearchFindsHybridListing(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	// Mega Man 2 (fixture 1024/NES 18): ja-JP "Rockman 2" matches no
	// listing, but canonical "Mega Man 2" hits Famicom fixture 5105.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1024, "platform_igdb_id": 18, "region": "ntsc_j",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("resolve: %d %s", resp.StatusCode, b)
	}
	p := decodeBody[api.Product](t, resp)
	if p.Pricecharting == nil || p.Pricecharting.PcProductId != 5105 {
		t.Fatalf("fallback leg must land the Famicom listing 5105: %+v", p.Pricecharting)
	}
	if p.Pricecharting.ConsoleName != "Famicom" {
		t.Fatalf("console_name: %q", p.Pricecharting.ConsoleName)
	}

	for _, q := range []string{"Rockman 2", "Mega Man 2"} {
		body, err := s.h.cache.GetSearch(ctx, "pc_listing", normQuery(q))
		if err != nil {
			t.Fatalf("GetSearch(%q): %v", q, err)
		}
		if body == nil {
			t.Fatalf("both legs must ride the pc_listing search cache; %q is missing", q)
		}
	}
}

// Pins gamePayloadFor's self-heal: a raw doc predating this feature
// (no release_dates key, decodes nil) fails the read, so one refetch
// repairs it. A raw already normalized to the empty-but-fetched
// marker satisfies the read and skips the provider entirely.
func TestResolve_HealsPreFeatureRawReleaseDates(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	const platformID = 8801

	// Bypass UpsertRaw's normalization: insert directly, omitting
	// release_dates so it decodes nil, not the fetched-but-none marker.
	const staleGameID = 93340
	game, err := json.Marshal(map[string]any{
		"id":        staleGameID,
		"name":      "Pre-Feature Game",
		"platforms": []map[string]any{{"id": platformID, "name": "Test Platform"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO igdb_raw (id, game, fetched_at, fields_version) VALUES ($1,$2,$3,$4)`,
		int64(staleGameID), game, time.Now().UTC(), 0); err != nil {
		t.Fatal(err)
	}

	var calls int
	healedDate := time.Date(2001, time.June, 20, 0, 0, 0, 0, time.UTC)
	s.h.games = &stubGames{
		gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
			calls++
			return []igdb.Game{{
				ID:        staleGameID,
				Name:      "Pre-Feature Game",
				Platforms: []igdb.Named{{ID: platformID, Name: "Test Platform"}},
				ReleaseDates: []igdb.ReleaseDate{
					{Date: healedDate.Unix(), Platform: platformID, Region: 5}, // japan
				},
			}}, nil
		},
		// Platform catalog is cold on a fresh container; platformLogoFor
		// triggers one refresh here (best-effort). A non-empty answer lets
		// UpsertPlatforms warm the catalog so later resolves don't re-trigger it.
		platforms: func(context.Context) ([]igdb.Platform, error) {
			return []igdb.Platform{{ID: platformID, Name: "Test Platform"}}, nil
		},
	}

	p := s.resolveGame(staleGameID, platformID)
	if calls != 1 {
		t.Fatalf("nil-table raw must trigger exactly one refetch, got %d calls", calls)
	}
	if p.Igdb == nil || p.Igdb.ReleaseDates == nil || len(*p.Igdb.ReleaseDates) != 1 {
		t.Fatalf("healed release_dates missing: %+v", p.Igdb)
	}
	rd := (*p.Igdb.ReleaseDates)[0]
	if rd.Region != "japan" || !rd.Date.Equal(healedDate) {
		t.Fatalf("healed release_date row: %+v", rd)
	}

	// Counter-case: a raw doc UpsertRaw already normalized (fetched,
	// IGDB listed no dated rows for the platform) must NOT refetch.
	const freshGameID = 93341
	if err := s.store.UpsertRaw(ctx, []igdb.Game{{
		ID:           freshGameID,
		Name:         "Fetched But None",
		Platforms:    []igdb.Named{{ID: platformID, Name: "Test Platform"}},
		ReleaseDates: []igdb.ReleaseDate{},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Seed the platform catalog directly rather than relying on the
	// case above having warmed it: this case must hold standalone, or
	// platformLogoFor hits a cold catalog and panics on h.games.Platforms.
	if err := s.store.UpsertPlatforms(ctx, []igdb.Platform{{ID: platformID, Name: "Test Platform"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.h.games = &stubGames{} // panics if GamesByIDs or Platforms is called
	p2 := s.resolveGame(freshGameID, platformID)
	if p2.Igdb == nil || p2.Igdb.GameId != freshGameID {
		t.Fatalf("fetched-but-none raw must resolve without a refetch: %+v", p2.Igdb)
	}
	if p2.Igdb.ReleaseDates != nil {
		t.Fatalf("no dated rows for the platform must serve release_dates absent: %+v", *p2.Igdb.ReleaseDates)
	}
}

// Pins gamePayloadFor's version-based heal: a raw doc with a real
// release table but predating fields_version (and the localization
// arrays that generation added) is still a miss; one refetch repairs it.
func TestResolve_HealsBelowVersionRaw(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	const gid = 424243
	const platformID = 8802
	naDate := time.Date(2003, time.September, 9, 0, 0, 0, 0, time.UTC)

	// Hand-write the raw row: release_dates is real, but fields_version
	// and the newer-generation localization arrays are absent (the below-version case).
	game, err := json.Marshal(map[string]any{
		"id":            gid,
		"name":          "Regional Quest II",
		"platforms":     []map[string]any{{"id": platformID, "name": "Test Platform"}},
		"release_dates": []map[string]any{{"date": naDate.Unix(), "platform": platformID, "release_region": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO igdb_raw (id, game, fetched_at, fields_version) VALUES ($1,$2,$3,$4)`,
		int64(gid), game, time.Now().UTC(), 0); err != nil {
		t.Fatal(err)
	}
	// Pre-warm the platform catalog: platformLogoFor otherwise reaches
	// for h.games.Platforms, which this stub doesn't carry.
	if err := s.store.UpsertPlatforms(ctx, []igdb.Platform{{ID: platformID, Name: "Test Platform"}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	var calls int
	s.h.games = &stubGames{gamesByIDs: func(_ context.Context, ids []int64) ([]igdb.Game, error) {
		calls++
		if len(ids) != 1 || ids[0] != gid {
			t.Fatalf("unexpected refetch ids: %v", ids)
		}
		return []igdb.Game{{
			ID:        gid,
			Name:      "Regional Quest II",
			Platforms: []igdb.Named{{ID: platformID, Name: "Test Platform"}},
			ReleaseDates: []igdb.ReleaseDate{
				{Date: naDate.Unix(), Platform: platformID, Region: 2}, // north_america
			},
			GameLocalizations: []igdb.GameLocalization{
				{Name: "地域限定クエスト", Region: igdb.LocalizationRegion{Identifier: "ja-JP"}},
			},
		}}, nil
	}}

	p := s.resolveGame(gid, platformID)
	if calls != 1 {
		t.Fatalf("below-version raw must trigger exactly one refetch, got %d", calls)
	}
	if p.Igdb == nil || p.Igdb.Localizations == nil || len(*p.Igdb.Localizations) != 1 {
		t.Fatalf("minted product must carry the refetched localizations: %+v", p.Igdb)
	}
}

func TestResolve_GameManualMatchMintsExactAnchor(t *testing.T) {
	s := newStack(t)
	// 6001 is the Super Nintendo System fixture, a listing auto-match
	// would never pick for Chrono Trigger (maps to 5011): proves the manual path ran.
	body := map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19, "pc_product_id": 6001}
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp.StatusCode)
	}
	p := decodeBody[api.Product](t, resp)
	if p.Pricecharting == nil || p.Pricecharting.PcProductId != 6001 {
		t.Fatalf("manual match must win over auto-match: %+v", p.Pricecharting)
	}
	if p.Pricecharting.MatchConfidence != 1.0 || p.Pricecharting.Verified {
		t.Fatalf("exact but unverified: %+v", p.Pricecharting)
	}
	if p.Igdb == nil || p.Igdb.GameId != 1011 {
		t.Fatalf("still a full game product: %+v", p.Igdb)
	}
	var n int64
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM price_snapshots WHERE product_id = $1", p.Id.String()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("initial snapshot: %d, %v", n, err)
	}

	// Idempotent: the same manual resolve converges on the same doc
	// and appends nothing.
	again := decodeBody[api.Product](t, s.do(http.MethodPost, "/products/resolve", s.userToken(), body))
	if again.Id != p.Id {
		t.Fatalf("must converge: %s vs %s", again.Id, p.Id)
	}
	_ = s.pool.QueryRow(context.Background(), "SELECT count(*) FROM price_snapshots WHERE product_id = $1", p.Id.String()).Scan(&n)
	if n != 1 {
		t.Fatalf("repeat resolve must not snapshot again, got %d", n)
	}
}

// Listing-keyed identity end to end: no-pick resolves converge; a
// manual pick of a different listing is a distinct family member; region/edition/variant are ignored.
func TestResolve_GameConvergesPerListing(t *testing.T) {
	s := newStack(t)

	auto1 := s.resolveGame(1005, 4)
	auto2 := s.resolveGame(1005, 4)
	if auto1.Id != auto2.Id {
		t.Fatalf("no-pick resolves must converge: %s vs %s", auto1.Id, auto2.Id)
	}
	if auto1.Pricecharting == nil || auto1.Pricecharting.PcProductId != 5005 {
		t.Fatalf("auto-match must land the base listing: %+v", auto1.Pricecharting)
	}

	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1005, "platform_igdb_id": 4,
		"pc_product_id": 5099, "variant": "players choice cart",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("picker resolve: %d", resp.StatusCode)
	}
	picked := decodeBody[api.Product](t, resp)
	if picked.Id == auto1.Id {
		t.Fatal("a different listing must be a distinct member")
	}
	if picked.Pricecharting == nil || picked.Pricecharting.PcProductId != 5099 ||
		picked.Pricecharting.MatchConfidence != 1.0 || picked.Pricecharting.Verified {
		t.Fatalf("picked member mapping: %+v", picked.Pricecharting)
	}
	if picked.Igdb == nil || auto1.Igdb == nil || picked.Igdb.GameId != auto1.Igdb.GameId {
		t.Fatal("members must share the igdb family")
	}

	resp = s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1005, "platform_igdb_id": 4, "pc_product_id": 5099,
	})
	again := decodeBody[api.Product](t, resp)
	if again.Id != picked.Id {
		t.Fatalf("same pick must converge: %s vs %s", again.Id, picked.Id)
	}
}

// A hint no candidate carries keeps the resolve conservative: it
// lands on the family's unmatched member (both times); a plain resolve still lands matched.
func TestResolve_MatchHintBelowThresholdLandsUnmatchedMember(t *testing.T) {
	s := newStack(t)

	body := map[string]any{
		"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19,
		"match_hint": "grey cart brick serial",
	}
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hinted resolve: %d", resp.StatusCode)
	}
	miss1 := decodeBody[api.Product](t, resp)
	if miss1.Pricecharting != nil {
		t.Fatalf("junk hint must not guess: %+v", miss1.Pricecharting)
	}
	resp = s.do(http.MethodPost, "/products/resolve", s.userToken(), body)
	miss2 := decodeBody[api.Product](t, resp)
	if miss2.Id != miss1.Id {
		t.Fatalf("unmatched member must be the family singleton: %s vs %s", miss2.Id, miss1.Id)
	}

	matched := s.resolveGame(1011, 19)
	if matched.Id == miss1.Id || matched.Pricecharting == nil || matched.Pricecharting.PcProductId != 5011 {
		t.Fatalf("plain resolve must land the matched member beside it: %+v", matched)
	}
}

func TestResolve_UnmatchedFixtureStaysUnmatched(t *testing.T) {
	s := newStack(t)
	// Terranigma (1018, SNES) deliberately has no PriceCharting
	// fixture: the below-threshold path stores an unmatched product.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1018, "platform_igdb_id": 19,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp.StatusCode)
	}
	p := decodeBody[api.Product](t, resp)
	if p.Pricecharting != nil {
		t.Fatalf("Terranigma must stay unmatched: %+v", p.Pricecharting)
	}
	// Unmatched means no snapshot either.
	var n int64
	_ = s.pool.QueryRow(context.Background(), "SELECT count(*) FROM price_snapshots WHERE product_id = $1", p.Id.String()).Scan(&n)
	if n != 0 {
		t.Fatalf("unmatched products must not snapshot, got %d", n)
	}
}

func TestResolve_HardwareBorrowsPlatform(t *testing.T) {
	s := newStack(t)
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "console", "pc_product_id": 6001,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve hardware: %d", resp.StatusCode)
	}
	p := decodeBody[api.Product](t, resp)
	if string(p.Type) != "console" || p.Name != "Super Nintendo System" {
		t.Fatalf("hardware product: %+v", p)
	}
	if p.Pricecharting == nil || p.Pricecharting.PcProductId != 6001 || p.Pricecharting.MatchConfidence != 1.0 {
		t.Fatalf("hardware mapping: %+v", p.Pricecharting)
	}
	if p.Igdb != nil {
		t.Fatal("hardware carries no igdb subdoc")
	}
	// "Super Nintendo" (PriceCharting) borrows IGDB platform 19.
	if p.Platform == nil || p.Platform.IgdbPlatformId != 19 {
		t.Fatalf("platform borrow: %+v", p.Platform)
	}
}

func TestResolve_ErrorTaxonomy(t *testing.T) {
	s := newStack(t)
	cases := []struct {
		name string
		body map[string]any
		code int
		want string
	}{
		{"unknown game", map[string]any{"type": "game", "igdb_game_id": 999999, "platform_igdb_id": 19}, http.StatusNotFound, "unknown_game"},
		{"wrong platform", map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 130}, http.StatusBadRequest, "invalid_body"},
		{"unknown hardware", map[string]any{"type": "console", "pc_product_id": 999999}, http.StatusNotFound, "unknown_pc_product"},
		{"missing game ids", map[string]any{"type": "game"}, http.StatusBadRequest, "invalid_body"},
		{"missing pc id", map[string]any{"type": "accessory"}, http.StatusBadRequest, "invalid_body"},
		{"bad type", map[string]any{"type": "amiibo"}, http.StatusBadRequest, "invalid_body"},
	}
	for _, tc := range cases {
		resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), tc.body)
		reqtest.AssertProblem(t, resp, tc.code, tc.want)
	}
}

func TestUnitResolve_UpstreamDown(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		// gamePayloadFor checks igdb_raw first; an empty read falls
		// through to the provider, which is the one erroring here.
		rawByIDs: func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return nil, errors.New("igdb down")
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19})
	reqtest.AssertProblemRec(t, rec, http.StatusBadGateway, "upstream_unavailable")
}

// Pins the physical-catalog gate on resolve: a game's own platform
// list can carry a digital-only platform (regionkit.DigitalOnlyPlatformIDs),
// and resolving onto it is rejected before anything is minted.
func TestUnitResolve_DigitalOnlyPlatformRejected(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs:  func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw: func(context.Context, []igdb.Game, time.Time) error { return nil },
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 119277, Name: "Genshin Impact",
			Platforms: []igdb.Named{{ID: 39, Name: "iOS"}, {ID: 48, Name: "PlayStation 4"}}}}, nil
	}}
	h := newUnitHandlers(st, games, nil, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 119277, "platform_igdb_id": 39})
	pb := reqtest.AssertProblemRec(t, rec, http.StatusBadRequest, "invalid_body")
	if !strings.Contains(pb.Detail, "physical") {
		t.Fatalf("detail must name the physical-only gate, got %q", pb.Detail)
	}
}

// A pre-feature raw (nil release table) with the provider down is
// still usable stale (misses only per-region dates), matching the
// read path's serve-stale posture; the nightly reprojection heals it
// later. Counterpart to TestUnitResolve_UpstreamDown (no raw -> error).
func TestUnitResolve_PreFeatureRawServesStaleWhenProviderDown(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	stale := igdb.Game{
		ID: 1011, Name: "Chrono Trigger",
		Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}},
		// ReleaseDates nil: the pre-feature sentinel.
	}
	var created store.Product
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs: func(context.Context, []int64) ([]store.RawGame, error) {
			return []store.RawGame{{GameID: 1011, Game: stale, FetchedAt: time.Unix(1000, 0).UTC()}}, nil
		},
		// upsertRaw left nil: serving stale must not refetch or re-upsert.
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
			p.ID = "77777777-7777-7777-7777-777777777777"
			created = p
			return p, nil
		},
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System"}}, nil
		},
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return nil, errors.New("igdb down") // refetch fails; the stale raw must be served
	}}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return nil, errors.New("pricecharting down") // auto-match lands on the unmatched member
	}}
	h := newUnitHandlers(st, games, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19})
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-feature raw must resolve from the stale payload when the provider is down: %d %s", rec.Code, rec.Body.String())
	}
	if created.IGDB == nil || created.IGDB.GameID != 1011 {
		t.Fatalf("stale payload must build the projection: %+v", created.IGDB)
	}
	if !created.IGDB.FetchedAt.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("stale projection must keep the raw's own stamp: %v", created.IGDB.FetchedAt)
	}
	if len(created.IGDB.ReleaseDates) != 0 {
		t.Fatalf("a pre-feature stale payload has no dated rows: %+v", created.IGDB.ReleaseDates)
	}
}

// Pins the stale-serve arm for a below-fields_version raw (a real
// release table, but the nil-table check alone would treat it as a
// hit): the provider being down for the repair attempt must still serve it.
func TestUnitResolve_BelowVersionRawServesStaleWhenProviderDown(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	stale := igdb.Game{
		ID:        1011,
		Name:      "Chrono Trigger",
		Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}},
		ReleaseDates: []igdb.ReleaseDate{
			{Date: 809049600, Platform: 19, Region: 2}, // real table: not the nil-table case
		},
		// FieldsVersion left at the RawGame literal's zero value below:
		// below store.RawFieldsVersion, the case under test.
	}
	var created store.Product
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs: func(context.Context, []int64) ([]store.RawGame, error) {
			return []store.RawGame{{GameID: 1011, Game: stale, FetchedAt: time.Unix(1000, 0).UTC()}}, nil
		},
		// upsertRaw left nil: serving stale must not refetch or re-upsert.
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
			p.ID = "88888888-8888-8888-8888-888888888888"
			created = p
			return p, nil
		},
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System"}}, nil
		},
	}
	var calls int
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		calls++
		return nil, errors.New("igdb down") // refetch fails; the stale raw must be served
	}}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return nil, errors.New("pricecharting down") // auto-match lands on the unmatched member
	}}
	h := newUnitHandlers(st, games, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19})
	if rec.Code != http.StatusOK {
		t.Fatalf("below-version raw must resolve from the stale payload when the provider is down: %d %s", rec.Code, rec.Body.String())
	}
	// Differentiator from the nil-table case: a below-version raw with
	// a real table must still attempt (and fail) a refetch, not serve straight from the read.
	if calls != 1 {
		t.Fatalf("below-version raw must attempt exactly one refetch before falling back to stale, got %d", calls)
	}
	if created.IGDB == nil || created.IGDB.GameID != 1011 {
		t.Fatalf("stale payload must build the projection: %+v", created.IGDB)
	}
	if !created.IGDB.FetchedAt.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("stale projection must keep the raw's own stamp: %v", created.IGDB.FetchedAt)
	}
	if len(created.IGDB.ReleaseDates) == 0 {
		t.Fatalf("a below-version stale payload still carries its real release rows: %+v", created.IGDB.ReleaseDates)
	}
}

func TestUnitResolve_PriceProviderDownStillCreatesUnmatched(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	var created store.Product
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs:  func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw: func(context.Context, []igdb.Game, time.Time) error { return nil },
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
			p.ID = "66666666-6666-6666-6666-666666666666"
			created = p
			return p, nil
		},
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System", LogoURL: "https://images.igdb.com/igdb/image/upload/t_logo_med/pl4k.jpg"}}, nil
		},
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger", Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}}}}, nil
	}}
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return nil, errors.New("pricecharting down")
	}}
	h := newUnitHandlers(st, games, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve must survive a pricing outage: %d %s", rec.Code, rec.Body.String())
	}
	if created.PriceCharting != nil {
		t.Fatal("pricing outage must store unmatched, not guess")
	}
	// The null member carries no region/edition/variant: those are
	// vestigial on games, not part of the (game, platform, listing) key.
	if created.Region != "" || created.Edition != "" || created.Variant != "" {
		t.Fatalf("unmatched member must not be a variant-keyed tuple: %+v", created)
	}
	// The platform ref picks up the catalog logo at create.
	if created.Platform == nil || created.Platform.LogoURL != "https://images.igdb.com/igdb/image/upload/t_logo_med/pl4k.jpg" {
		t.Fatalf("platform logo must ride the created product: %+v", created.Platform)
	}
}

// Guards a lost create race: two concurrent resolves converge via
// store.CreateProduct's duplicate-key path onto the winner's document
// (a different id than the loser passed in). The handler must skip
// the initial-snapshot append then, since the winner already appended its own.
func TestUnitResolve_LostRaceDoesNotDoubleSnapshot(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger", Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}}}}, nil
	}}
	loose := int64(500)
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return []pricecharting.Product{{ID: 5011, Name: "Chrono Trigger", ConsoleName: "Super Nintendo", LoosePriceCents: &loose}}, nil
	}}
	body := map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19}

	// Lost race: CreateProduct's stub hands back a different id than
	// passed (the winner's doc), mirroring store.go's real fallback.
	var passedID string
	var snapshotCalls int
	catalogStub := func(context.Context) ([]store.CatalogPlatform, error) {
		return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System"}}, nil
	}
	fetchedAtStub := func(context.Context) (time.Time, error) { return time.Now(), nil }
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs:  func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw: func(context.Context, []igdb.Game, time.Time) error { return nil },
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
			passedID = p.ID
			// Winner's doc returns under another id; listing-keyed identity
			// means loser and winner scored the same auto-match before racing.
			winner := p
			winner.ID = "77777777-7777-7777-7777-777777777777"
			return winner, nil
		},
		appendSnapshot: func(context.Context, store.Snapshot) error {
			snapshotCalls++
			return nil
		},
		platformsFetchedAt: fetchedAtStub,
		listPlatforms:      catalogStub,
	}
	h := newUnitHandlers(st, games, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if passedID == "" {
		t.Fatal("CreateProduct must be called with a pre-minted, non-empty id")
	}
	var p api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Id.String() != "77777777-7777-7777-7777-777777777777" {
		t.Fatalf("response must still serve the winner's doc: %s", p.Id)
	}
	if snapshotCalls != 0 {
		t.Fatalf("a lost race must not append a second snapshot, got %d calls", snapshotCalls)
	}

	// Won race: CreateProduct echoes the same product back (id
	// unchanged) -- the ordinary, non-converged create path.
	snapshotCalls = 0
	st2 := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs:      func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:     func(context.Context, []igdb.Game, time.Time) error { return nil },
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) { return p, nil },
		appendSnapshot: func(context.Context, store.Snapshot) error {
			snapshotCalls++
			return nil
		},
		platformsFetchedAt: fetchedAtStub,
		listPlatforms:      catalogStub,
	}
	h2 := newUnitHandlers(st2, games, prices, newStubCache())
	rec2 := serveUnit(t, h2, env, http.MethodPost, "/products/resolve", tok, body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec2.Code, rec2.Body.String())
	}
	if snapshotCalls != 1 {
		t.Fatalf("winning the create race must append exactly one snapshot, got %d calls", snapshotCalls)
	}
}

func TestUnitResolve_GameManualMatchMintsExactAnchor(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger", Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}}}}, nil
	}}
	// search is deliberately unset: if the manual path ever consulted
	// auto-match, the stub would panic.
	loose := int64(12500)
	prices := &stubPrices{product: func(_ context.Context, id int64) (pricecharting.Product, error) {
		if id != 7042 {
			t.Errorf("manual match must fetch the chosen listing, got %d", id)
		}
		return pricecharting.Product{ID: 7042, Name: "Chrono Trigger [PAL]", ConsoleName: "PAL Super Nintendo", LoosePriceCents: &loose}, nil
	}}
	var created store.Product
	var snapshots int
	st := &stubStore{
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		rawByIDs:      func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:     func(context.Context, []igdb.Game, time.Time) error { return nil },
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) { created = p; return p, nil },
		appendSnapshot: func(context.Context, store.Snapshot) error {
			snapshots++
			return nil
		},
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System"}}, nil
		},
	}
	h := newUnitHandlers(st, games, prices, newStubCache())
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19, "pc_product_id": 7042})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if created.PriceCharting == nil || created.PriceCharting.PCProductID != 7042 {
		t.Fatalf("manual match must map the product: %+v", created.PriceCharting)
	}
	if created.PriceCharting.MatchConfidence != 1.0 || created.PriceCharting.Verified {
		t.Fatalf("manual match is exact but unverified: %+v", created.PriceCharting)
	}
	if created.IGDB == nil || created.IGDB.GameID != 1011 {
		t.Fatalf("still a full game product: %+v", created.IGDB)
	}
	if snapshots != 1 {
		t.Fatalf("manual mint must append the initial snapshot, got %d", snapshots)
	}
}

func TestUnitResolve_GameManualMatchErrors(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 1011, Name: "Chrono Trigger", Platforms: []igdb.Named{{ID: 19, Name: "Super Nintendo Entertainment System"}}}}, nil
	}}
	notFound := func(context.Context, store.ProductKey) (store.Product, error) {
		return store.Product{}, store.ErrNotFound
	}
	catalogOK := func(context.Context) ([]store.CatalogPlatform, error) {
		return []store.CatalogPlatform{{ID: 19, Name: "Super Nintendo Entertainment System"}}, nil
	}
	fetchedOK := func(context.Context) (time.Time, error) { return time.Now(), nil }
	body := map[string]any{"type": "game", "igdb_game_id": 1011, "platform_igdb_id": 19, "pc_product_id": 7042}

	// Unknown listing -> 404 unknown_pc_product.
	st := &stubStore{findProduct: notFound,
		rawByIDs:           func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:          func(context.Context, []igdb.Game, time.Time) error { return nil },
		platformsFetchedAt: fetchedOK, listPlatforms: catalogOK}
	prices := &stubPrices{product: func(context.Context, int64) (pricecharting.Product, error) {
		return pricecharting.Product{}, pricecharting.ErrNotFound
	}}
	rec := serveUnit(t, newUnitHandlers(st, games, prices, newStubCache()), env, http.MethodPost, "/products/resolve", tok, body)
	reqtest.AssertProblemRec(t, rec, http.StatusNotFound, "unknown_pc_product")

	// Provider down -> 502 upstream_unavailable.
	prices.product = func(context.Context, int64) (pricecharting.Product, error) {
		return pricecharting.Product{}, errors.New("boom")
	}
	rec = serveUnit(t, newUnitHandlers(st, games, prices, newStubCache()), env, http.MethodPost, "/products/resolve", tok, body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("provider down: %d %s", rec.Code, rec.Body.String())
	}

	// Bogus game id + manual match -> 404 unknown_game: the IGDB fetch
	// runs first, and the listing is never fetched (unset = panic).
	noGames := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) { return nil, nil }}
	noRaw := &stubStore{findProduct: notFound,
		rawByIDs: func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil }}
	rec = serveUnit(t, newUnitHandlers(noRaw, noGames, &stubPrices{}, newStubCache()),
		env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "game", "igdb_game_id": 999999, "platform_igdb_id": 19, "pc_product_id": 7042})
	reqtest.AssertProblemRec(t, rec, http.StatusNotFound, "unknown_game")
}

// The hint reweights scoring (score-only): with unbracketed variant
// candidates, it flips the winner and stores the score, not 1.0 (the
// variant's extra "cartridge" token keeps it a genuine partial match).
func TestUnitResolve_MatchHintFlipsTheWinner(t *testing.T) {
	env := newAuthEnv(t)
	game := igdb.Game{ID: 1005, Name: "Super Mario 64", Platforms: []igdb.Named{{ID: 4, Name: "Nintendo 64"}}}
	loose := int64(3500)
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		return []pricecharting.Product{
			{ID: 901, Name: "Super Mario 64", ConsoleName: "Nintendo 64", LoosePriceCents: &loose},
			{ID: 902, Name: "Super Mario 64 Players Choice Cartridge", ConsoleName: "Nintendo 64", LoosePriceCents: &loose},
		}, nil
	}}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) { return []igdb.Game{game}, nil }}
	var created store.Product
	st := &stubStore{
		rawByIDs:           func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:          func(context.Context, []igdb.Game, time.Time) error { return nil },
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms:      func(context.Context) ([]store.CatalogPlatform, error) { return nil, nil },
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
			created = p
			return p, nil
		},
		appendSnapshot: func(context.Context, store.Snapshot) error { return nil },
	}
	h := newUnitHandlers(st, games, prices, newStubCache())
	tok := env.token(t, "u1", []string{"user"})

	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok, map[string]any{
		"type": "game", "igdb_game_id": 1005, "platform_igdb_id": 4,
		"match_hint": "players choice",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.Bytes())
	}
	if created.PriceCharting == nil || created.PriceCharting.PCProductID != 902 {
		t.Fatalf("hint must flip the winner: %+v", created.PriceCharting)
	}
	if created.PriceCharting.MatchConfidence >= 1.0 || created.PriceCharting.Verified {
		t.Fatalf("score-only hint must store the score unverified: %+v", created.PriceCharting)
	}
}

// The resolve reads and populates the SAME cache entries the
// pc_listing search endpoint serves: one provider search feeds both.
func TestUnitResolve_SharesThePCListingSearchCache(t *testing.T) {
	env := newAuthEnv(t)
	game := igdb.Game{ID: 1005, Name: "Super Mario 64", Platforms: []igdb.Named{{ID: 4, Name: "Nintendo 64"}}}
	loose := int64(3500)
	var searchCalls int
	prices := &stubPrices{search: func(context.Context, string) ([]pricecharting.Product, error) {
		searchCalls++
		return []pricecharting.Product{{ID: 5005, Name: "Super Mario 64", ConsoleName: "Nintendo 64", LoosePriceCents: &loose}}, nil
	}}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) { return []igdb.Game{game}, nil }}
	st := &stubStore{
		rawByIDs:           func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:          func(context.Context, []igdb.Game, time.Time) error { return nil },
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now(), nil },
		listPlatforms:      func(context.Context) ([]store.CatalogPlatform, error) { return nil, nil },
		findProduct: func(context.Context, store.ProductKey) (store.Product, error) {
			return store.Product{}, store.ErrNotFound
		},
		createProduct:  func(_ context.Context, p store.Product) (store.Product, error) { return p, nil },
		appendSnapshot: func(context.Context, store.Snapshot) error { return nil },
	}
	c := newStubCache()
	h := newUnitHandlers(st, games, prices, c)
	tok := env.token(t, "u1", []string{"user"})

	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok, map[string]any{
		"type": "game", "igdb_game_id": 1005, "platform_igdb_id": 4,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.Bytes())
	}
	if searchCalls != 1 {
		t.Fatalf("resolve must search the provider once, got %d", searchCalls)
	}
	if c.search["pc_listing:super mario 64"] == nil {
		t.Fatal("resolve must populate the pc_listing search cache")
	}

	// The search endpoint now serves the resolve-populated entry.
	rec = serveUnit(t, h, env, http.MethodGet, "/search?type=pc_listing&q=Super%20Mario%2064", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status %d", rec.Code)
	}
	if searchCalls != 1 {
		t.Fatalf("search endpoint must hit the shared cache, provider calls %d", searchCalls)
	}
}

func TestResolve_PCListingCreatesAndDedupes(t *testing.T) {
	s := newStack(t)
	body := map[string]any{"type": "pc_listing", "pc_product_id": 5001}
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first resolve: status %d", resp.StatusCode)
	}
	first := decodeBody[api.Product](t, resp)
	if string(first.Type) != "pc_listing" {
		t.Fatalf("type = %s, want pc_listing", first.Type)
	}
	if first.Igdb != nil {
		t.Fatal("pc_listing products must carry no igdb subdoc")
	}
	if first.Pricecharting == nil || first.Pricecharting.PcProductId != 5001 {
		t.Fatal("pc_listing product must carry the picked listing")
	}
	if first.Pricecharting.MatchConfidence != 1.0 || first.Pricecharting.Verified {
		t.Fatalf("want confidence 1.0 verified false, got %v/%v",
			first.Pricecharting.MatchConfidence, first.Pricecharting.Verified)
	}
	if first.Pricecharting.LooseCents == nil {
		t.Fatal("stub listings always price; want current prices on create")
	}
	if first.Region != nil || first.Edition != nil || first.Variant != nil {
		t.Fatal("pc_listing identity fields must stay empty")
	}

	// Same listing again, with stray variant fields: same product, ignored.
	body["variant"] = "players choice"
	resp = s.do(http.MethodPost, "/products/resolve", s.userToken(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second resolve: status %d", resp.StatusCode)
	}
	second := decodeBody[api.Product](t, resp)
	if second.Id != first.Id {
		t.Fatalf("dedup failed: %s vs %s", second.Id, first.Id)
	}

	// The create appended the day-zero snapshot.
	hist := s.do(http.MethodPost, "/products/price-history:batch", s.userToken(),
		map[string]any{"product_ids": []string{first.Id.String()}})
	series := decodeBody[api.PriceHistoryResponse](t, hist).Series
	if len(series[first.Id.String()]) != 1 {
		t.Fatalf("want exactly the initial snapshot, got %d points", len(series[first.Id.String()]))
	}
}

func TestUnitResolve_PCListingValidationAndErrors(t *testing.T) {
	env := newAuthEnv(t)
	tok := env.token(t, "u1", []string{"user"})
	st := &stubStore{}
	prices := &stubPrices{}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())

	// Missing pc_product_id.
	rec := serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "pc_listing"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: status %d", rec.Code)
	}

	// Unknown listing id -> 404 unknown_pc_product.
	prices.product = func(ctx context.Context, id int64) (pricecharting.Product, error) {
		return pricecharting.Product{}, pricecharting.ErrNotFound
	}
	st.findProduct = func(ctx context.Context, key store.ProductKey) (store.Product, error) {
		return store.Product{}, store.ErrNotFound
	}
	rec = serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "pc_listing", "pc_product_id": 999999})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d", rec.Code)
	}

	// Provider down -> 502 upstream_unavailable.
	prices.product = func(ctx context.Context, id int64) (pricecharting.Product, error) {
		return pricecharting.Product{}, errors.New("boom")
	}
	rec = serveUnit(t, h, env, http.MethodPost, "/products/resolve", tok,
		map[string]any{"type": "pc_listing", "pc_product_id": 5001})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("provider down: status %d", rec.Code)
	}
}

func TestAdminMapping_CorrectVerifyClearAndErrors(t *testing.T) {
	s := newStack(t)
	// Terranigma resolves unmatched; the admin pins it to a mapping.
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1018, "platform_igdb_id": 19,
	})
	p := decodeBody[api.Product](t, resp)
	id := p.Id.String()

	// Non-admin: 403.
	resp = s.do(http.MethodPut, "/admin/products/"+id+"/pricecharting", s.userToken(), map[string]any{"pc_product_id": 5017})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin remap: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Admin remap to EarthBound's product id: verified, priced,
	// snapshotted, visible immediately.
	resp = s.do(http.MethodPut, "/admin/products/"+id+"/pricecharting", s.adminToken(), map[string]any{"pc_product_id": 5017})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("remap: %d %s", resp.StatusCode, body)
	}
	mapped := decodeBody[api.Product](t, resp)
	if mapped.Pricecharting == nil || mapped.Pricecharting.PcProductId != 5017 ||
		!mapped.Pricecharting.Verified || mapped.Pricecharting.MatchConfidence != 1.0 ||
		mapped.Pricecharting.LooseCents == nil {
		t.Fatalf("mapping: %+v", mapped.Pricecharting)
	}
	var n int64
	_ = s.pool.QueryRow(context.Background(), "SELECT count(*) FROM price_snapshots WHERE product_id = $1", id).Scan(&n)
	if n != 1 {
		t.Fatalf("mapping snapshot: %d", n)
	}
	// The read path serves the correction immediately (cache dropped).
	resp = s.do(http.MethodGet, "/products/"+id, s.userToken(), nil)
	fresh := decodeBody[api.Product](t, resp)
	if fresh.Pricecharting == nil || fresh.Pricecharting.PcProductId != 5017 {
		t.Fatalf("read-after-remap: %+v", fresh.Pricecharting)
	}

	// Clearing makes it unmatched again.
	resp = s.do(http.MethodPut, "/admin/products/"+id+"/pricecharting", s.adminToken(), map[string]any{"pc_product_id": nil})
	cleared := decodeBody[api.Product](t, resp)
	if cleared.Pricecharting != nil {
		t.Fatalf("clear: %+v", cleared.Pricecharting)
	}

	// Error taxonomy.
	resp = s.do(http.MethodPut, "/admin/products/"+id+"/pricecharting", s.adminToken(), map[string]any{"pc_product_id": 999999})
	reqtest.AssertProblem(t, resp, http.StatusNotFound, "unknown_pc_product")
	ghost := "99999999-9999-9999-9999-999999999999"
	resp = s.do(http.MethodPut, "/admin/products/"+ghost+"/pricecharting", s.adminToken(), map[string]any{"pc_product_id": 5017})
	reqtest.AssertProblem(t, resp, http.StatusNotFound, "product_not_found")
}

// Mapping changes are identity moves: a taken listing (set) or a
// colliding clear both answer 409; a successful clear sets match_hold.
func TestAdminMapping_IdentityTakenAndHold(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	matched := s.resolveGame(1005, 4) // auto-lands listing 5005
	resp := s.do(http.MethodPost, "/products/resolve", s.userToken(), map[string]any{
		"type": "game", "igdb_game_id": 1005, "platform_igdb_id": 4,
		"match_hint": "grey cart brick serial",
	})
	unmatched := decodeBody[api.Product](t, resp)

	// Set: the target listing is already carried by the matched member.
	resp = s.do(http.MethodPut, "/admin/products/"+unmatched.Id.String()+"/pricecharting",
		s.adminToken(), map[string]any{"pc_product_id": 5005})
	p := reqtest.AssertProblem(t, resp, http.StatusConflict, "identity_taken")
	if !strings.Contains(p.Detail, "already carries that listing") {
		t.Fatalf("set-collision detail: %q", p.Detail)
	}

	// Clear: the family already has an unmatched member.
	resp = s.do(http.MethodPut, "/admin/products/"+matched.Id.String()+"/pricecharting",
		s.adminToken(), map[string]any{"pc_product_id": nil})
	p = reqtest.AssertProblem(t, resp, http.StatusConflict, "identity_taken")
	if !strings.Contains(p.Detail, "clearing would collide") {
		t.Fatalf("clear-collision detail: %q", p.Detail)
	}

	// A collision-free clear lands, unmaps, and holds.
	resp = s.do(http.MethodPut, "/admin/products/"+unmatched.Id.String()+"/pricecharting",
		s.adminToken(), map[string]any{"pc_product_id": 5099})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("free set: %d", resp.StatusCode)
	}
	resp = s.do(http.MethodPut, "/admin/products/"+matched.Id.String()+"/pricecharting",
		s.adminToken(), map[string]any{"pc_product_id": nil})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("free clear: %d", resp.StatusCode)
	}
	got, err := s.store.GetProduct(ctx, matched.Id.String())
	if err != nil || got.PriceCharting != nil || !got.MatchHold {
		t.Fatalf("clear must unmap and hold: %+v, %v", got, err)
	}
	// Setting a mapping lifts the hold and stays admin-verified.
	resp = s.do(http.MethodPut, "/admin/products/"+matched.Id.String()+"/pricecharting",
		s.adminToken(), map[string]any{"pc_product_id": 5005})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-set: %d", resp.StatusCode)
	}
	got, err = s.store.GetProduct(ctx, matched.Id.String())
	if err != nil || got.PriceCharting == nil || !got.PriceCharting.Verified || got.MatchHold {
		t.Fatalf("re-set must map verified and lift the hold: %+v, %v", got, err)
	}
}

// Pins that both identity_taken arms name the product already
// holding the identity, so an admin can look it up instead of guessing.
func TestAdminMapping_ConflictNamesHolder(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	matched, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Holder Game",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9301, Name: "Holder Game", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPriceCharting(ctx, matched.ID, &store.PCMeta{
		PCProductID: 5005, PCName: "Super Mario 64", ConsoleName: "Nintendo 64",
		MatchConfidence: 1, AsOf: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	unmatched, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Holder Game",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9301, Name: "Holder Game", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set collision: fixing the unmatched member to the taken listing
	// names the matched holder.
	resp := s.do(http.MethodPut, "/admin/products/"+unmatched.ID+"/pricecharting", s.adminToken(),
		map[string]any{"pc_product_id": 5005})
	p := reqtest.AssertProblem(t, resp, http.StatusConflict, "identity_taken")
	if !strings.Contains(p.Detail, matched.ID) || !strings.Contains(p.Detail, "Holder Game") {
		t.Fatalf("set-collision detail must name the holder: %q", p.Detail)
	}

	// Clear collision: clearing the matched member while the family
	// already has an unmatched member names that member.
	resp = s.do(http.MethodPut, "/admin/products/"+matched.ID+"/pricecharting", s.adminToken(),
		map[string]any{"pc_product_id": nil})
	p = reqtest.AssertProblem(t, resp, http.StatusConflict, "identity_taken")
	if !strings.Contains(p.Detail, unmatched.ID) {
		t.Fatalf("clear-collision detail must name the unmatched member: %q", p.Detail)
	}
}

// Pins the parking lever: PUT null on an already-unmatched product
// answers 200 with match_hold set, idempotently (no identity collision).
func TestAdminMapping_HoldUnmatched(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	orphan, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Orphan Game",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9302, Name: "Orphan Game", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		got := decodeBody[api.Product](t, s.do(http.MethodPut,
			"/admin/products/"+orphan.ID+"/pricecharting", s.adminToken(),
			map[string]any{"pc_product_id": nil}))
		if got.MatchHold == nil || !*got.MatchHold || got.Pricecharting != nil {
			t.Fatalf("round %d: hold must set and mapping stay absent: %+v", i, got)
		}
	}
}

// TestAdminDeleteProduct pins the guarded mop end to end: RBAC, the
// unmatched-only guard, snapshot cleanup, and honest status codes.
func TestAdminDeleteProduct(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	orphan, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Mop Target",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9501, Name: "Mop Target", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Mop Survivor",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9502, Name: "Mop Survivor", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPriceCharting(ctx, matched.ID, &store.PCMeta{
		PCProductID: 9510, PCName: "Mop Survivor", ConsoleName: "Nintendo 64",
		MatchConfidence: 1, AsOf: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Non-admin: 403, nothing deleted.
	resp := s.do(http.MethodDelete, "/admin/products/"+orphan.ID, s.userToken(), nil)
	reqtest.AssertProblem(t, resp, http.StatusForbidden, "forbidden")

	// Admin on unmatched: 204 and the product is gone.
	resp = s.do(http.MethodDelete, "/admin/products/"+orphan.ID, s.adminToken(), nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := s.store.GetProduct(ctx, orphan.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan must be gone, got %v", err)
	}

	// Repeat: 404 product_not_found.
	resp = s.do(http.MethodDelete, "/admin/products/"+orphan.ID, s.adminToken(), nil)
	reqtest.AssertProblem(t, resp, http.StatusNotFound, "product_not_found")

	// Matched: 409 product_matched, survives.
	resp = s.do(http.MethodDelete, "/admin/products/"+matched.ID, s.adminToken(), nil)
	reqtest.AssertProblem(t, resp, http.StatusConflict, "product_matched")
	if _, err := s.store.GetProduct(ctx, matched.ID); err != nil {
		t.Fatalf("matched product must survive: %v", err)
	}
}

func TestUnitAdminMapping_CommunityRefused(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "console", Name: "Handheld Mod", Origin: "community"}
	// prices stub left empty: any provider call would panic, proving
	// the guard fires before the mapping machinery.
	st := &stubStore{getProduct: func(context.Context, string) (store.Product, error) { return comm, nil }}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPut, "/admin/products/"+comm.ID+"/pricecharting", admin,
		map[string]any{"pc_product_id": 5005})
	reqtest.AssertProblemRec(t, rec, http.StatusConflict, "product_not_provider")
}
