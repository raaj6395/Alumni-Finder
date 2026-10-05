package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	n, err := strconv.Atoi(env(k, strconv.Itoa(def)))
	if err != nil || n < 1 {
		slog.Warn("invalid env var, using default", "var", k, "default", def)
		return def
	}
	return n
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	db, err := openDB(env("DB_PATH", "./data/alumni.db"))
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	a := &app{
		db:       db,
		s:        newSearcher(db, os.Getenv("SERPER_API_KEY"), time.Duration(envInt("CACHE_TTL_DAYS", 7))*24*time.Hour),
		maxPages: envInt("XRAY_MAX_PAGES", 3),
	}
	if n, err := a.rescoreAll(context.Background()); err != nil {
		slog.Error("rescore people", "err", err)
		os.Exit(1)
	} else {
		slog.Info("rescored stored people with current rules", "count", n)
	}
	for _, k := range []string{"SCHOOL_ID", "SCHOOL_NAME_VARIANTS"} {
		if os.Getenv(k) != "" {
			slog.Warn("env var is ignored now; colleges are configured in schools.go", "var", k)
		}
	}
	if !a.s.enabled() {
		slog.Warn("SERPER_API_KEY not set: X-ray search disabled, alumni links still work")
	}
	addr := "127.0.0.1:" + env("PORT", "8080")
	slog.Info("listening", "url", "http://"+addr)
	if err := http.ListenAndServe(addr, a.routes()); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	m.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"xray_enabled": a.s.enabled(), "schools": schools})
	})
	m.HandleFunc("GET /api/usage", handle(func(r *http.Request) (any, error) { return a.usage(r.Context()) }))
	m.HandleFunc("GET /api/companies", handle(func(r *http.Request) (any, error) { return a.listCompanies(r.Context()) }))
	m.HandleFunc("POST /api/search", handle(func(r *http.Request) (any, error) {
		var in struct {
			Name    string `json:"name"`
			School  string `json:"school"`
			Refresh bool   `json:"refresh"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		sc, err := schoolByKey(in.School)
		if err != nil {
			return nil, err
		}
		// Detach from the request so a closed tab doesn't abort a half-stored run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
		defer cancel()
		return a.run(ctx, in.Name, sc, in.Refresh)
	}))
	m.HandleFunc("GET /api/companies/{id}", handle(func(r *http.Request) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		sc, err := schoolByKey(r.URL.Query().Get("school"))
		if err != nil {
			return nil, err
		}
		c, err := a.getCompany(r.Context(), id, sc)
		if err != nil {
			return nil, err
		}
		people, err := a.listPeople(r.Context(), id, sc.Key)
		return &searchResult{School: sc, Company: c, People: people, Candidates: []candidate{},
			Summary: summary{XrayActive: a.s.enabled(), Elsewhere: []string{}}}, err
	}))
	m.HandleFunc("PATCH /api/companies/{id}", handle(func(r *http.Request) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var in struct {
			Slug   string `json:"slug"` // slug or LinkedIn company URL
			Source string `json:"source"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Source != "xray" && in.Source != "guess" {
			in.Source = "manual"
		}
		if err := a.setSlug(r.Context(), id, in.Slug, in.Source); err != nil {
			return nil, err
		}
		sc, err := schoolByKey(r.URL.Query().Get("school"))
		if err != nil {
			return nil, err
		}
		return a.getCompany(r.Context(), id, sc)
	}))
	m.HandleFunc("PATCH /api/people/{id}", handle(func(r *http.Request) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var in struct{ Status, Notes *string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, a.updatePerson(r.Context(), id, in.Status, in.Notes)
	}))
	return m
}

func handle(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			var ae *apiError
			if !errors.As(err, &ae) {
				slog.Error("request failed", "path", r.URL.Path, "err", err)
				ae = &apiError{500, "internal", err.Error()}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(ae.Status)
			json.NewEncoder(w).Encode(map[string]errJSON{"error": {ae.Code, ae.Message}})
			return
		}
		writeJSON(w, v)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return &apiError{400, "bad_json", err.Error()}
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, &apiError{400, "bad_id", "invalid id"}
	}
	return id, nil
}
