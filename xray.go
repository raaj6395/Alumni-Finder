package main

// X-ray query building, result-title parsing and deterministic relevance scoring.

import (
	"regexp"
	"strings"
)

// roleKeywords: a headline containing any of these earns the engineering-role bonus.
var roleKeywords = []string{"sde", "swe", "software engineer", "software developer", "engineer", "developer",
	"backend", "frontend", "full stack", "fullstack", "devops", "sre"}

// pastMarkers just before a company mention mean they probably no longer work there.
var pastMarkers = regexp.MustCompile(`(?i)(\bex\s*[-–]?\s*$|\bex[-–]|\bformer(ly)?\b|\bpreviously\b|\bworked\s+(at|with|for)\b)`)

// companyQuery: site:linkedin.com/company "{name}" finds the slug.
func companyQuery(name string) string { return `site:linkedin.com/company "` + name + `"` }

// xrayQueries: one OR query over the first two school variants, then one query per remaining
// variant. With the default variants this yields exactly:
//
//	site:linkedin.com/in ("MNNIT" OR "Motilal Nehru National Institute of Technology") "X"
//	site:linkedin.com/in "MNNIT Allahabad" "X"
//	site:linkedin.com/in "NIT Allahabad" "X"
func xrayQueries(company string, variants []string) []string {
	if len(variants) == 0 {
		return nil
	}
	head := variants[:min(2, len(variants))]
	qs := []string{`site:linkedin.com/in ("` + strings.Join(head, `" OR "`) + `") "` + company + `"`}
	for _, v := range variants[len(head):] {
		qs = append(qs, `site:linkedin.com/in "`+v+`" "`+company+`"`)
	}
	return qs
}

var (
	titleEllipsis = regexp.MustCompile(`\s*(\.\.\.|…)\s*$`)
	// " | LinkedIn", "- LinkedIn", and Google-truncated " | Linked", " | Li"
	titleSuffix = regexp.MustCompile(`(?i)\s*([|\-–—·]\s*l(i(n(k(e(d(in?)?)?)?)?)?)?|\blinkedin)\s*$`)
	titleSep    = regexp.MustCompile(`\s+(-|–|—|\|)\s+`)
)

// parseTitle splits "Name - Headline - Company | LinkedIn" into name and headline (best effort).
func parseTitle(title string) (name, headline string) {
	t := titleEllipsis.ReplaceAllString(strings.TrimSpace(title), "")
	t = titleSuffix.ReplaceAllString(t, "")
	parts := titleSep.Split(t, -1)
	name = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		headline = strings.TrimSpace(strings.Join(parts[1:], " - "))
	}
	return name, headline
}

// wordRe matches term as a whole word/phrase, case-insensitive.
func wordRe(term string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])` + regexp.QuoteMeta(term) + `($|[^\p{L}\p{N}])`)
}

// pastMention reports whether a past-employment marker sits within 30 chars before a company mention.
func pastMention(text string, company *regexp.Regexp) bool {
	for _, loc := range company.FindAllStringIndex(text, -1) {
		before := text[max(0, loc[0]-30) : loc[0]+1] // +1 keeps the boundary char (e.g. "-" in "Ex-Google")
		if pastMarkers.MatchString(before) {
			return true
		}
	}
	return false
}

var (
	ugDegree = regexp.MustCompile(`(?i:\bb[.\-]?\s?tech\b|\bbachelor|\bdual\s+degree\b|\bundergrad)|\bB\.\s?E\b`)
	// Branch / year words: usually B.Tech, but only trusted when no postgrad word is nearby.
	ugHint   = regexp.MustCompile(`(?i)\b(cse|ece|eee|computer\s+science|electronics|electrical|mechanical|civil|chemical|biotech(nology)?|(pre-?\s?)?final\s+year)\b`)
	pgDegree = regexp.MustCompile(`(?i:\bm[.\-]?\s?tech\b|\bmaster|\bmca\b|\bmba\b|\bph\.?\s?d\b|\bm\.?\s?sc\b|\bpost\s?-?graduat)|\bM\.\s?E\b`)
)

// degreeAt guesses which degree the person did at the school from words within 40 chars of a
// school mention: "btech", "pg", or "" when the text doesn't say (common: "Education: MNNIT").
// Precedence: explicit B.Tech > postgrad > branch/year hint. B.Tech beating postgrad covers
// "Dual Degree (B.Tech + M.Tech)" and errs toward showing someone rather than hiding them.
func degreeAt(variants []string, fields ...string) string {
	pg, hint := false, false
	for _, f := range fields {
		for _, v := range variants {
			for _, loc := range wordRe(v).FindAllStringIndex(f, -1) {
				win := f[max(0, loc[0]-40):min(len(f), loc[1]+40)]
				if ugDegree.MatchString(win) {
					return "btech"
				}
				pg = pg || pgDegree.MatchString(win)
				hint = hint || ugHint.MatchString(win)
			}
		}
	}
	if pg {
		return "pg"
	}
	if hint {
		return "btech"
	}
	return ""
}

type scored struct {
	Degree     string   `json:"degree"`
	Score      int      `json:"score"`
	Confidence string   `json:"confidence"`
	Reasons    []string `json:"reasons"`
}

// score is the transparent relevance rule set; Reasons is shown as the tooltip.
func score(company string, variants []string, title, snippet, headline string) scored {
	var s scored
	add := func(n int, why string) {
		s.Score += n
		s.Reasons = append(s.Reasons, why)
	}
	both := title + "\n" + snippet
	for _, v := range variants {
		if wordRe(v).MatchString(both) {
			add(3, "+3 school ("+v+") mentioned")
			break
		}
	}
	co := wordRe(company)
	switch {
	case co.MatchString(title):
		add(3, "+3 company in title (current-company signal)")
	case co.MatchString(snippet):
		add(1, "+1 company only in snippet")
	}
	lh := strings.ToLower(headline)
	for _, k := range roleKeywords {
		if wordRe(k).MatchString(lh) {
			add(2, "+2 engineering role ("+k+")")
			break
		}
	}
	switch s.Degree = degreeAt(variants, title, snippet); s.Degree {
	case "btech":
		add(1, "+1 B.Tech at this college")
	case "pg":
		add(-4, "−4 postgrad (M.Tech/MCA/MBA/PhD) at this college, not B.Tech")
	}
	if pastMention(both, co) {
		add(-3, "−3 looks like a past employee (ex-/former/previously near company)")
	}
	switch {
	case s.Score >= 6:
		s.Confidence = "high"
	case s.Score >= 3:
		s.Confidence = "medium"
	default:
		s.Confidence = "low"
	}
	return s
}
