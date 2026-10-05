package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

type app struct {
	db       *sql.DB
	s        *searcher
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
	School     school      `json:"school"`
	Company    company     `json:"company"`
	Candidates []candidate `json:"candidates"`
	People     []person    `json:"people"`
	Summary    summary     `json:"summary"`
}

func withURLs(c company, sc school) company {
	if c.Slug != "" {
		c.AlumniURL, c.CompanyURL = alumniURL(c.Slug, sc.ID), companyURL(c.Slug)
	}
	return c
}

func (a *app) getCompany(ctx context.Context, id int64, sc school) (company, error) {
	var c company
	err := a.db.QueryRowContext(ctx, `SELECT c.id, c.name, c.slug, c.slug_source,
		(SELECT count(*) FROM people p WHERE p.company_id = c.id) FROM companies c WHERE c.id=?`, id).
		Scan(&c.ID, &c.Name, &c.Slug, &c.SlugSource, &c.People)
	if errors.Is(err, sql.ErrNoRows) {
		return c, &apiError{404, "not_found", "company not found"}
	}
	return withURLs(c, sc), err
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
		out = append(out, withURLs(c, schools[0]))
	}
	return out, rows.Err()
}

func (a *app) listPeople(ctx context.Context, companyID int64, schoolKey string) ([]person, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, profile_url, name, headline, raw_title, snippet, score,
		confidence, score_reasons, status, notes FROM people WHERE company_id=? AND school=? ORDER BY score DESC, name`, companyID, schoolKey)
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
func (a *app) run(ctx context.Context, name string, sc school, refresh bool) (*searchResult, error) {
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
	c, err := a.getCompany(ctx, id, sc)
	if err != nil {
		return nil, err
	}
	res := &searchResult{School: sc, Summary: summary{XrayActive: a.s.enabled(), Elsewhere: []string{}}}
	fail := func(err error) (*searchResult, error) {
		var ae *apiError
		if !errors.As(err, &ae) {
			return nil, err
		}
		res.Summary.Error = &errJSON{ae.Code, ae.Message}
		return res, nil
	}

	// Slug lookup and all X-ray queries are independent network calls, so they run together.
	guess := guessSlug(name)
	var (
		slugRes    []result
		slugCached bool
		slugErr    error
		runs       []queryRun
		wg         sync.WaitGroup
	)
	if a.s.enabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slugRes, slugCached, slugErr = a.s.search(ctx, companyQuery(c.Name), 1, refresh)
		}()
		runs = a.xrayFetch(ctx, c.Name, sc, refresh)
		wg.Wait()
		res.Summary.Queries++
		res.Summary.Pages++
		if slugCached {
			res.Summary.CacheHits++
		}
	}

	// 1. Slug candidates.
	seen := map[string]bool{}
	for _, r := range slugRes {
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
	pick, source := guess, "guess"
	if containsSlug(res.Candidates, guess) {
		source = "xray" // verified guess wins
	} else if len(res.Candidates) > 0 {
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

	// 2. Store X-ray people (partial results too) in one transaction.
	xerr := slugErr
	if err := a.storePeople(ctx, id, c.Name, sc, runs, &res.Summary); err != nil {
		return nil, err
	}
	for _, r := range runs {
		if xerr == nil {
			xerr = r.err
		}
	}
	if res.Company, err = a.getCompany(ctx, id, sc); err != nil {
		return nil, err
	}
	if res.People, err = a.listPeople(ctx, id, sc.Key); err != nil {
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

// queryRun is what one X-ray query fetched across its pages.
type queryRun struct {
	hits             []result // profile results not already seen by another query
	pages, cacheHits int
	err              error
}

// xrayFetch runs every X-ray query in its own goroutine. Pages within a query stay sequential
// because "stop when a page adds no new profiles" depends on the previous page.
func (a *app) xrayFetch(ctx context.Context, name string, sc school, refresh bool) []queryRun {
	qs := xrayQueries(name, sc.Variants)
	runs := make([]queryRun, len(qs))
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i, q := range qs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &runs[i]
			for page := 1; page <= a.maxPages; page++ {
				results, cached, err := a.s.search(ctx, q, page, refresh)
				if err != nil {
					r.err = err
					return
				}
				r.pages++
				if cached {
					r.cacheHits++
				}
				fresh := 0
				mu.Lock()
				for _, res := range results {
					if slug, _, err := profileSlug(res.Link); err == nil && !seen[slug] {
						seen[slug] = true // non-profile results (posts, /pub/, etc.) are ignored
						fresh++
						r.hits = append(r.hits, res)
					}
				}
				mu.Unlock()
				if fresh == 0 {
					return // page added no new profiles
				}
			}
		}()
	}
	wg.Wait()
	return runs
}

func (a *app) storePeople(ctx context.Context, companyID int64, name string, sc school, runs []queryRun, sum *summary) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range runs {
		sum.Queries++
		sum.Pages += r.pages
		sum.CacheHits += r.cacheHits
		for _, res := range r.hits {
			slug, url, _ := profileSlug(res.Link)
			if err := a.upsertPerson(ctx, tx, companyID, name, sc, slug, url, res, sum); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (a *app) upsertPerson(ctx context.Context, tx *sql.Tx, companyID int64, company string, sch school, slug, url string, r result, sum *summary) error {
	pname, headline := parseTitle(r.Title)
	sc := score(company, sch.Variants, r.Title, r.Snippet, headline)
	reasons, _ := json.Marshal(sc.Reasons)

	var existingCo int64
	var coName, existingSchool string
	err := tx.QueryRowContext(ctx, `SELECT p.company_id, c.name, p.school FROM people p JOIN companies c ON c.id=p.company_id
		WHERE p.profile_slug=?`, slug).Scan(&existingCo, &coName, &existingSchool)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sum.New++
		_, err = tx.ExecContext(ctx, `INSERT INTO people(company_id, school, profile_url, profile_slug, name, headline, raw_title,
			snippet, score, confidence, score_reasons) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			companyID, sch.Key, url, slug, pname, headline, r.Title, r.Snippet, sc.Score, sc.Confidence, string(reasons))
		return err
	case err != nil:
		return err
	case existingCo != companyID || existingSchool != sch.Key:
		// One row per person: keep the first company/school that found them, untouched.
		sum.Elsewhere = append(sum.Elsewhere, pname+" (already under "+coName+" · "+existingSchool+")")
		return nil
	default: // refresh search data, keep tracker fields
		sum.Updated++
		_, err = tx.ExecContext(ctx, `UPDATE people SET name=?, headline=?, raw_title=?, snippet=?, score=?, confidence=?,
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
