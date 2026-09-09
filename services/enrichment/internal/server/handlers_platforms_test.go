// Tests for the platform catalog.

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/levonn-dev/vgkeep/libs/go/contract/common"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/gen/api"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/store"
)

func TestUnitListPlatforms_JoinsAliasesSortsAndCaches(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	var storeCalls int
	st := &stubStore{
		// Fresh stamp keeps ensurePlatforms on the cached-catalog path
		// (never touches the IGDB stub).
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now().UTC(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			storeCalls++
			return []store.CatalogPlatform{
				{ID: 19, Name: "Super Nintendo Entertainment System"},
				{ID: 18, Name: "Nintendo Entertainment System"},
				// Alias-less: PlatformAliases returns nil, which must still
				// serialize as [] (the contract requires string[], no null guard).
				{ID: 23, Name: "Dreamcast"},
			}, nil
		},
	}
	c := newStubCache()
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, c)

	rec := serveUnit(t, h, env, http.MethodGet, "/platforms", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platforms: %d %s", rec.Code, rec.Body.String())
	}
	var out api.PlatformCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// Dreamcast's alias-less row (see above) must serialize as [], never null.
	if body := rec.Body.String(); !strings.Contains(body, `"aliases":[]`) || strings.Contains(body, `"aliases":null`) {
		t.Fatalf("alias-less platform must serialize aliases:[] not null: %s", body)
	}
	// Sorted by name: Dreamcast precedes the Nintendo rows.
	if len(out.Platforms) != 3 || out.Platforms[0].Name != "Dreamcast" {
		t.Fatalf("sort by name failed: %+v", out.Platforms)
	}
	var snes *common.CatalogPlatform
	for i := range out.Platforms {
		if out.Platforms[i].IgdbId == 19 {
			snes = &out.Platforms[i]
		}
	}
	if snes == nil {
		t.Fatalf("snes row missing: %+v", out.Platforms)
	}
	if !slices.Contains(snes.Aliases, "snes") {
		t.Fatalf("snes aliases missing 'snes': %v", snes.Aliases)
	}

	// The second call is served from Valkey: the store is not read again.
	before := storeCalls
	rec = serveUnit(t, h, env, http.MethodGet, "/platforms", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached platforms: %d", rec.Code)
	}
	if storeCalls != before {
		t.Fatalf("second call hit the store, want cache")
	}
}

// Pins the physical-catalog gate on the platform catalog: digital-only
// platforms (regionkit.DigitalOnlyPlatformIDs) never reach the picker
// or the normalize lever, however the cached IGDB table lists them.
func TestUnitListPlatforms_OmitsDigitalOnlyPlatforms(t *testing.T) {
	env := newAuthEnv(t)
	user := env.token(t, uuid.NewString(), []string{"user"})
	st := &stubStore{
		platformsFetchedAt: func(context.Context) (time.Time, error) { return time.Now().UTC(), nil },
		listPlatforms: func(context.Context) ([]store.CatalogPlatform, error) {
			return []store.CatalogPlatform{
				{ID: 39, Name: "iOS"},
				{ID: 19, Name: "Super Nintendo Entertainment System"},
			}, nil
		},
	}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/platforms", user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platforms: %d %s", rec.Code, rec.Body.String())
	}
	var out api.PlatformCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Platforms) != 1 || out.Platforms[0].IgdbId != 19 {
		t.Fatalf("want only the SNES row, got %+v", out.Platforms)
	}
}
