package main

// Serper.dev client with SQLite response cache and per-day usage counting.
//
// Assumptions about Serper (see testdata/ fixtures):
//   - POST https://google.serper.dev/search, header X-API-KEY, body {"q","page","num"}.
//   - 200 body: {"organic":[{"title","link","snippet","position"}], ...}; missing "organic" = no results.
//   - Errors: non-2xx with {"message": "...", "statusCode": n}. Out of credits has been seen as
//     400 "Not enough credits", so quota is detected by message text as well as status 402/429.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type result struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
}

// apiError is surfaced to the UI verbatim as {error:{code,message}}.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

type searcher struct {
	db       *sql.DB
	key      string
	endpoint string
	ttl      time.Duration
	client   *http.Client

	sem chan struct{} // caps in-flight Serper calls
}

func newSearcher(db *sql.DB, key string, ttl time.Duration) *searcher {
	return &searcher{db: db, key: key, endpoint: "https://google.serper.dev/search", ttl: ttl,
		sem: make(chan struct{}, 4),
		// Reuse TLS connections across parallel calls (default keeps only 2 idle per host).
		client: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true}}}
}

func (s *searcher) enabled() bool { return s.key != "" }

// search returns organic results for (q, page), from cache unless refresh or expired.
func (s *searcher) search(ctx context.Context, q string, page int, refresh bool) (res []result, cached bool, err error) {
	if !s.enabled() {
		return nil, false, &apiError{503, "xray_disabled", "SERPER_API_KEY is not set; X-ray search is disabled"}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("serper|%s|%d", q, page)))
	key := hex.EncodeToString(sum[:])

	if !refresh {
		var body, fetched string
		err := s.db.QueryRowContext(ctx, `SELECT response_json, fetched_at FROM search_cache WHERE key=?`, key).Scan(&body, &fetched)
		if err == nil {
			if t, _ := time.Parse(time.RFC3339, fetched); time.Since(t) < s.ttl {
				res, err := parseOrganic([]byte(body))
				return res, true, err
			}
		} else if err != sql.ErrNoRows {
			return nil, false, err
		}
	}

	body, err := s.fetch(ctx, q, page)
	if err != nil {
		return nil, false, err
	}
	if res, err = parseOrganic(body); err != nil {
		return nil, false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO search_cache(key, provider, query, page, response_json, fetched_at)
		VALUES(?, 'serper', ?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET response_json=excluded.response_json, fetched_at=excluded.fetched_at`,
		key, q, page, string(body), time.Now().UTC().Format(time.RFC3339))
	return res, false, err
}

func (s *searcher) fetch(ctx context.Context, q string, page int) ([]byte, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	reqBody, _ := json.Marshal(map[string]any{"q": q, "page": page, "num": 10})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", s.key)
	req.Header.Set("Content-Type", "application/json")

	// Count every real attempt: Serper may bill failures too, and undercounting is worse.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_usage(date, provider, count) VALUES(?, 'serper', 1)
		ON CONFLICT(date, provider) DO UPDATE SET count = count + 1`, time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Error("serper request failed", "query", q, "page", page, "err", err)
		return nil, &apiError{502, "provider_error", "Serper request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, &apiError{502, "provider_error", "reading Serper response: " + err.Error()}
	}
	slog.Info("serper call", "query", q, "page", page, "status", resp.StatusCode)
	if resp.StatusCode/100 != 2 {
		return nil, classify(resp.StatusCode, body)
	}
	return body, nil
}

func classify(status int, body []byte) *apiError {
	var e struct{ Message string }
	_ = json.Unmarshal(body, &e)
	msg := e.Message
	if msg == "" {
		msg = string(body[:min(len(body), 300)])
	}
	msg = fmt.Sprintf("Serper HTTP %d: %s", status, msg)
	low := strings.ToLower(msg)
	switch {
	case status == 429 || status == 402 || strings.Contains(low, "credit") || strings.Contains(low, "quota"):
		return &apiError{429, "quota_exhausted", msg}
	case status == 401 || status == 403:
		return &apiError{502, "auth_error", msg + " (check SERPER_API_KEY)"}
	default:
		return &apiError{502, "provider_error", msg}
	}
}

func parseOrganic(body []byte) ([]result, error) {
	var r struct {
		Organic []result `json:"organic"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &apiError{502, "parse_error", "could not parse Serper response: " + err.Error()}
	}
	return r.Organic, nil
}
