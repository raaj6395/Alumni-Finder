package main

import (
	"reflect"
	"strings"
	"testing"
)

var testVariants = []string{"MNNIT", "Motilal Nehru National Institute of Technology", "MNNIT Allahabad", "NIT Allahabad"}

func TestXrayQueries(t *testing.T) {
	got := xrayQueries("Razorpay", testVariants)
	want := []string{
		`site:linkedin.com/in ("MNNIT" OR "Motilal Nehru National Institute of Technology") "Razorpay"`,
		`site:linkedin.com/in "MNNIT Allahabad" "Razorpay"`,
		`site:linkedin.com/in "NIT Allahabad" "Razorpay"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if got := xrayQueries("X", []string{"MNNIT"}); len(got) != 1 || got[0] != `site:linkedin.com/in ("MNNIT") "X"` {
		t.Errorf("single variant: %q", got)
	}
}

func TestParseTitle(t *testing.T) {
	tests := []struct{ in, name, headline string }{
		{"Asha Verma - SDE 2 - Razorpay | LinkedIn", "Asha Verma", "SDE 2 - Razorpay"},
		{"Rohit K – Product Manager – Flipkart | LinkedIn", "Rohit K", "Product Manager - Flipkart"},
		{"Neha S. — Razorpay", "Neha S.", "Razorpay"},
		{"Neha S. - Razorpay - LinkedIn", "Neha S.", "Razorpay"},
		{"Amit Kumar | LinkedIn", "Amit Kumar", ""},
		{"Amit Kumar - Senior Software Engineer - Google | Linked...", "Amit Kumar", "Senior Software Engineer - Google"},
		{"Priya · LinkedIn", "Priya", ""},
		{"Rahul Singh - Backend Engineer at Swiggy ...", "Rahul Singh", "Backend Engineer at Swiggy"},
		{"Ana-Maria Pop - SWE - Uber | LinkedIn", "Ana-Maria Pop", "SWE - Uber"},
		{"", "", ""},
	}
	for _, tt := range tests {
		n, h := parseTitle(tt.in)
		if n != tt.name || h != tt.headline {
			t.Errorf("parseTitle(%q) = %q, %q; want %q, %q", tt.in, n, h, tt.name, tt.headline)
		}
	}
}

func TestScore(t *testing.T) {
	tests := []struct {
		name, company, title, snippet string
		want                          int
		conf                          string
	}{
		{"current SDE alum", "Razorpay", "Asha - SDE 2 - Razorpay | LinkedIn", "Education: MNNIT Allahabad", 8, "high"},
		{"school + company in title, non-eng", "Razorpay", "Asha - Product Manager - Razorpay | LinkedIn", "MNNIT", 6, "high"},
		{"company only in snippet", "Razorpay", "Asha - Product Manager - Flipkart | LinkedIn", "MNNIT. Razorpay.", 4, "medium"},
		{"ex- penalty", "Razorpay", "Rohit - PM - Flipkart | LinkedIn", "Ex-Razorpay. MNNIT.", 1, "low"},
		{"ex space penalty", "Razorpay", "Rohit - Ex Razorpay | LinkedIn", "MNNIT", 3, "medium"},
		{"former penalty", "Google", "Rohit - Engineer | LinkedIn", "Former Google engineer, MNNIT alum", 3, "medium"},
		{"previously penalty", "Google", "Rohit - SWE - Amazon | LinkedIn", "Previously at Google. MNNIT.", 3, "medium"},
		{"worked at penalty", "Uber", "X - SDE | LinkedIn", "Worked at Uber. MNNIT Allahabad", 3, "medium"},
		{"next is not ex", "Google", "X - Next Google SDE | LinkedIn", "MNNIT", 8, "high"},
		{"word boundary: Uber vs Uberization", "Uber", "X - Uberization lead | LinkedIn", "", 0, "low"},
		{"no school", "Razorpay", "X - SDE - Razorpay | LinkedIn", "", 5, "medium"},
		{"case-insensitive", "razorpay", "X - sde - RAZORPAY | LinkedIn", "mnnit", 8, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, h := parseTitle(tt.title)
			s := score(tt.company, testVariants, tt.title, tt.snippet, h)
			if s.Score != tt.want || s.Confidence != tt.conf {
				t.Errorf("score = %d %s, want %d %s; reasons:\n%s", s.Score, s.Confidence, tt.want, tt.conf, strings.Join(s.Reasons, "\n"))
			}
		})
	}
}
