package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSerper serves fixture files. route maps a query substring to a fixture; page>1 gets empty.json.
func fakeSerper(t *testing.T, route func(q string, page int) (status int, fixture string)) (*httptest.Server, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-API-KEY") != "test-key" {
			t.Errorf("missing api key header")
		}
		var in struct {
			Q    string
			Page int
		}
		json.NewDecoder(r.Body).Decode(&in)
		status, fx := route(in.Q, in.Page)
		b, err := os.ReadFile(filepath.Join("testdata", fx))
		if err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(status)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestApp(t *testing.T, key, endpoint string) http.Handler {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := newSearcher(db, key, 24*time.Hour)
	s.endpoint = endpoint
	return (&app{db: db, s: s, maxPages: 3}).routes()
}

func do(t *testing.T, h http.Handler, method, path, body string, out any) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	b, _ := io.ReadAll(rec.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: bad json %q", method, path, b)
		}
	}
	return rec.Code
}

func happyRoute(q string, page int) (int, string) {
	switch {
	case strings.HasPrefix(q, "site:linkedin.com/company"):
		return 200, "company_razorpay.json"
	case page == 1 && strings.Contains(q, " OR "):
		return 200, "people_page1.json"
	case page == 1:
		return 200, "no_organic.json"
	default:
		return 200, "empty.json"
	}
}

func TestSearchFlow(t *testing.T) {
	srv, calls := fakeSerper(t, happyRoute)
	h := newTestApp(t, "test-key", srv.URL)

	var res searchResult
	if code := do(t, h, "POST", "/api/search", `{"name":"Razorpay"}`, &res); code != 200 {
		t.Fatalf("status %d", code)
	}
	if res.Summary.Error != nil {
		t.Fatalf("unexpected error %+v", res.Summary.Error)
	}
	c := res.Company
	if c.Slug != "razorpay" || c.SlugSource != "xray" || c.AlumniURL != "https://www.linkedin.com/company/razorpay/people/?facetSchool=15105506" {
		t.Errorf("company = %+v", c)
	}
	// razorpay (deduped from 2 URLs), razorpayx; showcase + post skipped; guess already present.
	if len(res.Candidates) != 2 || res.Candidates[1].Slug != "razorpayx" {
		t.Errorf("candidates = %+v", res.Candidates)
	}
	if len(res.People) != 3 {
		t.Fatalf("people = %+v", res.People)
	}
	if p := res.People[0]; p.Name != "Asha Verma" || p.Confidence != "high" || p.ProfileURL != "https://www.linkedin.com/in/asha-verma-123" {
		t.Errorf("top person = %+v", p)
	}
	if last := res.People[2]; last.Name != "Rohit K" || last.Confidence != "low" {
		t.Errorf("ex-employee should rank last: %+v", last)
	}
	// 1 company query + OR query p1,p2(empty→stop) + 2 single queries p1 (no organic→stop)
	if *calls != 5 || res.Summary.Pages != 5 || res.Summary.New != 3 {
		t.Errorf("calls=%d summary=%+v", *calls, res.Summary)
	}

	// Tracker edits survive a re-run; second run is fully cached.
	id := res.People[0].ID
	if code := do(t, h, "PATCH", "/api/people/"+itoa(id), `{"status":"request_sent","notes":"batchmate of X"}`, nil); code != 200 {
		t.Fatalf("patch person %d", code)
	}
	if code := do(t, h, "PATCH", "/api/people/"+itoa(id), `{"status":"bogus"}`, nil); code != 400 {
		t.Errorf("bad status accepted: %d", code)
	}
	do(t, h, "POST", "/api/search", `{"name":"  razorpay "}`, &res)
	if *calls != 5 || res.Summary.CacheHits != 5 || res.Summary.Updated != 3 || res.Company.ID != c.ID {
		t.Errorf("second run calls=%d summary=%+v", *calls, res.Summary)
	}
	if res.People[0].Status != "request_sent" || res.People[0].Notes != "batchmate of X" {
		t.Errorf("tracker fields lost: %+v", res.People[0])
	}

	// Manual slug override via URL.
	var co company
	if code := do(t, h, "PATCH", "/api/companies/"+itoa(c.ID), `{"slug":"https://in.linkedin.com/company/razorpayx/life"}`, &co); code != 200 || co.Slug != "razorpayx" || co.SlugSource != "manual" {
		t.Errorf("manual slug: %d %+v", code, co)
	}
	var usage map[string]int
	do(t, h, "GET", "/api/usage", "", &usage)
	if usage["today"] != 5 || usage["total"] != 5 {
		t.Errorf("usage = %v", usage)
	}
}

func TestQuotaKeepsPartialResults(t *testing.T) {
	srv, _ := fakeSerper(t, func(q string, page int) (int, string) {
		if strings.Contains(q, "MNNIT Allahabad") {
			return 400, "no_credits.json"
		}
		return happyRoute(q, page)
	})
	h := newTestApp(t, "test-key", srv.URL)
	var res searchResult
	do(t, h, "POST", "/api/search", `{"name":"Razorpay"}`, &res)
	if res.Summary.Error == nil || res.Summary.Error.Code != "quota_exhausted" || !strings.Contains(res.Summary.Error.Message, "Not enough credits") {
		t.Fatalf("error = %+v", res.Summary.Error)
	}
	if len(res.People) != 3 {
		t.Errorf("partial results lost: %d", len(res.People))
	}
}

func TestErrorsAreSurfaced(t *testing.T) {
	srv, _ := fakeSerper(t, func(string, int) (int, string) { return 403, "unauthorized.json" })
	h := newTestApp(t, "test-key", srv.URL)
	var res searchResult
	do(t, h, "POST", "/api/search", `{"name":"Razorpay"}`, &res)
	if res.Summary.Error == nil || res.Summary.Error.Code != "auth_error" {
		t.Fatalf("error = %+v", res.Summary.Error)
	}
	if res.Company.Slug != "razorpay" || res.Company.SlugSource != "guess" {
		t.Errorf("guessed slug should still be saved: %+v", res.Company)
	}

	var e map[string]errJSON
	if code := do(t, h, "POST", "/api/search", `{"name":"  "}`, &e); code != 400 || e["error"].Code != "bad_name" {
		t.Errorf("empty name: %d %+v", code, e)
	}
	if code := do(t, h, "GET", "/api/companies/999", "", &e); code != 404 || e["error"].Code != "not_found" {
		t.Errorf("missing company: %d %+v", code, e)
	}
}

func TestNoKeyStillGivesAlumniLink(t *testing.T) {
	h := newTestApp(t, "", "http://127.0.0.1:1") // would fail if ever called
	var res searchResult
	do(t, h, "POST", "/api/search", `{"name":"Goldman Sachs"}`, &res)
	if res.Summary.XrayActive || res.Summary.Error != nil || res.Company.AlumniURL != "https://www.linkedin.com/company/goldman-sachs/people/?facetSchool=15105506" {
		t.Errorf("res = %+v", res)
	}
}

func TestParseOrganicTolerant(t *testing.T) {
	for _, fx := range []string{"empty.json", "no_organic.json"} {
		b, _ := os.ReadFile(filepath.Join("testdata", fx))
		if r, err := parseOrganic(b); err != nil || len(r) != 0 {
			t.Errorf("%s: %v %v", fx, r, err)
		}
	}
	if _, err := parseOrganic([]byte("<html>")); err == nil {
		t.Error("garbage should be a parse error")
	}
}

func TestSchoolsAreSeparate(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv, _ := fakeSerper(t, func(q string, page int) (int, string) {
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		return happyRoute(q, page)
	})
	h := newTestApp(t, "test-key", srv.URL)

	var mnnit, iitr searchResult
	do(t, h, "POST", "/api/search", `{"name":"Razorpay","school":"mnnit"}`, &mnnit)
	queries = nil
	do(t, h, "POST", "/api/search", `{"name":"Razorpay","school":"iitr"}`, &iitr)

	if iitr.School.Key != "iitr" || iitr.Company.AlumniURL != "https://www.linkedin.com/company/razorpay/people/?facetSchool=157269" {
		t.Errorf("iitr company = %+v", iitr.Company)
	}
	for _, q := range queries {
		if strings.Contains(q, "MNNIT") {
			t.Errorf("IIT Roorkee search sent an MNNIT query: %s", q)
		}
	}
	if !slices.Contains(queries, `site:linkedin.com/in ("IIT Roorkee" OR "Indian Institute of Technology Roorkee") "Razorpay"`) {
		t.Errorf("queries = %q", queries)
	}
	// Fixture profiles were stored under MNNIT first; the IITR list stays scoped to IITR.
	if len(mnnit.People) != 3 || len(iitr.People) != 0 || len(iitr.Summary.Elsewhere) != 3 || iitr.Summary.Updated != 0 {
		t.Errorf("mnnit=%d iitr=%d people, summary=%+v", len(mnnit.People), len(iitr.People), iitr.Summary)
	}
	// MNNIT scores must not be recomputed with IIT Roorkee's names.
	var again searchResult
	do(t, h, "GET", "/api/companies/"+itoa(mnnit.Company.ID)+"?school=mnnit", "", &again)
	if again.People[0].Score != mnnit.People[0].Score {
		t.Errorf("score changed %d -> %d", mnnit.People[0].Score, again.People[0].Score)
	}
	var e map[string]errJSON
	if code := do(t, h, "POST", "/api/search", `{"name":"Razorpay","school":"xyz"}`, &e); code != 400 || e["error"].Code != "bad_school" {
		t.Errorf("unknown school: %d %+v", code, e)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Serper calls are network-bound; queries must run in parallel, not one after another.
func TestSearchRunsQueriesConcurrently(t *testing.T) {
	const latency = 300 * time.Millisecond
	srv, calls := fakeSerper(t, func(q string, page int) (int, string) {
		time.Sleep(latency)
		return happyRoute(q, page)
	})
	h := newTestApp(t, "test-key", srv.URL)
	start := time.Now()
	var res searchResult
	do(t, h, "POST", "/api/search", `{"name":"Razorpay"}`, &res)
	took := time.Since(start)
	t.Logf("%d calls in %v", *calls, took)
	// Longest chain is the OR query's 2 pages; everything else overlaps it.
	if took > 3*latency || len(res.People) != 3 {
		t.Errorf("took %v for %d calls (%d people); want < %v", took, *calls, len(res.People), 3*latency)
	}
}
