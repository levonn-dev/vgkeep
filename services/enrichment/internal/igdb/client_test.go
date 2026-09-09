package igdb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient wires a Client at httptest servers. The token server
// counts calls; the api handler is per-test.
func newTestClient(t *testing.T, api http.HandlerFunc) (*Client, *atomic.Int64) {
	t.Helper()
	var tokenCalls atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"app-token-1","expires_in":3600,"token_type":"bearer"}`))
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	c := NewClient("test-client-id", "test-secret")
	c.apiURL = apiSrv.URL
	c.tokenURL = tokenSrv.URL
	return c, &tokenCalls
}

func TestClient_QueryShapeAndTokenReuse(t *testing.T) {
	var bodies []string
	c, tokenCalls := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		if r.Header.Get("Client-ID") != "test-client-id" || r.Header.Get("Authorization") != "Bearer app-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1011,"name":"Chrono Trigger","genres":[{"id":12,"name":"Role-playing (RPG)"}],"first_release_date":788918400,"total_rating_count":812}]`))
	})

	got, err := c.SearchGames(context.Background(), `zelda "special"`, 20)
	if err != nil || len(got) != 1 || got[0].ID != 1011 || got[0].TotalRatingCount != 812 {
		t.Fatalf("search: %+v, %v", got, err)
	}
	if _, err := c.GamesByIDs(context.Background(), []int64{1011, 1012}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[0], `search "zelda \"special\"";`) || !strings.Contains(bodies[0], "fields name,cover.image_id") ||
		!strings.Contains(bodies[0], "total_rating_count") {
		t.Fatalf("bad search body: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "release_dates.date,release_dates.platform,release_dates.release_region") {
		t.Fatalf("search body must request the release table: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "where id = (1011,1012);") {
		t.Fatalf("bad where body: %s", bodies[1])
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token must be fetched once and reused, got %d fetches", tokenCalls.Load())
	}
}

func TestClient_GamesByIDsChunksAt500(t *testing.T) {
	var bodies []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(`[{"id":1,"name":"A"}]`))
	})
	ids := make([]int64, 1100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	got, err := c.GamesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want the chunk results concatenated (one per query), got %d", len(got))
	}
	if len(bodies) != 3 {
		t.Fatalf("want 3 queries for 1100 ids, got %d", len(bodies))
	}
	for i, want := range []struct{ first, last, limit string }{
		{"(1,", ",500);", "limit 500;"},
		{"(501,", ",1000);", "limit 500;"},
		{"(1001,", ",1100);", "limit 100;"},
	} {
		if !strings.Contains(bodies[i], "where id = "+want.first) ||
			!strings.Contains(bodies[i], want.last) ||
			!strings.Contains(bodies[i], want.limit) {
			t.Fatalf("chunk %d body wrong: %.80s...%s", i, bodies[i], bodies[i][max(0, len(bodies[i])-40):])
		}
	}
}

func TestClient_GamesByIDsChunkErrorPropagates(t *testing.T) {
	var calls atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	ids := make([]int64, 501)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if _, err := c.GamesByIDs(context.Background(), ids); err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("second-chunk failure must surface, got %v", err)
	}
}

// Pins the game_localizations where-body shape after the
// quote/backslash strip, distinct ids in response order, and the
// empty-after-strip short-circuit that must not spend a provider call.
func TestClient_SearchLocalizationsQuoteStripAndShape(t *testing.T) {
	var body string
	var calls atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`[{"game":1001},{"game":1001},{"game":1016}]`))
	})

	got, err := c.SearchLocalizations(context.Background(), `ゼルダ"の"伝説\`, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `where name ~ *"ゼルダの伝説"* & game.game_type != (1,5,6,7,12,13,14);`) || !strings.Contains(body, "limit 20;") {
		t.Fatalf("bad where body (quotes and backslashes must be stripped): %s", body)
	}
	if len(got) != 2 || got[0] != 1001 || got[1] != 1016 {
		t.Fatalf("want distinct ids in response order, got %v", got)
	}

	if _, err := c.SearchLocalizations(context.Background(), `"""\`, 20); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("an all-quotes-and-backslashes (empty-after-strip) query must not call the provider, got %d calls", calls.Load())
	}
}

func TestClient_PlatformsQueryShape(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`[{"id":4,"name":"Nintendo 64","abbreviation":"N64","generation":5,"platform_logo":{"image_id":"pl78"}}]`))
	})
	got, err := c.Platforms(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("platforms: %+v, %v", got, err)
	}
	p := got[0]
	if p.ID != 4 || p.Name != "Nintendo 64" || p.Abbreviation != "N64" || p.Generation != 5 ||
		p.PlatformLogo == nil || p.PlatformLogo.ImageID != "pl78" {
		t.Fatalf("projection decode: %+v", p)
	}
	if body != "fields name,abbreviation,generation,platform_logo.image_id; sort id asc; limit 500;" {
		t.Fatalf("bad platforms body: %s", body)
	}
}

// Pins the physical-catalog gate: every discovery query excludes the
// digital-only game types, while fetch-by-ids stays unfiltered so
// existing products keep refreshing whatever their type.
func TestClient_DiscoveryQueriesExcludeDigitalOnlyTypes(t *testing.T) {
	var bodies []string // search, localizations, popular, by-ids (call order)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(`[]`))
	})
	ctx := context.Background()
	if _, err := c.SearchGames(ctx, "zelda", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchLocalizations(ctx, "ゼルダ", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PopularGames(ctx, []int64{12}, []int64{7}, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GamesByIDs(ctx, []int64{1011}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 4 {
		t.Fatalf("want 4 provider calls, got %d", len(bodies))
	}
	const clause = "game_type != (1,5,6,7,12,13,14)"
	if !strings.Contains(bodies[0], `search "zelda"; fields `) || !strings.Contains(bodies[0], "; where "+clause+"; limit 20;") {
		t.Fatalf("search must gate on game type: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `where name ~ *"ゼルダ"* & game.`+clause+"; limit 20;") {
		t.Fatalf("localization leg must gate on the expanded game's type: %s", bodies[1])
	}
	if !strings.Contains(bodies[2], "where genres = (12) & total_rating_count >= 20 & id != (7) & "+clause+"; sort total_rating desc;") {
		t.Fatalf("popular query must append the gate to its where clause: %s", bodies[2])
	}
	if !strings.Contains(bodies[3], "; where id = (1011); limit 1;") || strings.Contains(bodies[3], "game_type") {
		t.Fatalf("fetch-by-ids must stay unfiltered: %s", bodies[3])
	}
}

func TestClient_PopularGames_ExcludesClientSide(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"name":"A","total_rating":90},{"id":2,"name":"B","total_rating":85},{"id":3,"name":"C","total_rating":80}]`))
	})
	got, err := c.PopularGames(context.Background(), []int64{12}, []int64{2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("exclusion/limit broken: %+v", got)
	}
}

// Pins that the constructed limit clause stays within maxIDsPerQuery
// even with a much larger exclude set (recommendations callers can
// pass thousands): limit+len(excludeIDs) must never reach the provider unclamped.
func TestClient_PopularGames_LimitClauseCappedForLargeExcludeSet(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`[]`))
	})
	exclude := make([]int64, 600)
	for i := range exclude {
		exclude[i] = int64(i + 1)
	}
	if _, err := c.PopularGames(context.Background(), []int64{12}, exclude, 40); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "limit 640;") {
		t.Fatalf("limit clause must be capped, not limit+len(excludeIDs) unclamped: %s", body)
	}
	if !strings.Contains(body, "limit 500;") {
		t.Fatalf("want the clamped ceiling (500) in the request body: %s", body)
	}
}

func TestClient_429SurfacesError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, err := c.SearchGames(context.Background(), "zelda", 5); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want 429 error, got %v", err)
	}
}

func TestClient_TimeoutSurfaces(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`[]`))
	})
	c.httpc.Timeout = 30 * time.Millisecond
	if _, err := c.SearchGames(context.Background(), "zelda", 5); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestClient_MalformedJSONSurfaces(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"not":"an array`))
	})
	if _, err := c.SearchGames(context.Background(), "zelda", 5); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestClient_TokenFailureSurfaces(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	// A bare http.HandlerFunc has no router, so a bogus tokenURL path
	// still succeeds; point at a dedicated token fake that always fails.
	failingTokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failingTokenSrv.Close)
	c.tokenURL = failingTokenSrv.URL

	if _, err := c.SearchGames(context.Background(), "zelda", 5); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want token error, got %v", err)
	}
}

func TestClient_GameDecodesReleaseDates(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1011,"name":"Chrono Trigger","release_dates":[
			{"id":9,"date":794880000,"platform":19,"release_region":5},
			{"id":10,"date":809049600,"platform":19,"release_region":2},
			{"id":11,"platform":19,"release_region":1}]}]`))
	})
	got, err := c.GamesByIDs(context.Background(), []int64{1011})
	if err != nil || len(got) != 1 {
		t.Fatalf("fetch: %+v, %v", got, err)
	}
	rds := got[0].ReleaseDates
	if len(rds) != 3 || rds[0].Date != 794880000 || rds[0].Platform != 19 || rds[0].Region != 5 {
		t.Fatalf("release_dates decode: %+v", rds)
	}
	if rds[2].Date != 0 {
		t.Fatalf("dateless row must decode with zero date: %+v", rds[2])
	}
}

// Pins the fetched-but-none sentinel at the plain decode boundary: an
// explicit empty array decodes non-nil, an absent key decodes nil.
// gamePayloadFor and NewIGDBMeta rely on telling these states apart.
func TestGame_ReleaseDatesSentinel(t *testing.T) {
	var withEmpty Game
	if err := json.Unmarshal([]byte(`{"id":1,"name":"A","release_dates":[]}`), &withEmpty); err != nil {
		t.Fatal(err)
	}
	if withEmpty.ReleaseDates == nil || len(withEmpty.ReleaseDates) != 0 {
		t.Fatalf("release_dates:[] must decode to a non-nil empty slice, got %#v", withEmpty.ReleaseDates)
	}

	var withoutKey Game
	if err := json.Unmarshal([]byte(`{"id":1,"name":"A"}`), &withoutKey); err != nil {
		t.Fatal(err)
	}
	if withoutKey.ReleaseDates != nil {
		t.Fatalf("a missing release_dates key must decode to nil, got %#v", withoutKey.ReleaseDates)
	}
}

func TestRegionName_MapsKnownAndDropsUnknown(t *testing.T) {
	for want, id := range map[string]int{
		"europe": 1, "north_america": 2, "australia": 3, "new_zealand": 4,
		"japan": 5, "china": 6, "asia": 7, "worldwide": 8, "korea": 9, "brazil": 10,
	} {
		got, ok := RegionName(id)
		if !ok || got != want {
			t.Fatalf("RegionName(%d) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if _, ok := RegionName(11); ok {
		t.Fatal("unknown region enum must not map")
	}
}

func TestTwinPlatformID_MapsJPTwinsAndDropsOthers(t *testing.T) {
	for id, want := range map[int64]int64{
		19: 58, 58: 19, // SNES <-> Super Famicom
		18: 99, 99: 18, // NES  <-> Family Computer
	} {
		if got := TwinPlatformID(id); got != want {
			t.Fatalf("TwinPlatformID(%d) = %d; want %d", id, got, want)
		}
	}
	// A platform with no JP twin (and the zero id) returns 0: no fold.
	for _, id := range []int64{0, 4, 130, 8} {
		if got := TwinPlatformID(id); got != 0 {
			t.Fatalf("TwinPlatformID(%d) = %d; want 0 (no twin)", id, got)
		}
	}
}
