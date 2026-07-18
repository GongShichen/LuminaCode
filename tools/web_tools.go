package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"LuminaCode/config"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
)

const (
	defaultSearxNGBaseURL = "http://127.0.0.1:8888"
	searchCacheMaxItems   = 200
)

type WebSearchInput struct {
	Query      string `json:"query" jsonschema_description:"Search query to send to the configured SearxNG instance"`
	NumResults int    `json:"num_results,omitempty" jsonschema:"default=10" jsonschema_description:"Maximum number of results to return"`
	Language   string `json:"language,omitempty" jsonschema_description:"Optional SearxNG language code, for example en, zh-CN, or auto"`
	TimeRange  string `json:"time_range,omitempty" jsonschema_description:"Optional SearxNG time range such as day, week, month, or year"`
	Categories string `json:"categories,omitempty" jsonschema_description:"Optional SearxNG categories value such as general, science, or files"`
	Site       string `json:"site,omitempty" jsonschema_description:"Optional site/domain restriction. The tool prepends site:<site> to the query."`
}

type WebFetchInput struct {
	URL      string `json:"url" jsonschema_description:"URL to fetch after SearxNG source verification"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema_description:"Maximum characters of extracted text to return"`
}

type WebSearchTool struct{ BaseTool }

func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{BaseTool{Spec: ToolSpec{
		Name:            "WebSearch",
		Description:     "Search the web through the configured local SearxNG instance. Returns structured search results with titles, URLs, snippets, engines, categories, and ranks.",
		InputPrototype:  WebSearchInput{},
		Aliases:         []string{"web_search"},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		TimeoutSeconds:  30,
		MaxOutputChars:  80_000,
	}}}
}

func (t *WebSearchTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	cfg := configFromExecCtx(execCtx)
	if !cfg.WebSearchEnabled {
		return false, "web_search_enabled is false in the LuminaCode user settings"
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.WebSearchProvider), "searxng") {
		return false, fmt.Sprintf("unsupported web_search_provider %q; expected searxng", cfg.WebSearchProvider)
	}
	in := deref[WebSearchInput](input)
	if strings.TrimSpace(in.Query) == "" {
		return false, "query is required"
	}
	return true, ""
}

func (t *WebSearchTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	cfg := configFromExecCtx(execCtx)
	in := deref[WebSearchInput](input)
	result, err := runSearxNGSearch(ctx, cfg, in)
	if err != nil {
		return "", err
	}
	if err := appendSearchCache(execCtx, result.Results); err != nil {
		result.CacheWarning = err.Error()
	}
	return marshalIndented(result)
}

type WebFetchTool struct{ BaseTool }

func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{BaseTool{Spec: ToolSpec{
		Name:            "WebFetch",
		Description:     "Fetch a URL after verifying it through SearxNG search results, then extract readable page text. Use WebSearch first when possible.",
		InputPrototype:  WebFetchInput{},
		Aliases:         []string{"web_fetch"},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		TimeoutSeconds:  45,
		MaxOutputChars:  120_000,
	}}}
}

func (t *WebFetchTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	cfg := configFromExecCtx(execCtx)
	if !cfg.WebFetchEnabled {
		return false, "web_fetch_enabled is false in the LuminaCode user settings"
	}
	in := deref[WebFetchInput](input)
	if strings.TrimSpace(in.URL) == "" {
		return false, "url is required"
	}
	parsed, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false, "url must be an absolute http(s) URL"
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false, "url must use http or https"
	}
	return true, ""
}

func (t *WebFetchTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	cfg := configFromExecCtx(execCtx)
	in := deref[WebFetchInput](input)
	target := strings.TrimSpace(in.URL)
	verified := false
	verification := "disabled"
	if cfg.WebFetchRequireSearch {
		var err error
		verified, verification, err = verifyURLWithSearxNG(ctx, execCtx, cfg, target)
		if err != nil {
			return "", err
		}
		if !verified {
			return "", fmt.Errorf("SearxNG source verification failed for %s; run WebSearch first or verify the URL is discoverable", target)
		}
	}
	fetched, err := fetchReadableURL(ctx, cfg, target, maxPositive(in.MaxChars, cfg.WebFetchMaxChars))
	if err != nil {
		return "", err
	}
	fetched.SourceVerifiedBySearxNG = verified
	fetched.SourceVerification = verification
	return marshalIndented(fetched)
}

func (t *WebFetchTool) TimeoutForInput(input any) time.Duration {
	_ = deref[WebFetchInput](input)
	return 0
}

type searxNGResponse struct {
	Query               string          `json:"query"`
	NumberOfResults     int             `json:"number_of_results"`
	Results             []searxNGResult `json:"results"`
	Answers             []string        `json:"answers"`
	Corrections         []string        `json:"corrections"`
	Infoboxes           []any           `json:"infoboxes"`
	Suggestions         []string        `json:"suggestions"`
	UnresponsiveEngines [][]any         `json:"unresponsive_engines"`
}

type searxNGResult struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Engine        string  `json:"engine"`
	Category      string  `json:"category"`
	PublishedDate any     `json:"publishedDate"`
	Score         float64 `json:"score"`
}

type WebSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	Engine      string `json:"engine,omitempty"`
	Category    string `json:"category,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Rank        int    `json:"rank"`
}

type WebSearchOutput struct {
	Provider     string            `json:"provider"`
	BaseURL      string            `json:"base_url"`
	Query        string            `json:"query"`
	Results      []WebSearchResult `json:"results"`
	Answers      []string          `json:"answers,omitempty"`
	Suggestions  []string          `json:"suggestions,omitempty"`
	CacheWarning string            `json:"cache_warning,omitempty"`
}

type WebFetchOutput struct {
	URL                     string `json:"url"`
	Title                   string `json:"title,omitempty"`
	Text                    string `json:"text"`
	Excerpt                 string `json:"excerpt,omitempty"`
	Byline                  string `json:"byline,omitempty"`
	SiteName                string `json:"site_name,omitempty"`
	PublishedAt             string `json:"published_at,omitempty"`
	FetchedAt               string `json:"fetched_at"`
	ContentType             string `json:"content_type,omitempty"`
	SourceVerifiedBySearxNG bool   `json:"source_verified_by_searxng"`
	SourceVerification      string `json:"source_verification"`
	Truncated               bool   `json:"truncated"`
}

func runSearxNGSearch(ctx context.Context, cfg config.Config, input WebSearchInput) (WebSearchOutput, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.WebSearchBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultSearxNGBaseURL
	}
	requestURL, err := url.Parse(baseURL + "/search")
	if err != nil {
		return WebSearchOutput{}, fmt.Errorf("invalid web_search_base_url %q: %w", baseURL, err)
	}
	query := strings.TrimSpace(input.Query)
	if site := strings.TrimSpace(input.Site); site != "" {
		query = "site:" + site + " " + query
	}
	limit := input.NumResults
	if limit <= 0 {
		limit = cfg.WebSearchMaxResults
	}
	if limit <= 0 {
		limit = 10
	}
	params := requestURL.Query()
	params.Set("q", query)
	params.Set("format", "json")
	if input.Language != "" {
		params.Set("language", input.Language)
	}
	if input.TimeRange != "" {
		params.Set("time_range", input.TimeRange)
	}
	if input.Categories != "" {
		params.Set("categories", input.Categories)
	}
	requestURL.RawQuery = params.Encode()

	client := &http.Client{Timeout: secondsDuration(cfg.WebSearchTimeoutSeconds, 20*time.Second)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return WebSearchOutput{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", nonEmpty(cfg.WebFetchUserAgent, "LuminaCode/1.0"))
	resp, err := client.Do(req)
	if err != nil {
		return WebSearchOutput{}, fmt.Errorf("SearxNG search request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return WebSearchOutput{}, fmt.Errorf("SearxNG search failed: HTTP %d %s: %s", resp.StatusCode, resp.Status, truncateString(string(body), 1000))
	}
	var parsed searxNGResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return WebSearchOutput{}, fmt.Errorf("SearxNG returned invalid JSON from %s: %w; body: %s", requestURL.String(), err, truncateString(string(body), 1000))
	}
	out := WebSearchOutput{Provider: "searxng", BaseURL: baseURL, Query: query, Answers: parsed.Answers, Suggestions: parsed.Suggestions}
	for i, item := range parsed.Results {
		if i >= limit {
			break
		}
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		out.Results = append(out.Results, WebSearchResult{
			Title:       strings.TrimSpace(item.Title),
			URL:         strings.TrimSpace(item.URL),
			Snippet:     strings.TrimSpace(item.Content),
			Engine:      strings.TrimSpace(item.Engine),
			Category:    strings.TrimSpace(item.Category),
			PublishedAt: searxPublishedAt(item.PublishedDate),
			Rank:        len(out.Results) + 1,
		})
	}
	return out, nil
}

func verifyURLWithSearxNG(ctx context.Context, execCtx ExecutionContext, cfg config.Config, target string) (bool, string, error) {
	if found, source := urlInSearchCache(execCtx, target); found {
		return true, source, nil
	}
	parsed, _ := url.Parse(target)
	query := target
	if parsed != nil && parsed.Host != "" {
		query = "site:" + parsed.Host + " " + strings.Trim(parsed.Path, "/")
		if strings.TrimSpace(query) == "site:"+parsed.Host {
			query = target
		}
	}
	result, err := runSearxNGSearch(ctx, cfg, WebSearchInput{Query: query, NumResults: maxPositive(cfg.WebSearchMaxResults, 10)})
	if err != nil {
		return false, "", fmt.Errorf("SearxNG verification search failed for %s: %w", target, err)
	}
	_ = appendSearchCache(execCtx, result.Results)
	for _, item := range result.Results {
		if sameWebURL(item.URL, target) {
			return true, "SearxNG verification search: " + query, nil
		}
	}
	return false, "SearxNG verification search: " + query, nil
}

func fetchReadableURL(ctx context.Context, cfg config.Config, target string, maxChars int) (WebFetchOutput, error) {
	client := &http.Client{Timeout: secondsDuration(cfg.WebFetchTimeoutSeconds, 20*time.Second)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return WebFetchOutput{}, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("User-Agent", nonEmpty(cfg.WebFetchUserAgent, "LuminaCode/1.0"))
	resp, err := client.Do(req)
	if err != nil {
		return WebFetchOutput{}, fmt.Errorf("WebFetch HTTP request failed: %w", err)
	}
	defer resp.Body.Close()
	bodyLimit := maxChars * 4
	if bodyLimit < 1_000_000 {
		bodyLimit = 1_000_000
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(bodyLimit)))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return WebFetchOutput{}, fmt.Errorf("WebFetch failed: HTTP %d %s: %s", resp.StatusCode, resp.Status, truncateString(string(body), 1000))
	}
	contentType := resp.Header.Get("Content-Type")
	pageURL, _ := url.Parse(target)
	text := ""
	title := ""
	excerpt := ""
	byline := ""
	siteName := ""
	publishedAt := ""
	if strings.Contains(strings.ToLower(contentType), "html") || strings.Contains(strings.ToLower(contentType), "xml") || contentType == "" {
		article, err := readability.FromReader(bytes.NewReader(body), pageURL)
		if err == nil {
			title = strings.TrimSpace(article.Title)
			text = strings.TrimSpace(article.TextContent)
			excerpt = strings.TrimSpace(article.Excerpt)
			byline = strings.TrimSpace(article.Byline)
			siteName = strings.TrimSpace(article.SiteName)
			if article.PublishedTime != nil {
				publishedAt = article.PublishedTime.Format(time.RFC3339)
			}
		}
		if text == "" {
			doc, docErr := goquery.NewDocumentFromReader(bytes.NewReader(body))
			if docErr == nil {
				title = strings.TrimSpace(doc.Find("title").First().Text())
				text = strings.TrimSpace(doc.Find("body").Text())
			}
		}
	} else {
		text = strings.TrimSpace(string(body))
	}
	if text == "" {
		return WebFetchOutput{}, errors.New("WebFetch could not extract readable text from response body")
	}
	truncated := false
	if maxChars > 0 && len([]rune(text)) > maxChars {
		text = truncateString(text, maxChars)
		truncated = true
	}
	return WebFetchOutput{
		URL:         target,
		Title:       title,
		Text:        text,
		Excerpt:     excerpt,
		Byline:      byline,
		SiteName:    siteName,
		PublishedAt: publishedAt,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
		ContentType: contentType,
		Truncated:   truncated,
	}, nil
}

func appendSearchCache(execCtx ExecutionContext, results []WebSearchResult) error {
	path := webSearchCachePath(execCtx)
	if path == "" {
		return nil
	}
	cache := loadSearchCache(path)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, result := range results {
		if result.URL == "" {
			continue
		}
		cache.Items = append(cache.Items, cachedSearchResult{Result: result, CachedAt: now})
	}
	if len(cache.Items) > searchCacheMaxItems {
		cache.Items = cache.Items[len(cache.Items)-searchCacheMaxItems:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func urlInSearchCache(execCtx ExecutionContext, target string) (bool, string) {
	path := webSearchCachePath(execCtx)
	if path == "" {
		return false, ""
	}
	cache := loadSearchCache(path)
	for i := len(cache.Items) - 1; i >= 0; i-- {
		if sameWebURL(cache.Items[i].Result.URL, target) {
			return true, "recent SearxNG result cache"
		}
	}
	return false, ""
}

type searchCache struct {
	Items []cachedSearchResult `json:"items"`
}

type cachedSearchResult struct {
	Result   WebSearchResult `json:"result"`
	CachedAt string          `json:"cached_at"`
}

func loadSearchCache(path string) searchCache {
	data, err := os.ReadFile(path)
	if err != nil {
		return searchCache{}
	}
	var cache searchCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return searchCache{}
	}
	return cache
}

func webSearchCachePath(execCtx ExecutionContext) string {
	runtimeDir, _ := execCtx["runtime_dir"].(string)
	if runtimeDir == "" {
		return ""
	}
	sessionID, _ := execCtx["_session_id"].(string)
	if sessionID == "" {
		sessionID = "default"
	}
	agentID, _ := execCtx["_agent_id"].(string)
	if agentID == "" {
		agentID = "main"
	}
	scope, _ := execCtx["web_search_scope"].(string)
	if strings.TrimSpace(scope) == "" {
		scope = sessionID + "/" + agentID
	}
	key := sha256.Sum256([]byte(scope))
	return filepath.Join(runtimeDir, "web-search", hex.EncodeToString(key[:8])+".json")
}

func configFromExecCtx(execCtx ExecutionContext) config.Config {
	if cfg, ok := execCtx["config"].(config.Config); ok {
		return cfg
	}
	return config.NewConfig()
}

func sameWebURL(a, b string) bool {
	na := normalizeWebURL(a)
	nb := normalizeWebURL(b)
	return na != "" && na == nb
}

func normalizeWebURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	values := parsed.Query()
	for _, key := range []string{"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content", "fbclid", "gclid"} {
		values.Del(key)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := url.Values{}
	for _, key := range keys {
		for _, value := range values[key] {
			ordered.Add(key, value)
		}
	}
	parsed.RawQuery = ordered.Encode()
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func searxPublishedAt(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value > 0 {
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

func secondsDuration(seconds float64, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds * float64(time.Second))
}

func maxPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func marshalIndented(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
