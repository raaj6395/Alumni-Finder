package main

import "testing"

func TestCompanySlug(t *testing.T) {
	ok := map[string]string{
		"https://www.linkedin.com/company/razorpay/":                             "razorpay",
		"https://www.linkedin.com/company/razorpay":                              "razorpay",
		"https://in.linkedin.com/company/razorpay":                               "razorpay",
		"https://sg.linkedin.com/company/grab/about/":                            "grab",
		"https://www.linkedin.com/company/razorpay/jobs?trk=abc":                 "razorpay",
		"https://www.linkedin.com/company/razorpay/life":                         "razorpay",
		"https://www.linkedin.com/company/razorpay/people/?x=1":                  "razorpay",
		"https://www.linkedin.com/company/Goldman-Sachs#about":                   "goldman-sachs",
		"https://www.linkedin.com/company/soci%C3%A9t%C3%A9-g%C3%A9n%C3%A9rale/": "société-générale",
		"linkedin.com/company/flipkart":                                          "flipkart",
		"  www.linkedin.com/company/swiggy-in  ":                                 "swiggy-in",
		"http://linkedin.com/company/1035/":                                      "1035",
	}
	for in, want := range ok {
		got, err := companySlug(in)
		if err != nil || got != want {
			t.Errorf("companySlug(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"https://www.linkedin.com/showcase/razorpay-capital/",
		"https://www.linkedin.com/school/mnnit-allahabad/",
		"https://www.linkedin.com/in/someone",
		"https://www.linkedin.com/company/",
		"https://evil-linkedin.com/company/x",
		"https://linkedin.com.evil.io/company/x",
		"https://www.google.com/company/x",
		"not a url at all",
		"",
	}
	for _, in := range bad {
		if got, err := companySlug(in); err == nil {
			t.Errorf("companySlug(%q) = %q, want error", in, got)
		}
	}
}

func TestProfileSlug(t *testing.T) {
	tests := []struct{ in, slug, url string }{
		{"https://www.linkedin.com/in/asha-verma-123", "asha-verma-123", "https://www.linkedin.com/in/asha-verma-123"},
		{"https://in.linkedin.com/in/asha-verma-123/", "asha-verma-123", "https://www.linkedin.com/in/asha-verma-123"},
		{"https://in.linkedin.com/in/Asha-Verma-123?originalSubdomain=in", "asha-verma-123", "https://www.linkedin.com/in/asha-verma-123"},
		{"https://www.linkedin.com/in/asha/details/experience/", "asha", "https://www.linkedin.com/in/asha"},
		{"https://www.linkedin.com/in/j%C3%B6rg-m", "jörg-m", "https://www.linkedin.com/in/j%C3%B6rg-m"},
		{"linkedin.com/in/x#top", "x", "https://www.linkedin.com/in/x"},
	}
	for _, tt := range tests {
		slug, url, err := profileSlug(tt.in)
		if err != nil || slug != tt.slug || url != tt.url {
			t.Errorf("profileSlug(%q) = %q, %q, %v; want %q, %q", tt.in, slug, url, err, tt.slug, tt.url)
		}
	}
	for _, in := range []string{"https://www.linkedin.com/pulse/x", "https://www.linkedin.com/company/x", "https://www.linkedin.com/in/", "https://x.com/in/y"} {
		if _, _, err := profileSlug(in); err == nil {
			t.Errorf("profileSlug(%q) want error", in)
		}
	}
}

func TestGuessAndNormalize(t *testing.T) {
	for in, want := range map[string]string{"Razorpay": "razorpay", " Goldman Sachs & Co. ": "goldman-sachs-co", "PhonePe": "phonepe", "D. E. Shaw": "d-e-shaw"} {
		if got := guessSlug(in); got != want {
			t.Errorf("guessSlug(%q) = %q want %q", in, got, want)
		}
	}
	if normalizeName("  Razorpay,  Inc. ") != normalizeName("razorpay inc") {
		t.Error("normalizeName should ignore case, punctuation and spacing")
	}
}

func TestURLs(t *testing.T) {
	if got := alumniURL("razorpay", "15105506"); got != "https://www.linkedin.com/company/razorpay/people/?facetSchool=15105506" {
		t.Error(got)
	}
	if got := companyURL("razorpay"); got != "https://www.linkedin.com/company/razorpay/" {
		t.Error(got)
	}
}
