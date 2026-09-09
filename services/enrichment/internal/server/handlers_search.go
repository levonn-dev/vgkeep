// Catalog search: provider search with cache-first reads, the
// degraded local-catalog fallback, and the community-lane interleave.

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/levonn-dev/vgkeep/libs/go/contract/common"
	"github.com/levonn-dev/vgkeep/libs/go/regionkit"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/gen/api"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/igdb"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/match"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/store"
)

const searchLimit = 20

// communityLaneLimit caps the search community lane (the contract's
// documented cap).
const communityLaneLimit = 10

// normQuery folds a query for cache keying (the provider gets the
// trimmed original).
func normQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// matchNamesFor returns the auto-match target forms for a game in an
// entry region: the chained transliteration first when a bundle has
// one (primary query), else just the canonical name.
func matchNamesFor(g igdb.Game, region string) []string {
	for _, id := range regionQueryChains[region] {
		for _, b := range igdb.BundleLocalizations(g) {
			if b.Region == id && b.Translit != "" {
				return []string{b.Translit, g.Name}
			}
		}
	}
	return []string{g.Name}
}

// matchCandidates adapts provider search rows to scoring candidates.
func matchCandidates(results []common.SearchResult) []match.Candidate {
	cands := make([]match.Candidate, 0, len(results))
	for _, r := range results {
		if r.PcProductId == nil || r.ConsoleName == nil {
			continue
		}
		cands = append(cands, match.Candidate{PCProductID: *r.PcProductId, Name: r.Name, ConsoleName: *r.ConsoleName})
	}
	return cands
}

// SearchCatalog is the discovery search: cache in front of the
// provider, never DB-first (catalog is incomplete by construction).
// Provider-down + cold-cache degrades to a local name match, uncached.
func (h *Handlers) SearchCatalog(w http.ResponseWriter, r *http.Request, params api.SearchCatalogParams) {
	ctx := r.Context()
	// q's blank-after-trim guard stays: minLength:1 catches empty but
	// not whitespace-only; type's enum is specval's job.
	q := strings.TrimSpace(params.Q)
	if q == "" {
		problem(w, r, http.StatusBadRequest, "invalid_param", "q must not be empty")
		return
	}
	kind := string(params.Type)
	nq := normQuery(q)

	var out api.SearchResults
	if body, err := h.cache.GetSearch(ctx, kind, nq); err != nil {
		h.failOpen(ctx, "search_get", err)
	} else if body != nil {
		if err := json.Unmarshal(body, &out); err != nil {
			h.failOpen(ctx, "search_decode", err)
		} else {
			h.countSearch(ctx, kind, "cache")
			h.interleaveCommunityResults(ctx, w, kind, q, out)
			return
		}
	}

	var (
		results []common.SearchResult
		perr    error
	)
	switch kind {
	case "game":
		results, perr = h.searchGames(ctx, q)
	case "hardware":
		results, perr = h.searchHardware(ctx, q)
	default:
		results, perr = h.searchPCListings(ctx, q)
	}
	degraded := perr != nil
	if degraded {
		h.logger.WarnContext(ctx, "search provider unavailable; serving local catalog match", "kind", kind, "err", perr)
		var types []string
		switch kind {
		case "game":
			types = []string{"game"}
		case "hardware":
			types = []string{"console", "accessory"}
		default: // pc_listing: any provider product type may carry a listing mapping
			types = []string{"game", "console", "accessory", "pc_listing"}
		}
		local, err := h.store.SearchByName(ctx, types, q, searchLimit)
		if err != nil {
			h.internalError(w, r, "search_local_fallback", "search failed", err)
			return
		}
		results = localResults(kind, local)
		h.countSearch(ctx, kind, "degraded")
	} else {
		h.countSearch(ctx, kind, "provider")
	}
	if results == nil {
		results = []common.SearchResult{}
	}

	out = api.SearchResults{Degraded: degraded, Results: results}
	body, err := json.Marshal(out)
	if err != nil {
		h.internalError(w, r, "search_encode", "encoding failed", err)
		return
	}
	// Degraded answers are never cached: the next request should try
	// the provider again.
	if !degraded {
		if err := h.cache.PutSearch(ctx, kind, nq, body, h.searchTTL); err != nil {
			h.failOpen(ctx, "search_put", err)
		}
	}
	h.interleaveCommunityResults(ctx, w, kind, q, out)
}

// communityResult maps an admin-minted community product onto the
// unified search shape. type stays the game/hardware discriminator;
// item_type carries the finer kind; origin marks the row as community.
func communityResult(p store.Product) common.SearchResult {
	res := common.SearchResult{Name: p.Name}
	if p.Type == "game" {
		res.Type = "game"
	} else {
		res.Type = "hardware"
	}
	o := common.ProductOrigin("community")
	res.Origin = &o
	if id, err := uuid.Parse(p.ID); err == nil {
		res.ProductId = &id
	}
	it := common.ItemType(p.Type)
	res.ItemType = &it
	if p.Community != nil {
		if p.Community.PlatformName != "" {
			pn := p.Community.PlatformName
			res.PlatformName = &pn
		}
		if p.Community.CoverURL != "" {
			cu := p.Community.CoverURL
			res.CoverUrl = &cu
		}
		if p.Community.Region != "" {
			rg := p.Community.Region
			res.Region = &rg
		}
		if !p.Community.FirstReleaseDate.IsZero() {
			fd := openapi_types.Date{Time: p.Community.FirstReleaseDate}
			res.FirstReleaseDate = &fd
		}
		if len(p.Community.Developers) > 0 {
			devs := p.Community.Developers
			res.Developers = &devs
		}
		if len(p.Community.Publishers) > 0 {
			pubs := p.Community.Publishers
			res.Publishers = &pubs
		}
	}
	return res
}

// interleaveCommunityResults merges the community lane into results:
// community mints score by the same name similarity as provider
// order and merge in descending score (ties favor the provider row).
// Runs on the by-value copy AFTER cache resolution, so the provider
// cache stays provider-only and a fresh mint appears immediately.
func (h *Handlers) interleaveCommunityResults(ctx context.Context, w http.ResponseWriter, kind, q string, out api.SearchResults) {
	var types []string
	switch kind {
	case "game":
		types = []string{"game"}
	case "hardware":
		types = []string{"console", "accessory"}
	default: // pc_listing picks price anchors; community products have none
		writeJSON(w, http.StatusOK, out)
		return
	}
	comm, err := h.store.SearchCommunityProducts(ctx, types, q, communityLaneLimit)
	if err != nil {
		// Fail open: the community lane is an optional overlay, so a
		// store fault degrades to the provider-only answer already in out.
		h.failOpen(ctx, "community_search", err)
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(comm) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	type scored struct {
		res      common.SearchResult
		score    float64
		provider bool
	}
	merged := make([]scored, 0, len(out.Results)+len(comm))
	for _, res := range out.Results {
		merged = append(merged, scored{res: res, score: match.Score(q, res.Name), provider: true})
	}
	for _, p := range comm {
		merged = append(merged, scored{res: communityResult(p), score: match.Score(q, p.Name), provider: false})
	}
	// Descending score; ties favor the provider row. SliceStable keeps
	// provider order and the store's name-asc community order otherwise.
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].score != merged[j].score {
			return merged[i].score > merged[j].score
		}
		return merged[i].provider && !merged[j].provider
	})
	results := make([]common.SearchResult, 0, len(merged))
	for _, m := range merged {
		results = append(results, m.res)
	}
	out.Results = results
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) searchGames(ctx context.Context, q string) ([]common.SearchResult, error) {
	games, err := h.games.SearchGames(ctx, q, searchLimit)
	if err != nil {
		return nil, err
	}
	// Non-latin queries get the supplementary localization leg (see
	// hasNonLatinLetter); a leg/fetch failure just serves primary results.
	if hasNonLatinLetter(q) {
		ids, lerr := h.games.SearchLocalizations(ctx, q, searchLimit)
		switch {
		case lerr != nil:
			h.logger.WarnContext(ctx, "localization search leg failed; serving primary results", "err", lerr)
			h.countLocalizationLeg(ctx, "error")
		case len(ids) == 0:
			h.countLocalizationLeg(ctx, "empty")
		default:
			have := make(map[int64]bool, len(games))
			for _, g := range games {
				have[g.ID] = true
			}
			var missing []int64
			for _, id := range ids {
				if !have[id] {
					missing = append(missing, id)
				}
			}
			if len(missing) > 0 {
				extra, gerr := h.games.GamesByIDs(ctx, missing)
				if gerr != nil {
					h.logger.WarnContext(ctx, "localization leg fetch failed; serving primary results", "err", gerr)
					h.countLocalizationLeg(ctx, "error")
				} else {
					games = append(games, extra...)
					h.countLocalizationLeg(ctx, "merged")
				}
			} else {
				h.countLocalizationLeg(ctx, "merged")
			}
		}
	}
	games = physicalOnly(games)
	games = rankExactFirst(q, games)
	if len(games) > searchLimit {
		games = games[:searchLimit]
	}
	out := make([]common.SearchResult, 0, len(games))
	for _, g := range games {
		res := gameResult(g)
		if mr := matchedRegion(q, g); mr != "" {
			res.MatchedRegion = &mr
		}
		out = append(out, res)
	}
	return out, nil
}

// physicalOnly prunes digital-only platforms (regionkit.DigitalOnlyPlatformIDs)
// from each game's platform list and drops games left with none; a
// game IGDB lists without platforms at all passes through unchanged.
func physicalOnly(games []igdb.Game) []igdb.Game {
	out := make([]igdb.Game, 0, len(games))
	for _, g := range games {
		if len(g.Platforms) == 0 {
			out = append(out, g)
			continue
		}
		kept := make([]igdb.Named, 0, len(g.Platforms))
		for _, p := range g.Platforms {
			if !regionkit.DigitalOnlyPlatformIDs[p.ID] {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			continue
		}
		g.Platforms = kept
		out = append(out, g)
	}
	return out
}

// rankExactFirst floats exact-name matches (normalized: brackets,
// articles, possessives fold) to the top, since IGDB ranks loosely on
// exactness; exacts sort by rating count so the known release leads.
func rankExactFirst(q string, games []igdb.Game) []igdb.Game {
	exactName := func(g igdb.Game) bool {
		if match.SameName(g.Name, q) {
			return true
		}
		for _, b := range igdb.BundleLocalizations(g) {
			if (b.Name != "" && match.SameName(b.Name, q)) || (b.Translit != "" && match.SameName(b.Translit, q)) {
				return true
			}
		}
		return false
	}
	exact := make([]igdb.Game, 0, len(games))
	rest := make([]igdb.Game, 0, len(games))
	for _, g := range games {
		if exactName(g) {
			exact = append(exact, g)
		} else {
			rest = append(rest, g)
		}
	}
	sort.SliceStable(exact, func(i, j int) bool {
		return exact[i].TotalRatingCount > exact[j].TotalRatingCount
	})
	return append(exact, rest...)
}

// matchedRegion reports which region's localized title recognized the
// query, or "" if the canonical name did (or nothing did). Containment
// over normQuery-folded strings, never the Dice scorer (whitespace-token
// shaped, can't grade CJK). Guards: 3+ runes latin, 2+ non-latin.
func matchedRegion(q string, g igdb.Game) string {
	nq := normQuery(q)
	minRunes := 3
	if !asciiOnlyQuery(nq) {
		minRunes = 2
	}
	if utf8.RuneCountInString(nq) < minRunes {
		return ""
	}
	contains := func(name string) bool {
		nn := normQuery(name)
		return nn != "" && (strings.Contains(nn, nq) || strings.Contains(nq, nn))
	}
	if contains(g.Name) {
		return ""
	}
	for _, b := range igdb.BundleLocalizations(g) {
		if (b.Name != "" && contains(b.Name)) || (b.Translit != "" && contains(b.Translit)) {
			return b.Region
		}
	}
	return ""
}

func asciiOnlyQuery(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// hasNonLatinLetter gates the supplementary localization leg: IGDB
// already matches latin names, so only non-latin queries pay the extra call.
func hasNonLatinLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return true
		}
	}
	return false
}

// platformReleaseRegions returns the distinct canonical regions this
// game released in on one platform, ordered by earliest release date
// (dateless sorts last, alpha tiebreak). Unlike platformReleaseDates,
// this is platform-exact: JP twins (Famicom/NES, Super Famicom/SNES)
// are NOT folded here, since a search result badges the actual physical release.
func platformReleaseRegions(g igdb.Game, platformID int64) []string {
	type regionSpan struct {
		earliest time.Time
		hasDate  bool
	}
	byRegion := map[string]*regionSpan{}
	var order []string
	for _, rd := range g.ReleaseDates {
		// A platform-0 row matches no real platform; skipping it also
		// defends a platformID of 0 from matching every unplatformed row.
		if rd.Platform == 0 || rd.Platform != platformID {
			continue
		}
		name, ok := igdb.RegionName(rd.Region)
		if !ok {
			continue
		}
		span, seen := byRegion[name]
		if !seen {
			span = &regionSpan{}
			byRegion[name] = span
			order = append(order, name)
		}
		if rd.Date != 0 {
			d := time.Unix(rd.Date, 0).UTC().Truncate(24 * time.Hour)
			if !span.hasDate || d.Before(span.earliest) {
				span.earliest = d
				span.hasDate = true
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := byRegion[order[i]], byRegion[order[j]]
		if a.hasDate != b.hasDate {
			return a.hasDate // dated regions before dateless ones
		}
		if a.hasDate && !a.earliest.Equal(b.earliest) {
			return a.earliest.Before(b.earliest)
		}
		return order[i] < order[j]
	})
	return order
}

func gameResult(g igdb.Game) common.SearchResult {
	res := common.SearchResult{Type: common.SearchResultType("game"), Name: g.Name}
	id := g.ID
	res.IgdbGameId = &id
	if len(g.Platforms) > 0 {
		prs := make([]common.PlatformRef, 0, len(g.Platforms))
		for _, p := range g.Platforms {
			pr := common.PlatformRef{IgdbPlatformId: p.ID, Name: p.Name}
			// platformReleaseRegions stays plain []string (pure, testable);
			// the wire enum conversion happens only here.
			if regions := platformReleaseRegions(g, p.ID); len(regions) > 0 {
				wire := make([]common.ReleaseRegion, len(regions))
				for i, r := range regions {
					wire[i] = common.ReleaseRegion(r)
				}
				pr.ReleaseRegions = &wire
			}
			prs = append(prs, pr)
		}
		res.Platforms = &prs
	}
	if d := g.ReleaseDate(); !d.IsZero() {
		fd := openapi_types.Date{Time: d}
		res.FirstReleaseDate = &fd
	}
	if cu := g.CoverURL(); cu != "" {
		res.CoverUrl = &cu
	}
	if bundles := igdb.BundleLocalizations(g); len(bundles) > 0 {
		locs := make([]common.Localization, 0, len(bundles))
		for _, b := range bundles {
			al := common.Localization{Region: b.Region}
			if b.Name != "" {
				n := b.Name
				al.Name = &n
			}
			if b.Translit != "" {
				tr := b.Translit
				al.Translit = &tr
			}
			if b.CoverURL != "" {
				cu := b.CoverURL
				al.CoverUrl = &cu
			}
			locs = append(locs, al)
		}
		res.Localizations = &locs
	}
	return res
}

func isHardwareCategory(genre string) bool {
	switch genre {
	case "Systems", "Controllers", "Accessories":
		return true
	}
	return false
}

func (h *Handlers) searchHardware(ctx context.Context, q string) ([]common.SearchResult, error) {
	prods, err := h.prices.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]common.SearchResult, 0, len(prods))
	for _, p := range prods {
		if !isHardwareCategory(p.Genre) {
			continue
		}
		id, console, cat := p.ID, p.ConsoleName, p.Genre
		out = append(out, common.SearchResult{
			Type: "hardware", Name: p.Name,
			PcProductId: &id, ConsoleName: &console, Category: &cat,
		})
		if len(out) == searchLimit {
			break
		}
	}
	return out, nil
}

// searchPCListings is the all-of-PriceCharting search behind the
// proxy picker: no category filter (surfaces variant rows IGDB doesn't separate).
func (h *Handlers) searchPCListings(ctx context.Context, q string) ([]common.SearchResult, error) {
	// The provider's tokenizer misses possessive-less listing names when
	// the query keeps the possessive, so every pc_listing query drops it.
	prods, err := h.prices.Search(ctx, match.ProviderQuery(q))
	if err != nil {
		return nil, err
	}
	out := make([]common.SearchResult, 0, len(prods))
	for _, p := range prods {
		out = append(out, pcListingResult(p.ID, p.Name, p.ConsoleName, p.Genre, quoteOf(p)))
		if len(out) == searchLimit {
			break
		}
	}
	return out, nil
}

// pcListingResult maps one PC listing (live or a product's cached
// mapping on the degraded path) onto the wire shape.
func pcListingResult(id int64, name, console, category string, q store.PriceQuote) common.SearchResult {
	res := common.SearchResult{
		Type: common.SearchResultType("pc_listing"), Name: name,
		PcProductId: &id, ConsoleName: &console,
	}
	if category != "" {
		res.Category = &category
	}
	res.LooseCents = q.LooseCents
	res.CibCents = q.CIBCents
	res.NewCents = q.NewCents
	return res
}

// localResults maps catalog products onto search results for the
// degraded path.
func localResults(kind string, prods []store.Product) []common.SearchResult {
	out := make([]common.SearchResult, 0, len(prods))
	if kind == "pc_listing" {
		// Degraded: any product's stored mapping is a known listing. A
		// game/hardware product can share pc_product_id with a separate
		// pc_listing anchor (independent resolves), so de-dupe keeps one row.
		seen := make(map[int64]bool, len(prods))
		for _, p := range prods {
			if p.PriceCharting == nil || seen[p.PriceCharting.PCProductID] {
				continue
			}
			seen[p.PriceCharting.PCProductID] = true
			pc := p.PriceCharting
			out = append(out, pcListingResult(pc.PCProductID, pc.PCName, pc.ConsoleName, "", pc.Current))
		}
		return out
	}
	for _, p := range prods {
		isGame := p.Type == "game"
		if (kind == "game") != isGame {
			continue
		}
		if isGame {
			res := common.SearchResult{Type: common.SearchResultType("game"), Name: p.Name}
			if p.IGDB != nil {
				id := p.IGDB.GameID
				res.IgdbGameId = &id
				if p.IGDB.CoverURL != "" {
					cu := p.IGDB.CoverURL
					res.CoverUrl = &cu
				}
				if !p.IGDB.FirstReleaseDate.IsZero() {
					fd := openapi_types.Date{Time: p.IGDB.FirstReleaseDate}
					res.FirstReleaseDate = &fd
				}
			}
			if p.Platform != nil {
				pr := common.PlatformRef{IgdbPlatformId: p.Platform.IGDBID, Name: p.Platform.Name}
				if p.Platform.LogoURL != "" {
					lu := p.Platform.LogoURL
					pr.LogoUrl = &lu
				}
				prs := []common.PlatformRef{pr}
				res.Platforms = &prs
			}
			out = append(out, res)
			continue
		}
		res := common.SearchResult{Type: common.SearchResultType("hardware"), Name: p.Name}
		if p.PriceCharting != nil {
			id := p.PriceCharting.PCProductID
			console := p.PriceCharting.ConsoleName
			res.PcProductId = &id
			res.ConsoleName = &console
		}
		out = append(out, res)
	}
	return out
}

// searchPCListingsCached is the resolve-side twin of the pc_listing
// search endpoint: same cache key/shape, same never-cache-on-failure
// discipline. Repeat adds of a family hit cache instead of the provider.
func (h *Handlers) searchPCListingsCached(ctx context.Context, q string) ([]common.SearchResult, error) {
	nq := normQuery(q)
	if body, err := h.cache.GetSearch(ctx, "pc_listing", nq); err != nil {
		h.failOpen(ctx, "search_get", err)
	} else if body != nil {
		var res api.SearchResults
		if err := json.Unmarshal(body, &res); err == nil {
			return res.Results, nil
		}
		// A malformed cache entry reads as a miss.
	}
	results, err := h.searchPCListings(ctx, q)
	if err != nil {
		return nil, err
	}
	if results == nil {
		results = []common.SearchResult{}
	}
	body, err := json.Marshal(api.SearchResults{Degraded: false, Results: results})
	if err != nil {
		return results, nil // still usable for scoring; just not cached
	}
	if err := h.cache.PutSearch(ctx, "pc_listing", nq, body, h.searchTTL); err != nil {
		h.failOpen(ctx, "search_put", err)
	}
	return results, nil
}
