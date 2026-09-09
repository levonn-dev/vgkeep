// Community products: admin mints, the unmatched/community/promote
// worklists, and promoting a community product onto provider anchors.

package server

import (
	"errors"
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/levonn-dev/vgkeep/libs/go/catalogval"
	"github.com/levonn-dev/vgkeep/libs/go/contract/common"
	"github.com/levonn-dev/vgkeep/libs/go/httpkit"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/gen/api"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/pricecharting"
	"github.com/levonn-dev/vgkeep/services/enrichment/internal/store"
)

// CreateCommunityProduct mints an anchor-less product from an
// approved catalog submission. Community identity is the curated
// name (no uniqueness machinery); the review panel's search is the dedup check.
func (h *Handlers) CreateCommunityProduct(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req api.CommunityProductSpec
	if !httpkit.DecodeBody(w, r, 16*1024, &req) {
		return
	}
	// type/developers/publishers/cover_url validation is specval's job.
	// name keeps its blank-after-trim guard: minLength:1 misses "   ".
	name := strings.TrimSpace(req.Name)
	if name == "" {
		problem(w, r, http.StatusBadRequest, "invalid_body", "name must not be empty")
		return
	}
	devs := catalogval.NormalizeCredits(req.Developers)
	pubs := catalogval.NormalizeCredits(req.Publishers)
	p := store.Product{Type: string(req.Type), Name: name, Origin: "community"}
	if req.Edition != nil {
		p.Edition = *req.Edition
	}
	hasCover := req.CoverUrl != nil && *req.CoverUrl != ""
	// Trimmed: a whitespace-only region must not by itself earn an
	// otherwise-empty community block (cm.Region below trims too).
	hasRegion := req.Region != nil && strings.TrimSpace(*req.Region) != ""
	// Same: a whitespace-only platform_name must not earn the block alone.
	hasPlatformName := req.PlatformName != nil && strings.TrimSpace(*req.PlatformName) != ""
	if hasPlatformName || req.FirstReleaseDate != nil || hasCover || hasRegion || devs != nil || pubs != nil {
		cm := &store.CommunityMeta{Developers: devs, Publishers: pubs}
		if hasPlatformName {
			cm.PlatformName = strings.TrimSpace(*req.PlatformName)
		}
		if hasRegion {
			cm.Region = strings.TrimSpace(*req.Region)
		}
		if req.FirstReleaseDate != nil {
			cm.FirstReleaseDate = req.FirstReleaseDate.Time
		}
		if hasCover {
			cm.CoverURL = *req.CoverUrl
		}
		p.Community = cm
	}
	created, err := h.store.CreateProduct(r.Context(), p)
	if err != nil {
		h.internalError(w, r, "community_product_create", "create failed", err)
		return
	}
	w.Header().Set("Location", "/products/"+created.ID)
	writeJSON(w, http.StatusCreated, toAPIProduct(created))
}

// ListUnmatchedProducts serves the admin worklist: every unmatched
// product, including held ones (surfacing deliberate clears is the point).
func (h *Handlers) ListUnmatchedProducts(w http.ResponseWriter, r *http.Request, params api.ListUnmatchedProductsParams) {
	if !h.requireAdmin(w, r) {
		return
	}
	// limit/offset are already within the contract's 1-500/>=0 bounds
	// (specval's job); only the default-when-absent case is handled here.
	limit := 200
	if params.Limit != nil {
		limit = *params.Limit
	}
	offset := 0
	if params.Offset != nil {
		offset = *params.Offset
	}
	prods, total, err := h.store.ListUnmatchedProducts(r.Context(), limit, offset)
	if err != nil {
		h.internalError(w, r, "unmatched_products_list", "list failed", err)
		return
	}
	page := api.UnmatchedProductsPage{Products: make([]api.Product, 0, len(prods)), TotalCount: total}
	for _, p := range prods {
		page.Products = append(page.Products, toAPIProduct(p))
	}
	writeJSON(w, http.StatusOK, page)
}

// ListCommunityProducts serves the admin community listing: every
// un-promoted community product, so an admin can find and remove ones
// no entry references. Filters on origin, not mapping absence.
func (h *Handlers) ListCommunityProducts(w http.ResponseWriter, r *http.Request, params api.ListCommunityProductsParams) {
	if !h.requireAdmin(w, r) {
		return
	}
	// See ListUnmatchedProducts: bounds are specval's job; only the
	// default-when-absent case is left.
	limit := 200
	if params.Limit != nil {
		limit = *params.Limit
	}
	offset := 0
	if params.Offset != nil {
		offset = *params.Offset
	}
	prods, total, err := h.store.ListCommunityProductsPage(r.Context(), limit, offset)
	if err != nil {
		h.internalError(w, r, "community_products_list", "list failed", err)
		return
	}
	page := api.CommunityProductsPage{Products: make([]api.Product, 0, len(prods)), TotalCount: total}
	for _, p := range prods {
		page.Products = append(page.Products, toAPIProduct(p))
	}
	writeJSON(w, http.StatusOK, page)
}

// ListPromoteCandidates pages the sweep's flagged community products,
// strongest candidate first.
func (h *Handlers) ListPromoteCandidates(w http.ResponseWriter, r *http.Request, params api.ListPromoteCandidatesParams) {
	if !h.requireAdmin(w, r) {
		return
	}
	// See ListUnmatchedProducts: bounds are specval's job; only the
	// default-when-absent case is left.
	limit := 200
	if params.Limit != nil {
		limit = *params.Limit
	}
	offset := 0
	if params.Offset != nil {
		offset = *params.Offset
	}
	productID := ""
	if params.ProductId != nil {
		productID = params.ProductId.String()
	}
	prods, total, err := h.store.ListPromoteCandidateProducts(r.Context(), limit, offset, productID)
	if err != nil {
		h.internalError(w, r, "promote_candidates_list", "list failed", err)
		return
	}
	page := api.PromoteCandidatesPage{Products: make([]common.PromoteCandidateProduct, 0, len(prods)), TotalCount: total}
	for _, p := range prods {
		row := common.PromoteCandidateProduct{Product: toAPIProduct(p), Candidates: make([]common.PromoteCandidate, 0, len(p.PromoteCandidates))}
		for _, c := range p.PromoteCandidates {
			row.Candidates = append(row.Candidates, common.PromoteCandidate{
				Provider: common.CatalogProvider(c.Provider), ProviderId: c.ProviderID,
				Name: c.Name, Score: c.Score, FoundAt: c.FoundAt,
			})
		}
		page.Products = append(page.Products, row)
	}
	writeJSON(w, http.StatusOK, page)
}

// DismissPromoteCandidate silences one candidate pair permanently.
func (h *Handlers) DismissPromoteCandidate(w http.ResponseWriter, r *http.Request, productId openapi_types.UUID) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req api.DismissCandidateRequest
	if !httpkit.DecodeBody(w, r, 16*1024, &req) {
		return
	}
	// provider's enum is specval's job now.
	err := h.store.DismissPromoteCandidate(r.Context(), productId.String(), string(req.Provider), req.ProviderId)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, r, http.StatusNotFound, "product_not_found", "no such product")
		return
	}
	if err != nil {
		h.internalError(w, r, "promote_candidate_dismiss", "dismiss failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PromoteProduct attaches provider anchors to a community product and
// flips it to provider origin in place: the id stays stable. A twin
// identity answers 409 with the holder named; true merge is not automated.
func (h *Handlers) PromoteProduct(w http.ResponseWriter, r *http.Request, productId openapi_types.UUID) {
	ctx := r.Context()
	if !h.requireAdmin(w, r) {
		return
	}
	var req api.PromoteRequest
	if !httpkit.DecodeBody(w, r, 16*1024, &req) {
		return
	}
	id := productId.String()
	prod, err := h.store.GetProduct(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, r, http.StatusNotFound, "product_not_found", "no such product")
		return
	} else if err != nil {
		h.internalError(w, r, "promote_product_get", "get failed", err)
		return
	}
	if prod.Origin != "community" {
		problem(w, r, http.StatusConflict, "product_not_community",
			"only community products promote; provider products use the mapping fix")
		return
	}

	var (
		igdbMeta *store.IGDBMeta
		platform *store.Platform
		pcMeta   *store.PCMeta
	)
	switch prod.Type {
	case "game":
		if req.IgdbGameId == nil || req.PlatformIgdbId == nil {
			problem(w, r, http.StatusBadRequest, "invalid_body", "game promotion requires igdb_game_id and platform_igdb_id")
			return
		}
		g, fetchedAt, gerr := h.gamePayloadFor(ctx, *req.IgdbGameId)
		if gerr != nil {
			// Same taxonomy the resolve flow consumes: *resolveErr carries
			// unknown_game/upstream_unavailable, a raw fault is internal.
			// Reuse resolveError rather than re-deriving that mapping.
			h.resolveError(w, r, gerr)
			return
		}
		p, perr := platformOf(g, *req.PlatformIgdbId)
		if perr != nil {
			h.resolveError(w, r, perr)
			return
		}
		platform = p
		meta := store.NewIGDBMeta(g, *req.PlatformIgdbId, fetchedAt)
		igdbMeta = &meta
	case "console", "accessory":
		if req.PcProductId == nil {
			problem(w, r, http.StatusBadRequest, "invalid_body", "hardware promotion requires pc_product_id")
			return
		}
	default:
		problem(w, r, http.StatusBadRequest, "invalid_body", "only game, console and accessory products promote")
		return
	}
	if req.PcProductId != nil {
		pc, perr := h.prices.Product(ctx, *req.PcProductId)
		if errors.Is(perr, pricecharting.ErrNotFound) {
			problem(w, r, http.StatusNotFound, "unknown_pc_product", "no such pricecharting product")
			return
		}
		if perr != nil {
			problem(w, r, http.StatusBadGateway, "upstream_unavailable", "price provider unavailable")
			return
		}
		pcMeta = &store.PCMeta{
			PCProductID: pc.ID, PCName: pc.Name, ConsoleName: pc.ConsoleName,
			MatchConfidence: 1.0, Verified: true,
			Current: quoteOf(pc), AsOf: h.now(),
		}
	}

	err = h.store.PromoteProduct(ctx, id, igdbMeta, platform, pcMeta)
	if errors.Is(err, store.ErrIdentityTaken) {
		key := store.ProductKey{Type: prod.Type, Region: prod.Region, Edition: prod.Edition, Variant: prod.Variant}
		if req.IgdbGameId != nil {
			key.IGDBGameID = *req.IgdbGameId
		}
		if req.PlatformIgdbId != nil {
			key.PlatformIGDBID = *req.PlatformIgdbId
		}
		if req.PcProductId != nil {
			key.PCProductID = *req.PcProductId
		}
		problem(w, r, http.StatusConflict, "identity_taken",
			withHolder(ctx, h.store, "a provider product already holds that identity", key))
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		problem(w, r, http.StatusNotFound, "product_not_found", "no such product")
		return
	}
	if err != nil {
		h.internalError(w, r, "promote_product_write", "promote failed", err)
		return
	}
	if pcMeta != nil {
		if err := h.store.AppendSnapshot(ctx, store.Snapshot{
			ProductID: id, CapturedAt: pcMeta.AsOf,
			LooseCents: pcMeta.Current.LooseCents, CIBCents: pcMeta.Current.CIBCents, NewCents: pcMeta.Current.NewCents,
		}); err != nil {
			h.logger.WarnContext(ctx, "promote snapshot failed", "product", id, "err", err)
		}
	}
	if err := h.cache.InvalidateProduct(ctx, id); err != nil {
		h.failOpen(ctx, "promote_invalidate", err)
	}
	p, err := h.store.GetProduct(ctx, id)
	if err != nil {
		h.internalError(w, r, "promote_product_reload", "reload failed", err)
		return
	}
	h.writeProduct(ctx, w, r, p)
}
