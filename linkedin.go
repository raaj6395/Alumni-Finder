package main

// Pure LinkedIn URL helpers. Nothing here touches the network.

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// www., in., sg., uk., ... or bare linkedin.com
var linkedinHost = regexp.MustCompile(`^([a-z]{2,3}\.)?linkedin\.com$`)

// linkedinPath parses raw and returns its unescaped path segments if the host is LinkedIn.
func linkedinPath(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !linkedinHost.MatchString(strings.ToLower(u.Hostname())) {
		return nil, errors.New("not a linkedin.com URL")
	}
	return strings.Split(strings.Trim(u.Path, "/"), "/"), nil // u.Path is already unescaped
}

// companySlug extracts {slug} from linkedin.com/company/{slug}[/anything]. /showcase/ and /school/ are rejected.
func companySlug(raw string) (string, error) {
	seg, err := linkedinPath(raw)
	if err != nil {
		return "", err
	}
	if len(seg) < 2 || seg[0] != "company" || seg[1] == "" {
		return "", errors.New("not a linkedin.com/company/{slug} URL")
	}
	return strings.ToLower(seg[1]), nil
}

// profileSlug extracts the dedupe key and canonical URL from linkedin.com/in/{slug}.
func profileSlug(raw string) (slug, canonical string, err error) {
	seg, err := linkedinPath(raw)
	if err != nil {
		return "", "", err
	}
	if len(seg) < 2 || seg[0] != "in" || seg[1] == "" {
		return "", "", errors.New("not a linkedin.com/in/{slug} URL")
	}
	slug = strings.ToLower(seg[1])
	return slug, "https://www.linkedin.com/in/" + url.PathEscape(slug), nil
}

var nonSlug = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// guessSlug: "Goldman Sachs & Co." -> "goldman-sachs-co". Unverified.
func guessSlug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// normalizeName is the company dedupe key: "  Razorpay, Inc. " -> "razorpay inc".
func normalizeName(name string) string {
	return strings.TrimSpace(nonSlug.ReplaceAllString(strings.ToLower(name), " "))
}

func companyURL(slug string) string {
	return "https://www.linkedin.com/company/" + url.PathEscape(slug) + "/"
}

func alumniURL(slug, schoolID string) string {
	return "https://www.linkedin.com/company/" + url.PathEscape(slug) + "/people/?facetSchool=" + url.QueryEscape(schoolID)
}
