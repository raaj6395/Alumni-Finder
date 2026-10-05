package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type app struct {
	db       *sql.DB
	s        *searcher
	schoolID string
	variants []string
	maxPages int
}

type company struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	SlugSource string `json:"slug_source"`
	AlumniURL  string `json:"alumni_url"`
	CompanyURL string `json:"company_url"`
	People     int    `json:"people"`
}

type person struct {
	ID         int64           `json:"id"`
	ProfileURL string          `json:"profile_url"`
	Name       string          `json:"name"`
	Headline   string          `json:"headline"`
	RawTitle   string          `json:"raw_title"`
	Snippet    string          `json:"snippet"`
	Score      int             `json:"score"`
	Confidence string          `json:"confidence"`
	Reasons    json.RawMessage `json:"reasons"`
	Status     string          `json:"status"`
	Notes      string          `json:"notes"`
}

type candidate struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"` // xray | guess
}

type summary struct {
	Queries    int      `json:"queries"`
	Pages      int      `json:"pages"`
	CacheHits  int      `json:"cache_hits"`
	New        int      `json:"new"`
	Updated    int      `json:"updated"`
	Elsewhere  []string `json:"elsewhere"` // "Name (already under Company)"
	Error      *errJSON `json:"error,omitempty"`
	XrayActive bool     `json:"xray_active"`
}

type errJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type searchResult struct {
	Company    company     `json:"company"`
	Candidates []candidate `json:"candidates"`
	People     []person    `json:"people"`
	Summary    summary     `json:"summary"`
}

func (a *app) withURLs(c company) company {
	if c.Slug != "" {
		c.AlumniURL, c.CompanyURL = alumniURL(c.Slug, a.schoolID), companyURL(c.Slug)
	}
	return c
}

func (a *app) getCompany(ctx context.Context, id int64) (company, error) {
	var c company
	err := a.db.QueryRowContext(ctx, `SELECT c.id, c.name, c.slug, c.slug_source,
		(SELECT count(*) FROM people p WHERE p.company_id = c.id) FROM companies c WHERE c.id=?`, id).
		Scan(&c.ID, &c.Name, &c.Slug, &c.SlugSource, &c.People)
	if errors.Is(err, sql.ErrNoRows) {
		return c, &apiError{404, "not_found", "company not found"}
	}
	return a.withURLs(c), err
}

func (a *app) listCompanies(ctx context.Context) ([]company, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT c.id, c.name, c.slug, c.slug_source,
		(SELECT count(*) FROM people p WHERE p.company_id = c.id) FROM companies c ORDER BY c.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []company{}
	for rows.Next() {
		var c company
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.SlugSource, &c.People); err != nil {
			return nil, err
		}
		out = append(out, a.withURLs(c))
	}
	return out, rows.Err()
}

func (a *app) listPeople(ctx context.Context, companyID int64) ([]person, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, profile_url, name, headline, raw_title, snippet, score,
		confidence, score_reasons, status, notes FROM people WHERE company_id=? ORDER BY score DESC, name`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []person{}
	for rows.Next() {
		var p person
		var reasons string
		if err := rows.Scan(&p.ID, &p.ProfileURL, &p.Name, &p.Headline, &p.RawTitle, &p.Snippet, &p.Score,
			&p.Confidence, &reasons, &p.Status, &p.Notes); err != nil {
			return nil, err
		}
		p.Reasons = json.RawMessage(reasons)
		out = append(out, p)
	}
	return out, rows.Err()
}

// setSlug accepts a bare slug or any linkedin.com/company URL.
func (a *app) setSlug(ctx context.Context, id int64, input, source string) error {
	slug := strings.TrimSpace(input)
	if strings.Contains(slug, "linkedin.com") {
		s, err := companySlug(slug)
		if err != nil {
			return &apiError{400, "bad_url", err.Error()}
		}
		slug = s
	} else {
		slug = strings.ToLower(strings.Trim(slug, "/ "))
	}
	if slug == "" || strings.ContainsAny(slug, "/?# ") {
		return &apiError{400, "bad_slug", "slug must be non-empty with no spaces or slashes"}
	}
	res, err := a.db.ExecContext(ctx, `UPDATE companies SET slug=?, slug_source=?, updated_at=datetime('now') WHERE id=?`, slug, source, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &apiError{404, "not_found", "company not found"}
	}
	return nil
}

// run is the whole "type a company, get alumni" flow.
func (a *app) run(ctx context.Context, name string, refresh bool) (*searchResult, error) {
	name = strings.TrimSpace(name)
	norm := normalizeName(name)
	if norm == "" {
		return nil, &apiError{400, "bad_name", "company name is required"}
	}
	var id int64
	err := a.db.QueryRowContext(ctx, `INSERT INTO companies(name, normalized_name) VALUES(?, ?)
		ON CONFLICT(normalized_name) DO UPDATE SET updated_at=datetime('now') RETURNING id`, name, norm).Scan(&id)
	if err != nil {
		return nil, err
	}
	c, err := a.getCompany(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &searchResult{Summary: summary{XrayActive: a.s.enabled(), Elsewhere: []string{}}}
	fail := func(err error) (*searchResult, error) {
		var ae *apiError
		if !errors.As(err, &ae) {
			return nil, err
		}
		res.Summary.Error = &errJSON{ae.Code, ae.Message}
		return res, nil
	}

	// 1. Slug candidates. Search results are cached, so this is free on repeat visits.
	guess := guessSlug(name)
	var xerr error
	if a.s.enabled() {
		results, cached, err := a.s.search(ctx, companyQuery(c.Name), 1, refresh)
		res.Summary.Queries++
		res.Summary.Pages++
		if cached {
			res.Summary.CacheHits++
		}
		if err != nil {
			xerr = err // still save the guessed slug below so the alumni link works
		}
		seen := map[string]bool{}
		for _, r := range results {
			slug, err := companySlug(r.Link)
			if err != nil || seen[slug] {
				continue
			}
			seen[slug] = true
			res.Candidates = append(res.Candidates, candidate{slug, r.Title, r.Snippet, "xray"})
			if len(res.Candidates) == 5 {
				break
			}
		}
	}
	pick := guess
	source := "guess"
	for _, cd := range res.Candidates {
		if cd.Slug == guess {
			pick, source = guess, "xray" // verified guess wins
			break
		}
	}
	if source == "guess" && len(res.Candidates) > 0 {
		pick, source = res.Candidates[0].Slug, "xray"
	}
	if !containsSlug(res.Candidates, guess) {
		res.Candidates = append(res.Candidates, candidate{Slug: guess, Source: "guess"})
	}
	if c.Slug == "" || (refresh && c.SlugSource != "manual") {
		if err := a.setSlug(ctx, id, pick, source); err != nil {
			return nil, err
		}
	}

	// 2. X-ray people search.
	if xerr == nil && a.s.enabled() {
		xerr = a.xray(ctx, id, c.Name, refresh, &res.Summary)
	}
	if res.Company, err = a.getCompany(ctx, id); err != nil {
		return nil, err
	}
	if res.People, err = a.listPeople(ctx, id); err != nil {
		return nil, err
	}
	if xerr != nil {
		return fail(xerr) // partial results already stored and returned
	}
	return res, nil
}

func containsSlug(cs []candidate, slug string) bool {
	for _, c := range cs {
		if c.Slug == slug {
			return true
		}
	}
	return false
}

func (a *app) xray(ctx context.Context, companyID int64, name string, refresh bool, sum *summary) error {
	seen := map[string]bool{}
	for _, q := range xrayQueries(name, a.variants) {
		sum.Queries++
		for page := 1; page <= a.maxPages; page++ {
			results, cached, err := a.s.search(ctx, q, page, refresh)
			if err != nil {
				return err
			}
			sum.Pages++
			if cached {
				sum.CacheHits++
			}
			fresh := 0
			for _, r := range results {
				slug, url, err := profileSlug(r.Link)
				if err != nil || seen[slug] {
					continue // non-profile results (posts, /pub/, etc.) are ignored
				}
				seen[slug] = true
				fresh++
				if err := a.upsertPerson(ctx, companyID, name, slug, url, r, sum); err != nil {
					return err
				}
			}
			if fresh == 0 {
				break // page added no new profiles
			}
		}
	}
	return nil
}

func (a *app) upsertPerson(ctx context.Context, companyID int64, company, slug, url string, r result, sum *summary) error {
	pname, headline := parseTitle(r.Title)
	sc := score(company, a.variants, r.Title, r.Snippet, headline)
	reasons, _ := json.Marshal(sc.Reasons)

	var existingCo int64
	var coName string
	err := a.db.QueryRowContext(ctx, `SELECT p.company_id, c.name FROM people p JOIN companies c ON c.id=p.company_id
		WHERE p.profile_slug=?`, slug).Scan(&existingCo, &coName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sum.New++
		_, err = a.db.ExecContext(ctx, `INSERT INTO people(company_id, profile_url, profile_slug, name, headline, raw_title,
			snippet, score, confidence, score_reasons) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			companyID, url, slug, pname, headline, r.Title, r.Snippet, sc.Score, sc.Confidence, string(reasons))
		return err
	case err != nil:
		return err
	case existingCo != companyID:
		sum.Elsewhere = append(sum.Elsewhere, pname+" (already under "+coName+")")
		return nil
	default: // refresh search data, keep tracker fields
		sum.Updated++
		_, err = a.db.ExecContext(ctx, `UPDATE people SET name=?, headline=?, raw_title=?, snippet=?, score=?, confidence=?,
			score_reasons=?, updated_at=datetime('now') WHERE profile_slug=?`,
			pname, headline, r.Title, r.Snippet, sc.Score, sc.Confidence, string(reasons), slug)
		return err
	}
}

func (a *app) updatePerson(ctx context.Context, id int64, status, notes *string) error {
	res, err := a.db.ExecContext(ctx, `UPDATE people SET status=coalesce(?, status), notes=coalesce(?, notes),
		updated_at=datetime('now') WHERE id=?`, status, notes, id)
	if err != nil {
		if strings.Contains(err.Error(), "CHECK") {
			return &apiError{400, "bad_status", "invalid status"}
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &apiError{404, "not_found", "person not found"}
	}
	return nil
}

func (a *app) usage(ctx context.Context) (map[string]int, error) {
	var today, total int
	err := a.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN date=date('now','localtime') THEN count END),0),
		coalesce(sum(count),0) FROM api_usage`).Scan(&today, &total)
	return map[string]int{"today": today, "total": total}, err
}
