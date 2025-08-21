## US Stocks Gap Analyzer (Fade vs Follow) — Go + Polygon + Chart.js

A fast, single-binary web app that tells you — with data — whether to fade or follow opening gaps for US equities. It backtests up to 5 years of daily sessions, bins by gap size/day-of-week, and reports expected returns, win rates, and gap‑fill behavior. The app launches a local web dashboard automatically.

> Research tool only — not investment advice.

### Highlights

- Gap math uses US RTH session times: 09:30 ET open versus prior session close (≈16:00 ET)
- Intraday add‑on: first 15 minutes (to 09:45 ET) using 15‑minute Polygon bars
- Breakdowns: continuation rate, gap‑up vs gap‑down, size bins, day‑of‑week, gap‑fill
- Strategy tests: Fade vs Follow (avg %/trade, cumulative curves, plus 0–15m)
- Single binary; UI is embedded with go:embed (Chart.js + Axios via CDN)

---

## Quick start

1) Prerequisites
- Go 1.23+
- A Polygon.io API key (free tier works; be mindful of rate limits)

2) Setup

```bash
git clone https://github.com/yourname/gap-analyzer
cd gap-analyzer
cp env.example .env
$EDITOR .env   # set POLYGON_API_KEY, optional PORT
```

3) Run

```bash
# Option A: env file
go run .

# Option B: pass flags (overrides .env)
go run . -apikey YOUR_KEY -port 8083

# Or via helper
./go.sh
```

The app starts on `http://localhost:8083` by default and will try to open your browser (Chrome/Chromium/xdg-open).

---

## Usage

### Web UI
- Enter a US stock ticker (e.g., AAPL), select years (1–5), choose a minimum gap %, and click Analyze.
- Dashboard panels include overall metrics, first‑15‑minutes snapshot, side‑by‑side gap‑up vs gap‑down stats, distributions, scatter, strategy bars, cumulative performance, and tables by gap bin and day of week.

### REST API
Endpoint
```
GET /api/gaps?ticker=SYMBOL&years=1..5&minGap=0.1..20
```

Examples
```bash
curl 'http://localhost:8083/api/gaps?ticker=AAPL&years=3&minGap=0.3' | jq .summary
curl 'http://localhost:8083/api/gaps?ticker=SPY&years=5&minGap=0.5' | jq .bins
```

Parameters
- ticker: required, e.g., AAPL, SPY
- years: optional, default 3, range 1–5
- minGap: optional, default 0.3 (%). Must be > 0 and < 20

Selected response fields
- `data[]`: per‑session points with `date`, `gap_pct`, `daily_return_pct`, `direction`, `same_dir`, `filled`, `bin`, `ret_15m_pct`, `filled_by_0945`
- `summary`: daily close→open analytics; includes `continuation_rate`, `fade_avg`, `follow_avg`, `best_strategy`, `expected_return`, gap counts and sizes
- `summary_15m`: first 15‑minutes snapshot; includes continuation, fade/follow averages, best strategy, and gap‑fill by 09:45
- `bins` and `bins_15m`: per gap‑size bin metrics (count, continuation rate, gap‑fill, fade/follow returns, recommendation)
- `gap_up`/`gap_down` and `gap_up_15m`/`gap_down_15m`: splits by gap direction
- `by_dow` and `by_dow_15m`: day‑of‑week stats
- `cum_dates`, `cum_fade`, `cum_follow`: cumulative paths of strategy returns (daily window)

---

## How it works

### Data
- Daily RTH aggregates (1d bars) from Polygon.io for close and next‑day open
- 15‑minute aggregates to obtain the first bar of the session (09:30–09:45 ET)

### Definitions
- Gap %: `(09:30 open − prior close) / prior close * 100`
- Daily return %: `(close − open) / open * 100`
- Direction: `+1` if gap‑up, `-1` if gap‑down
- Continuation (daily): sign of daily return equals gap direction
- Gap fill (daily): price trades back to prior close intraday
- 0–15m return %: `(09:45 close − 09:30 open) / 09:30 open * 100`
- Gap fill by 09:45: prior close touched within the first 15 minutes

### Binning and recommendations
- Default bins: `[max(minGap, 0.1)–0.5%]`, `[0.5–1.0%]`, `[1.0–1.5%]`, `[>1.5%]`
- Per bin we compute counts, continuation rate, gap‑fill rate, Fade/Follow averages, and a coarse recommendation:
  - FOLLOW if continuation > 60%
  - FADE if continuation < 40%
  - NEUTRAL otherwise

### Strategy returns
- Follow: align with gap direction; per‑trade return equals `sign(gap) * daily_return`
- Fade: oppose gap direction; per‑trade return equals `-sign(gap) * daily_return`
- For 0–15m, the same logic applies using the 09:30→09:45 return
- Cumulative curves are simple running sums ordered by date (no compounding)

---

## Configuration

Environment (`.env` or process env)
- `POLYGON_API_KEY`: required unless provided via `-apikey`
- `PORT`: optional, defaults to 8083

Flags (override env)
- `-apikey`: Polygon.io API key
- `-port`: HTTP port

Time zone
- All session logic uses America/New_York; dates and weekday labels are New York time

---

## Development

Project layout
- `main.go`: server, data fetching, analytics, and API
- `web/index.html`: embedded UI (go:embed), Chart.js + Axios via CDN
- `env.example`: template for `.env`
- `go.sh`: convenience runner (`go run .`)

Common tasks
```bash
# Run with hot compile
go run .

# Build a static binary
go build -o gap-analyzer
./gap-analyzer -apikey YOUR_KEY
```

Go version and deps
- `go 1.23`
- `github.com/joho/godotenv` for `.env` loading

---

## Notes & limitations
- Polygon free tier has rate limits; excessive requests can fail with 429/5xx
- Uses unadjusted daily aggregates as provided; corporate actions and true overnight tape gaps are not normalized beyond bar definitions
- Only US trading days (Mon–Fri); holidays/half days are as reflected by Polygon bars
- Backtests are simplified and do not include transaction costs, slippage, borrow, or risk management
- Recommendations are heuristic and for research only

---
