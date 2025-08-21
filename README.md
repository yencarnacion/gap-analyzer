# US Stocks Gap Analyzer (Fade vs Follow) — Go + Polygon + Chart.js

A fast, single-binary web app that tells you — with **real stats** — whether to **fade** or **follow** opening gaps for US stocks. It backtests up to 5 years of daily sessions, binning by gap size and day-of-week, and reports **expected returns**, **win rates**, and **gap-fill behavior**. Runs locally, opens Chrome, and serves a beautiful dashboard.

> **Research tool only — not investment advice.**

---

## ✨ Features

- **Gap math rooted in market session times**
  - **Gap** = **09:30 ET** open vs **prior session close** (typically **16:00 ET**).
  - Uses Polygon **daily** aggregates (RTH only), one request for entire range.
- **Stats & breakdowns**
  - **Continuation rate** (momentum vs mean reversion)
  - **Gap-up** vs **gap-down** splits
  - **Gap-size bins** (0.3–0.5%, 0.5–1.0%, 1.0–1.5%, >1.5%)
  - **Day-of-week** effects
  - **Gap-fill rate**
- **Backtests two simple strategies**
  - **Follow**: go with the gap direction (long after gap-up, short after gap-down)
  - **Fade**: bet on reversal
  - Reports **avg % per trade** and **cumulative curves**
- **Actionable guidance**
  - Best strategy (**FADE**/**FOLLOW**) with **expected return**
  - Risk hints (e.g., stop at gap fill, target = 1.5× gap)
- **Polished UI**
  - Neon/terminal theme, responsive layout
  - Distribution, scatter, bar, and cumulative charts
  - Gap-bin & Day-of-week tables

---

## 🧮 How the gap is calculated

**Definition:**  
> `Gap% = ( Open_today_09:30 - Close_prior_session ) / Close_prior_session * 100`

In code (`main.go → analyzeGaps`):

```go
prevClose := prev.C  // prior day official RTH close (≈16:00 ET)
open      := day.O   // current day official RTH open (≈09:30 ET)
gapPct    := (open - prevClose) / prevClose * 100
