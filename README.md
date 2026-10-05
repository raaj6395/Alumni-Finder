# MNNIT Alumni Finder

Type a company name and get MNNIT Allahabad alumni who work there, so you can ask them for referrals.

- **LinkedIn alumni link**: finds the company's LinkedIn slug and builds
  `linkedin.com/company/{slug}/people/?facetSchool=15105506`. Open it while logged in to see everyone.
- **Google X-ray** (needs a Serper.dev key): searches `site:linkedin.com/in "MNNIT" "{company}"` and
  similar queries, then ranks the profiles with simple rules. Hover over a score to see why it got that score.
- **Tracker**: set a status and notes for each person. Re-running a search keeps them.

The app never sends requests to linkedin.com. It only builds links for you to open.

## Run
```
cp .env.example .env      # paste your SERPER_API_KEY (free tier at serper.dev)
make dev                  # http://127.0.0.1:8080
```
`make build` builds one binary at `bin/alumni`. `make test` and `make lint` check the code.

Google results are cached for `CACHE_TTL_DAYS`. Each uncached result page costs 1 Serper credit, and a new
company costs at most 1 + 3 queries × `XRAY_MAX_PAGES`. The header shows how many calls you've used.
Tick "Refresh" to skip the cache.
