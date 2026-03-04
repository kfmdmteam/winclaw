package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func executeWebSearch(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("web_search: bad input: %w", err)
	}
	if params.Query == "" {
		return "", fmt.Errorf("web_search: query is required")
	}

	// DuckDuckGo Instant Answer API — free, no key required.
	apiURL := "https://api.duckduckgo.com/?q=" + url.QueryEscape(params.Query) +
		"&format=json&no_html=1&skip_disambig=1"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("web_search: build request: %w", err)
	}
	req.Header.Set("User-Agent", "WinClaw/0.1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Sprintf("search request failed: %v", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return fmt.Sprintf("read response failed: %v", err), nil
	}

	var result struct {
		AbstractText  string `json:"AbstractText"`
		AbstractURL   string `json:"AbstractURL"`
		Answer        string `json:"Answer"`
		Definition    string `json:"Definition"`
		RelatedTopics []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Sprintf("parse response failed: %v", err), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %q\n\n", params.Query))

	if result.Answer != "" {
		sb.WriteString("Answer: " + result.Answer + "\n\n")
	}
	if result.AbstractText != "" {
		sb.WriteString(result.AbstractText + "\n")
		if result.AbstractURL != "" {
			sb.WriteString("Source: " + result.AbstractURL + "\n")
		}
		sb.WriteString("\n")
	}
	if result.Definition != "" {
		sb.WriteString("Definition: " + result.Definition + "\n\n")
	}

	count := 0
	for _, t := range result.RelatedTopics {
		if t.Text == "" {
			continue
		}
		sb.WriteString("- " + t.Text + "\n")
		if t.FirstURL != "" {
			sb.WriteString("  " + t.FirstURL + "\n")
		}
		count++
		if count >= 5 {
			break
		}
	}

	if sb.Len() < 80 {
		// DuckDuckGo returned little — fall back to HTML scrape summary.
		return scrapeSearch(ctx, params.Query)
	}

	return sb.String(), nil
}

func executeFetchURL(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("fetch_url: bad input: %w", err)
	}
	if params.URL == "" {
		return "", fmt.Errorf("fetch_url: url is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, params.URL, nil)
	if err != nil {
		return fmt.Sprintf("error building request: %v", err), nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) WinClaw/0.1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Sprintf("fetch failed: %v", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return fmt.Sprintf("read failed: %v", err), nil
	}

	text := stripHTML(string(body))
	text = strings.TrimSpace(text)

	const maxLen = 12000
	if len(text) > maxLen {
		text = text[:maxLen] + fmt.Sprintf("\n... [truncated, %d total chars]", len(text))
	}

	return text, nil
}

// scrapeSearch falls back to scraping DuckDuckGo HTML for sparse queries.
func scrapeSearch(ctx context.Context, query string) (string, error) {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Sprintf("search unavailable: %v", err), nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) WinClaw/0.1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	text := stripHTML(string(body))

	// Extract the first meaningful chunk.
	lines := strings.Split(text, "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) > 40 {
			kept = append(kept, l)
		}
		if len(kept) >= 20 {
			break
		}
	}
	return fmt.Sprintf("Search results for %q:\n\n", query) + strings.Join(kept, "\n"), nil
}

var (
	reTag    = regexp.MustCompile(`<[^>]+>`)
	reSpaces = regexp.MustCompile(`[ \t]{2,}`)
	reLines  = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	// Decode common HTML entities.
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = reSpaces.ReplaceAllString(s, " ")
	s = reLines.ReplaceAllString(s, "\n\n")
	return s
}
