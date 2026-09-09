// Tests for community products: admin mints, worklists, and
// promoting a community product onto provider anchors.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestAdminUnmatchedWorklist(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	oldest, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Worklist Oldest",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB:     &store.IGDBMeta{GameID: 9201, Name: "Worklist Oldest", Genres: []store.Genre{}, Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.store.CreateProduct(ctx, store.Product{
		Type: "console", Name: "Worklist Held Console", Region: "pal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPriceCharting(ctx, held.ID, nil); err != nil { // deliberate clear = hold
		t.Fatal(err)
	}

	// Non-admin: 403 with the forbidden code.
	resp := s.do(http.MethodGet, "/admin/products/unmatched", s.userToken(), nil)
	reqtest.AssertProblem(t, resp, http.StatusForbidden, "forbidden")

	// Admin: the full envelope, oldest first, held item flagged.
	page := decodeBody[api.UnmatchedProductsPage](t,
		s.do(http.MethodGet, "/admin/products/unmatched", s.adminToken(), nil))
	if page.TotalCount != 2 || len(page.Products) != 2 {
		t.Fatalf("envelope: total=%d len=%d", page.TotalCount, len(page.Products))
	}
	if page.Products[0].Id.String() != oldest.ID || page.Products[1].Id.String() != held.ID {
		t.Fatalf("order: %v then %v", page.Products[0].Id, page.Products[1].Id)
	}
	if page.Products[0].MatchHold != nil {
		t.Fatal("plain unmatched product must not carry match_hold")
	}
	if page.Products[1].MatchHold == nil || !*page.Products[1].MatchHold {
		t.Fatal("held product must carry match_hold true")
	}

	// limit/offset slice the same order; total_count stays full.
	paged := decodeBody[api.UnmatchedProductsPage](t,
		s.do(http.MethodGet, "/admin/products/unmatched?limit=1&offset=1", s.adminToken(), nil))
	if paged.TotalCount != 2 || len(paged.Products) != 1 || paged.Products[0].Id.String() != held.ID {
		t.Fatalf("paged: total=%d %+v", paged.TotalCount, paged.Products)
	}

	// Out-of-bounds limits reject rather than clamping: the contract's
	// bound (1-500) is specval's job now.
	if resp := s.do(http.MethodGet, "/admin/products/unmatched?limit=0", s.adminToken(), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=0 must be rejected: %d", resp.StatusCode)
	}
	if resp := s.do(http.MethodGet, "/admin/products/unmatched?limit=9999", s.adminToken(), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=9999 must be rejected: %d", resp.StatusCode)
	}
}

func TestAdminCommunityWorklist(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	oldest, err := s.store.CreateProduct(ctx, store.Product{Type: "game", Name: "Community Oldest", Origin: "community"})
	if err != nil {
		t.Fatal(err)
	}
	// Guards against a same-millisecond updated_at tie (the _id
	// tiebreak is a random UUID, not a real proxy for creation order).
	time.Sleep(5 * time.Millisecond)
	newer, err := s.store.CreateProduct(ctx, store.Product{Type: "console", Name: "Community Newer", Origin: "community"})
	if err != nil {
		t.Fatal(err)
	}
	// A provider product must never surface in the community listing.
	if _, err := s.store.CreateProduct(ctx, store.Product{
		Type: "game", Name: "Community Worklist Provider Excluded",
		Platform: &store.Platform{IGDBID: 4, Name: "Nintendo 64"},
		IGDB: &store.IGDBMeta{GameID: 9601, Name: "Community Worklist Provider Excluded", Genres: []store.Genre{},
			Themes: []string{}, Franchises: []string{}, SimilarGames: []int64{}, Companies: []store.Company{}, FetchedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}

	// Non-admin: 403 with the forbidden code.
	resp := s.do(http.MethodGet, "/admin/products/community", s.userToken(), nil)
	reqtest.AssertProblem(t, resp, http.StatusForbidden, "forbidden")

	// Admin: the full envelope, oldest first, provider product excluded.
	page := decodeBody[api.CommunityProductsPage](t,
		s.do(http.MethodGet, "/admin/products/community", s.adminToken(), nil))
	if page.TotalCount != 2 || len(page.Products) != 2 {
		t.Fatalf("envelope: total=%d len=%d", page.TotalCount, len(page.Products))
	}
	if page.Products[0].Id.String() != oldest.ID || page.Products[1].Id.String() != newer.ID {
		t.Fatalf("order: %v then %v", page.Products[0].Id, page.Products[1].Id)
	}
	for _, p := range page.Products {
		if p.Origin == nil || *p.Origin != "community" {
			t.Fatalf("every product must carry origin community: %+v", p)
		}
	}

	// limit/offset slice the same order; total_count stays full.
	paged := decodeBody[api.CommunityProductsPage](t,
		s.do(http.MethodGet, "/admin/products/community?limit=1&offset=1", s.adminToken(), nil))
	if paged.TotalCount != 2 || len(paged.Products) != 1 || paged.Products[0].Id.String() != newer.ID {
		t.Fatalf("paged: total=%d %+v", paged.TotalCount, paged.Products)
	}

	// Out-of-bounds limits reject rather than clamping (1-500, specval's
	// job); TestValidatorPath_ListCommunityProducts_LimitOverMax_ClampReversal pins it directly.
	if resp := s.do(http.MethodGet, "/admin/products/community?limit=0", s.adminToken(), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=0 must be rejected: %d", resp.StatusCode)
	}
	if resp := s.do(http.MethodGet, "/admin/products/community?limit=9999", s.adminToken(), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=9999 must be rejected: %d", resp.StatusCode)
	}
}

func TestUnitCreateCommunityProduct(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	user := env.token(t, uuid.NewString(), []string{"user"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	// Role gate first: the store must never run for a plain user.
	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", user,
		map[string]any{"type": "game", "name": "Repro Alpha"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d, want 403", rec.Code)
	}

	// Validation: bad type and empty name are 400 invalid_body.
	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "pc_listing", "name": "X"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type: %d, want 400", rec.Code)
	}
	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank name: %d, want 400", rec.Code)
	}

	// The mint: origin community, facts in the community block, the
	// single edition field carried through, variant left empty.
	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin, map[string]any{
		"type": "game", "name": "Repro Alpha", "platform_name": "SNES",
		"region": "pal", "edition": "glow cart", "first_release_date": "1995-10-09",
		"developers": []string{"  Garage Team  ", ""}, "publishers": []string{"Repro House"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Origin != "community" || got.Type != "game" || got.Name != "Repro Alpha" {
		t.Fatalf("stored product wrong: %+v", got)
	}
	if got.Variant != "" {
		t.Fatalf("variant must stay empty on community mints, got %q", got.Variant)
	}
	if got.Community == nil || got.Community.PlatformName != "SNES" ||
		got.Community.FirstReleaseDate.Format("2006-01-02") != "1995-10-09" {
		t.Fatalf("community facts wrong: %+v", got.Community)
	}
	// Region lives in the community block, not the top-level field:
	// the top-level field stays empty on community mints.
	if got.Region != "" || got.Community.Region != "pal" || got.Edition != "glow cart" {
		t.Fatalf("region/edition wrong: top=%q community=%q edition=%q", got.Region, got.Community.Region, got.Edition)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if loc := rec.Header().Get("Location"); loc != "/products/"+out.Id.String() {
		t.Fatalf("Location = %q, want /products/%s", loc, out.Id)
	}
	if out.Origin == nil || string(*out.Origin) != "community" {
		t.Fatalf("response origin missing: %+v", out.Origin)
	}
	if out.Community == nil || out.Community.PlatformName == nil || *out.Community.PlatformName != "SNES" {
		t.Fatalf("response community block missing: %+v", out.Community)
	}
	if len(got.Community.Developers) != 1 || got.Community.Developers[0] != "Garage Team" {
		t.Fatalf("stored developers = %v, want trimmed with the empty element dropped", got.Community.Developers)
	}
	if len(got.Community.Publishers) != 1 || got.Community.Publishers[0] != "Repro House" {
		t.Fatalf("stored publishers = %v", got.Community.Publishers)
	}
	if out.Community.Developers == nil || len(*out.Community.Developers) != 1 || (*out.Community.Developers)[0] != "Garage Team" {
		t.Fatalf("response developers = %v", out.Community.Developers)
	}
}

// Pins the manual caps the router does not enforce: more than 10
// names or a name over 120 runes is a 400.
func TestUnitCreateCommunityProduct_CreditCaps(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	h := newUnitHandlers(&stubStore{}, &stubGames{}, &stubPrices{}, newStubCache())

	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = fmt.Sprintf("Studio %d", i)
	}
	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Repro Alpha", "developers": eleven})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("eleven developers: %d, want 400", rec.Code)
	}

	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Repro Alpha", "publishers": []string{strings.Repeat("x", 121)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("121-rune publisher: %d, want 400", rec.Code)
	}
}

// Pins the type+name-only mint: with neither platform_name nor
// first_release_date present, Community must stay nil, and the
// response must omit it entirely, not emit an empty object.
func TestUnitCreateCommunityProduct_MinimalBodyOmitsCommunityBlock(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Repro Minimal"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community != nil {
		t.Fatalf("no platform_name/first_release_date must leave Community nil, got %+v", got.Community)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community != nil {
		t.Fatalf("response must omit the community block entirely, got %+v", out.Community)
	}
}

// Pins the hasRegion gate against a whitespace-only region: the gate
// must trim before checking, or " " would slip through and mint an empty block.
func TestUnitCreateCommunityProduct_WhitespaceRegionOmitsCommunityBlock(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Blank Region", "region": "   "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community != nil {
		t.Fatalf("a whitespace-only region alone must leave Community nil, got %+v", got.Community)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community != nil {
		t.Fatalf("response must omit the community block entirely, got %+v", out.Community)
	}
}

// The other side of the same gate: a whitespace-only region beside a
// real fact still builds the block, but the region itself lands empty, not untrimmed.
func TestUnitCreateCommunityProduct_WhitespaceRegionWithOtherFact(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin, map[string]any{
		"type": "game", "name": "Blank Region With Platform", "platform_name": "SNES", "region": "   ",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community == nil || got.Community.PlatformName != "SNES" {
		t.Fatalf("community facts wrong: %+v", got.Community)
	}
	if got.Community.Region != "" {
		t.Fatalf("whitespace-only region must not persist, got %q", got.Community.Region)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community == nil || out.Community.Region != nil {
		t.Fatalf("response region must stay absent, got %+v", out.Community)
	}
}

// Pins the hasPlatformName gate against a whitespace-only value: the
// gate must trim before checking, or it would slip through and mint an empty block.
func TestUnitCreateCommunityProduct_WhitespacePlatformNameOmitsCommunityBlock(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Blank Platform", "platform_name": "   "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community != nil {
		t.Fatalf("a whitespace-only platform_name alone must leave Community nil, got %+v", got.Community)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community != nil {
		t.Fatalf("response must omit the community block entirely, got %+v", out.Community)
	}
}

// The other side of the same gate: a whitespace-only platform_name
// beside a real fact still builds the block, but lands empty, not untrimmed.
func TestUnitCreateCommunityProduct_WhitespacePlatformNameWithOtherFact(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin, map[string]any{
		"type": "game", "name": "Blank Platform With Region", "platform_name": "   ", "region": "NTSC-U",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community == nil || got.Community.Region != "NTSC-U" {
		t.Fatalf("community facts wrong: %+v", got.Community)
	}
	if got.Community.PlatformName != "" {
		t.Fatalf("whitespace-only platform_name must not persist, got %q", got.Community.PlatformName)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community == nil || out.Community.PlatformName != nil {
		t.Fatalf("response platform_name must stay absent, got %+v", out.Community)
	}
}

// Mirrors TestUnitBatchPrices_CapAndBadBody: serveUnit's body param
// always marshals valid JSON, so a malformed payload needs a hand-built request.
func TestUnitCreateCommunityProduct_MalformedBodyIs400(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	// createProduct left nil: a malformed body must 400 before the
	// store is ever touched.
	h := newUnitHandlers(&stubStore{}, &stubGames{}, &stubPrices{}, newStubCache())

	req := httptest.NewRequest(http.MethodPost, "/admin/products", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	router, err := NewRouter(h, env.validator(), slog.New(slog.DiscardHandler), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnitCreateCommunityProduct_Cover(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	// A non-https cover is 400 invalid_body; the store never runs.
	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Repro Cover", "cover_url": "http://img.example/x.jpg"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("http cover: %d, want 400", rec.Code)
	}

	// A valid https cover stores into the community facts block and
	// projects onto community.cover_url.
	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin, map[string]any{
		"type": "game", "name": "Repro Cover", "platform_name": "SNES",
		"cover_url": "https://img.example/rc.jpg",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community == nil || got.Community.CoverURL != "https://img.example/rc.jpg" {
		t.Fatalf("stored cover wrong: %+v", got.Community)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community == nil || out.Community.CoverUrl == nil || *out.Community.CoverUrl != "https://img.example/rc.jpg" {
		t.Fatalf("response cover missing: %+v", out.Community)
	}
}

// Pins the `|| hasCover` gate term: a mint with ONLY a cover must
// still build the community facts block (the sibling cover test
// always pairs cover with platform_name, leaving this term unpinned).
func TestUnitCreateCommunityProduct_CoverOnlyMint(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	var got store.Product
	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		got = p
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin, map[string]any{
		"type": "game", "name": "Cover Only", "cover_url": "https://img.example/co.jpg",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	if got.Community == nil || got.Community.CoverURL != "https://img.example/co.jpg" {
		t.Fatalf("stored cover wrong: %+v", got.Community)
	}
	if got.Community.PlatformName != "" || !got.Community.FirstReleaseDate.IsZero() {
		t.Fatalf("cover-only mint must not fabricate other community fields: %+v", got.Community)
	}
	var out api.Product
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Community == nil || out.Community.CoverUrl == nil || *out.Community.CoverUrl != "https://img.example/co.jpg" {
		t.Fatalf("response cover missing: %+v", out.Community)
	}
}

// Pins the contract's cover_url maxLength:512 boundary through the
// full handler stack.
func TestUnitCreateCommunityProduct_CoverLengthBoundary(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})

	st := &stubStore{createProduct: func(_ context.Context, p store.Product) (store.Product, error) {
		p.ID = uuid.NewString()
		now := time.Now().UTC()
		p.CreatedAt, p.UpdatedAt = now, now
		return p, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	const prefix = "https://img.example/"
	url512 := prefix + strings.Repeat("a", 512-len(prefix))
	if len(url512) != 512 {
		t.Fatalf("fixture length = %d, want 512", len(url512))
	}
	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Boundary 512", "cover_url": url512})
	if rec.Code != http.StatusCreated {
		t.Fatalf("512-char cover: %d %s, want 201", rec.Code, rec.Body.String())
	}

	url513 := url512 + "a"
	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products", admin,
		map[string]any{"type": "game", "name": "Boundary 513", "cover_url": url513})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("513-char cover: %d, want 400", rec.Code)
	}
}

// Pins that a community mint's region lands in the community facts
// block, not the top-level field: community docs carry no provider
// hardware identity, so region is curated, not identity.
func TestCreateCommunityProduct_RegionInBlock(t *testing.T) {
	s := newStack(t)

	resp := s.do(http.MethodPost, "/admin/products", s.adminToken(), map[string]any{
		"type": "game", "name": "PachiPals", "region": "ntsc_j",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint: %d", resp.StatusCode)
	}
	out := decodeBody[api.Product](t, resp)
	if out.Region != nil {
		t.Fatalf("top-level region must stay absent on community mints, got %q", *out.Region)
	}
	if out.Community == nil || out.Community.Region == nil || *out.Community.Region != "ntsc_j" {
		t.Fatalf("response community.region wrong: %+v", out.Community)
	}

	got, err := s.store.GetProduct(context.Background(), out.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Region != "" {
		t.Fatalf("stored top-level region must stay empty, got %q", got.Region)
	}
	if got.Community == nil || got.Community.Region != "ntsc_j" {
		t.Fatalf("stored community.region wrong: %+v", got.Community)
	}
}

func TestPromoteCommunityProduct(t *testing.T) {
	s := newStack(t)
	mint := func(name string) string {
		resp := s.do(http.MethodPost, "/admin/products", s.adminToken(), map[string]any{
			"type": "game", "name": name, "platform_name": "SNES",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mint: %d", resp.StatusCode)
		}
		return decodeBody[api.Product](t, resp).Id.String()
	}
	first := mint("Chrono Trigger Repro A")
	second := mint("Chrono Trigger Repro B")

	resp := s.do(http.MethodPost, "/admin/products/"+first+"/promote", s.userToken(),
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin promote: %d", resp.StatusCode)
	}

	resp = s.do(http.MethodPost, "/admin/products/"+first+"/promote", s.adminToken(),
		map[string]any{"platform_igdb_id": 19})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing igdb_game_id: %d", resp.StatusCode)
	}

	resp = s.do(http.MethodPost, "/admin/products/"+first+"/promote", s.adminToken(),
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("promote: %d", resp.StatusCode)
	}
	promoted := decodeBody[api.Product](t, resp)
	if promoted.Origin != nil {
		t.Fatal("promoted product must not emit origin")
	}
	if promoted.Igdb == nil || promoted.Igdb.GameId != 1011 {
		t.Fatalf("igdb block missing: %+v", promoted.Igdb)
	}
	if promoted.Community == nil || promoted.Community.PlatformName == nil {
		t.Fatal("community facts must be retained as gap-fill")
	}

	resp = s.do(http.MethodPost, "/admin/products/"+first+"/promote", s.adminToken(),
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-promote: %d, want 409 product_not_community", resp.StatusCode)
	}

	// Twin: the (game, platform, no-listing) slot is taken; the index
	// adjudicates and the detail names the holder.
	resp = s.do(http.MethodPost, "/admin/products/"+second+"/promote", s.adminToken(),
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})
	p := reqtest.AssertProblem(t, resp, http.StatusConflict, "identity_taken")
	if !strings.Contains(p.Detail, first) {
		t.Fatalf("twin detail must name the holder: %q", p.Detail)
	}
}

// Pins the console/accessory promote branch: PromoteProduct must
// receive the pc anchor built from the fetched listing
// (verified=true, unlike a plain resolve's anchor), no igdb/platform
// anchors, and the anchor promote must append exactly one snapshot.
func TestUnitPromoteHardware_HappyPathBuildsAnchorAndSnapshots(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "console", Name: "Handheld Mod", Origin: "community"}
	loose := int64(4200)

	var (
		gotID           string
		gotIGDB         *store.IGDBMeta
		gotPlatform     *store.Platform
		gotPC           *store.PCMeta
		snapshotCalled  bool
		gotSnap         store.Snapshot
		getProductCalls int
	)
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) {
			getProductCalls++
			if getProductCalls == 1 {
				return comm, nil
			}
			// The post-promote reload: the handler serves whatever
			// GetProduct now answers (the flipped, anchored state).
			return store.Product{
				ID: comm.ID, Type: "console", Name: comm.Name, Origin: "provider",
				PriceCharting: &store.PCMeta{PCProductID: 5005, PCName: "Handheld Mod", ConsoleName: "Nintendo 64", Current: store.PriceQuote{LooseCents: &loose}},
			}, nil
		},
		promoteProduct: func(_ context.Context, id string, igdbMeta *store.IGDBMeta, platform *store.Platform, pc *store.PCMeta) error {
			gotID, gotIGDB, gotPlatform, gotPC = id, igdbMeta, platform, pc
			return nil
		},
		appendSnapshot: func(_ context.Context, s store.Snapshot) error {
			snapshotCalled = true
			gotSnap = s
			return nil
		},
	}
	prices := &stubPrices{product: func(_ context.Context, id int64) (pricecharting.Product, error) {
		if id != 5005 {
			t.Fatalf("wrong pc product id requested: %d", id)
		}
		return pricecharting.Product{ID: 5005, Name: "Handheld Mod", ConsoleName: "Nintendo 64", LoosePriceCents: &loose}, nil
	}}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"pc_product_id": 5005})
	if rec.Code != http.StatusOK {
		t.Fatalf("hardware promote: %d %s", rec.Code, rec.Body.String())
	}
	if gotID != comm.ID {
		t.Fatalf("promote id = %q, want %q", gotID, comm.ID)
	}
	if gotIGDB != nil || gotPlatform != nil {
		t.Fatalf("hardware promote must carry no igdb/platform anchors: igdb=%+v platform=%+v", gotIGDB, gotPlatform)
	}
	if gotPC == nil || gotPC.PCProductID != 5005 || gotPC.ConsoleName != "Nintendo 64" {
		t.Fatalf("pc anchor wrong: %+v", gotPC)
	}
	if gotPC.MatchConfidence != 1.0 || !gotPC.Verified {
		t.Fatalf("a promote-built pc anchor must be exact and admin-verified: %+v", gotPC)
	}
	if !snapshotCalled {
		t.Fatal("AppendSnapshot must fire for the pc anchor")
	}
	if gotSnap.ProductID != comm.ID || gotSnap.LooseCents == nil || *gotSnap.LooseCents != loose {
		t.Fatalf("snapshot wrong: %+v", gotSnap)
	}
}

// Pins the game branch's unknown-game outcome (raw miss, then a
// provider fetch returning zero games) routing to 404 unknown_game
// via PromoteProduct's own resolveError call site.
func TestUnitPromoteGame_UnknownGameIs404(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "game", Name: "Chrono Trigger Repro", Origin: "community"}
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return comm, nil },
		rawByIDs:   func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) { return nil, nil }}
	h := newUnitHandlers(st, games, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"igdb_game_id": 999999, "platform_igdb_id": 19})

	reqtest.AssertProblemRec(t, rec, http.StatusNotFound, "unknown_game")
}

// Pins the game branch's upstream_unavailable outcome: no raw on
// file and the provider fetch itself failing must answer 502, not the
// 500 a raw-read/DB fault earns (see TestUnitPromoteGame_RawFaultIsInternal).
func TestUnitPromoteGame_ProviderOutageIs502(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "game", Name: "Chrono Trigger Repro", Origin: "community"}
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return comm, nil },
		rawByIDs:   func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return nil, errors.New("igdb down")
	}}
	h := newUnitHandlers(st, games, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})

	reqtest.AssertProblemRec(t, rec, http.StatusBadGateway, "upstream_unavailable")
}

// Pins the physical-catalog gate on promote: the same platform check
// as resolve rejects a digital-only platform (regionkit.DigitalOnlyPlatformIDs).
func TestUnitPromoteGame_DigitalOnlyPlatformRejected(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "game", Name: "Genshin Impact Repro", Origin: "community"}
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return comm, nil },
		rawByIDs:   func(context.Context, []int64) ([]store.RawGame, error) { return nil, nil },
		upsertRaw:  func(context.Context, []igdb.Game, time.Time) error { return nil },
	}
	games := &stubGames{gamesByIDs: func(context.Context, []int64) ([]igdb.Game, error) {
		return []igdb.Game{{ID: 119277, Name: "Genshin Impact",
			Platforms: []igdb.Named{{ID: 39, Name: "iOS"}, {ID: 48, Name: "PlayStation 4"}}}}, nil
	}}
	h := newUnitHandlers(st, games, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"igdb_game_id": 119277, "platform_igdb_id": 39})

	pb := reqtest.AssertProblemRec(t, rec, http.StatusBadRequest, "invalid_body")
	if !strings.Contains(pb.Detail, "physical") {
		t.Fatalf("detail must name the physical-only gate, got %q", pb.Detail)
	}
}

// Pins the hardware branch's unknown-listing outcome: the pc anchor
// fetch answering pricecharting.ErrNotFound maps to 404
// unknown_pc_product, a distinct call site from /products/resolve's.
func TestUnitPromoteHardware_UnknownPCProductIs404(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "console", Name: "Handheld Mod", Origin: "community"}
	st := &stubStore{getProduct: func(context.Context, string) (store.Product, error) { return comm, nil }}
	prices := &stubPrices{product: func(context.Context, int64) (pricecharting.Product, error) {
		return pricecharting.Product{}, pricecharting.ErrNotFound
	}}
	h := newUnitHandlers(st, &stubGames{}, prices, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"pc_product_id": 999999})

	reqtest.AssertProblemRec(t, rec, http.StatusNotFound, "unknown_pc_product")
}

// Pins the promote game branch's non-resolveErr fallthrough to 500:
// a raw-read store fault is internal, never the 502 a provider outage earns.
func TestUnitPromoteGame_RawFaultIsInternal(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	comm := store.Product{ID: uuid.NewString(), Type: "game", Name: "Chrono Trigger Repro", Origin: "community"}
	// rawByIDs faults with a plain error, which gamePayloadFor wraps as
	// a non-resolveErr; games/prices stay empty since reaching a provider would be wrong.
	st := &stubStore{
		getProduct: func(context.Context, string) (store.Product, error) { return comm, nil },
		rawByIDs:   func(context.Context, []int64) ([]store.RawGame, error) { return nil, errors.New("store unreachable") },
	}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodPost, "/admin/products/"+comm.ID+"/promote", admin,
		map[string]any{"igdb_game_id": 1011, "platform_igdb_id": 19})

	// AssertProblemRec's exact code match rules out the upstream_unavailable
	// classification a provider outage would earn.
	reqtest.AssertProblemRec(t, rec, http.StatusInternalServerError, "internal")
}

func TestUnitPromoteCandidates_ListAndDismiss(t *testing.T) {
	env := newAuthEnv(t)
	admin := env.token(t, uuid.NewString(), []string{"user", "admin"})
	user := env.token(t, uuid.NewString(), []string{"user"})
	now := time.Now().UTC()
	flagged := store.Product{
		ID: uuid.NewString(), Type: "console", Name: "Handheld Mod", Origin: "community",
		PromoteCandidates: []store.PromoteCandidate{{Provider: "pricecharting", ProviderID: 5005, Name: "Super Mario 64", Score: 0.9, FoundAt: now}},
		CreatedAt:         now, UpdatedAt: now,
	}
	var dismissed struct {
		id, provider string
		providerID   int64
	}
	st := &stubStore{
		listPromoteCandidateProducts: func(_ context.Context, limit, offset int, productID string) ([]store.Product, int64, error) {
			return []store.Product{flagged}, 1, nil
		},
		dismissPromoteCandidate: func(_ context.Context, id, provider string, providerID int64) error {
			dismissed.id, dismissed.provider, dismissed.providerID = id, provider, providerID
			return nil
		},
	}
	h := newUnitHandlers(st, &stubGames{}, &stubPrices{}, newStubCache())

	rec := serveUnit(t, h, env, http.MethodGet, "/admin/products/promote-candidates", user, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list: %d", rec.Code)
	}
	rec = serveUnit(t, h, env, http.MethodGet, "/admin/products/promote-candidates", admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var page api.PromoteCandidatesPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 1 || len(page.Products) != 1 || len(page.Products[0].Candidates) != 1 {
		t.Fatalf("page shape: %+v", page)
	}

	rec = serveUnit(t, h, env, http.MethodPost, "/admin/products/"+flagged.ID+"/promote-candidates/dismiss", admin,
		map[string]any{"provider": "pricecharting", "provider_id": 5005})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body.String())
	}
	if dismissed.id != flagged.ID || dismissed.provider != "pricecharting" || dismissed.providerID != 5005 {
		t.Fatalf("dismiss args: %+v", dismissed)
	}
}
