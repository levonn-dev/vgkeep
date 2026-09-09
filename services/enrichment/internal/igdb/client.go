package igdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/time/rate"
)

const (
	defaultAPIURL   = "https://api.igdb.com/v4"
	defaultTokenURL = "https://id.twitch.tv/oauth2/token" //nolint:gosec // G101: public OAuth token endpoint URL, not a credential
	// Expanded references always include ids, so genres.name yields
	// {id, name} pairs without asking for genres.id.
	gameFields     = "name,cover.image_id,genres.name,themes.name,franchises.name,similar_games,involved_companies.company.name,involved_companies.developer,involved_companies.publisher,first_release_date,release_dates.date,release_dates.platform,release_dates.release_region,platforms.name,total_rating,total_rating_count,alternative_names.name,alternative_names.comment,game_localizations.name,game_localizations.region.identifier,game_localizations.cover.image_id"
	platformFields = "name,abbreviation,generation,platform_logo.image_id"
	// physicalTypesWhere drops the digital-only game types (DLC, Mod,
	// Episode, Season, Fork, Pack / Addon, Update) from discovery
	// queries; fetch-by-ids stays unfiltered so existing products keep refreshing.
	physicalTypesWhere = "game_type != (1,5,6,7,12,13,14)"
)

// Client is the real IGDB v4 client: a cached Twitch app token behind
// a client-side limiter matching the documented 4 req/s budget.
type Client struct {
	httpc    *http.Client
	limiter  *rate.Limiter
	clientID string
	secret   string
	apiURL   string
	tokenURL string

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewClient builds a Client for the given Twitch application.
func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		httpc: &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		limiter:  rate.NewLimiter(4, 4),
		clientID: clientID,
		secret:   clientSecret,
		apiURL:   defaultAPIURL,
		tokenURL: defaultTokenURL,
	}
}

// appToken returns the cached app token, refetching inside a 60s
// expiry margin (plain client-credentials; no signing keys here).
func (c *Client) appToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp.Add(-60*time.Second)) {
		return c.token, nil
	}
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.secret},
		"grant_type":    {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("igdb: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("igdb: token: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("igdb: token: status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("igdb: token: decode: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("igdb: token: empty access_token")
	}
	c.token = body.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return c.token, nil
}

// query POSTs one APICalypse body and decodes the array response.
func (c *Client) query(ctx context.Context, endpoint, body string, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("igdb: limiter: %w", err)
	}
	tok, err := c.appToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/"+endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("igdb: request: %w", err)
	}
	req.Header.Set("Client-ID", c.clientID)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("igdb: %s: %w", endpoint, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("igdb: %s: status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("igdb: %s: decode: %w", endpoint, err)
	}
	return nil
}

func intsCSV(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// SearchGames runs a full-text search.
func (c *Client) SearchGames(ctx context.Context, q string, limit int) ([]Game, error) {
	body := fmt.Sprintf("search %q; fields %s; where %s; limit %d;", q, gameFields, physicalTypesWhere, limit)
	var out []Game
	if err := c.query(ctx, "games", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchLocalizations finds games by localized title: IGDB's search
// index covers name + alternative_names but NOT game_localizations.
// Returns distinct ids; quotes/backslashes strip (would break the APICalypse string).
func (c *Client) SearchLocalizations(ctx context.Context, q string, limit int) ([]int64, error) {
	clean := strings.ReplaceAll(strings.ReplaceAll(q, `"`, ""), `\`, "")
	if strings.TrimSpace(clean) == "" {
		return nil, nil
	}
	body := fmt.Sprintf(`fields game; where name ~ *"%s"* & game.%s; limit %d;`, clean, physicalTypesWhere, limit)
	var out []struct {
		Game int64 `json:"game"`
	}
	if err := c.query(ctx, "game_localizations", body, &out); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(out))
	seen := make(map[int64]bool, len(out))
	for _, row := range out {
		if row.Game != 0 && !seen[row.Game] {
			seen[row.Game] = true
			ids = append(ids, row.Game)
		}
	}
	return ids, nil
}

// maxIDsPerQuery caps one where-id query; the API rejects any larger
// limit outright ("your maximum is 500").
const maxIDsPerQuery = 500

// GamesByIDs fetches full payloads, chunking past the per-query limit;
// unknown ids are silently absent.
func (c *Client) GamesByIDs(ctx context.Context, ids []int64) ([]Game, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]Game, 0, len(ids))
	for start := 0; start < len(ids); start += maxIDsPerQuery {
		chunk := ids[start:min(start+maxIDsPerQuery, len(ids))]
		body := fmt.Sprintf("fields %s; where id = (%s); limit %d;", gameFields, intsCSV(chunk), len(chunk))
		var got []Game
		if err := c.query(ctx, "games", body, &got); err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// PopularGames backs the genre-profile fallback: well-rated games in
// any genre; the where-clause exclusion caps at 100 ids, the rest filters here.
func (c *Client) PopularGames(ctx context.Context, genreIDs []int64, excludeIDs []int64, limit int) ([]Game, error) {
	if len(genreIDs) == 0 {
		return nil, nil
	}
	where := fmt.Sprintf("genres = (%s) & total_rating_count >= 20", intsCSV(genreIDs))
	if len(excludeIDs) > 0 {
		capped := excludeIDs
		if len(capped) > 100 {
			capped = capped[:100]
		}
		where += fmt.Sprintf(" & id != (%s)", intsCSV(capped))
	}
	where += " & " + physicalTypesWhere
	// limit grows with the exclude count (headroom for the client-side
	// filter), capped at maxIDsPerQuery, the API's hard ceiling.
	queryLimit := min(limit+len(excludeIDs), maxIDsPerQuery)
	body := fmt.Sprintf("fields %s; where %s; sort total_rating desc; limit %d;", gameFields, where, queryLimit)
	var out []Game
	if err := c.query(ctx, "games", body, &out); err != nil {
		return nil, err
	}
	excluded := make(map[int64]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excluded[id] = true
	}
	filtered := out[:0]
	for _, g := range out {
		if !excluded[g.ID] {
			filtered = append(filtered, g)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

// Platforms fetches the platform catalog wholesale (it is small and
// changes rarely; the store caches it for 30 days).
func (c *Client) Platforms(ctx context.Context) ([]Platform, error) {
	body := fmt.Sprintf("fields %s; sort id asc; limit 500;", platformFields)
	var out []Platform
	if err := c.query(ctx, "platforms", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}
