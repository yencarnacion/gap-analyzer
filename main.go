// main.go
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// ========================= Config & Embeds =========================

//go:embed web/index.html
var indexHTML string

var (
	apiKeyFlag = flag.String("apikey", "", "Polygon.io API key (overrides .env)")
	portFlag   = flag.Int("port", 0, "HTTP port (overrides .env)")
)

var (
	polygonAPIKey string
	listenPort    int
)

// ========================= Polygon Types =========================

type polygonBar struct {
	T int64   `json:"t"` // ms epoch (start of interval for intraday)
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"`
}

type polygonResp struct {
	Results []polygonBar `json:"results"`
}

// ========================= Gap Analysis Types =========================

type GapPoint struct {
	Date            string  `json:"date"`                 // YYYY-MM-DD (NY)
	GapPct          float64 `json:"gap_pct"`              // (open-prevClose)/prevClose * 100
	DailyReturnPct  float64 `json:"daily_return_pct"`     // (close-open)/open * 100
	Direction       int     `json:"direction"`            // 1 gap-up, -1 gap-down
	SameDir         int     `json:"same_dir"`             // 1 continuation (close dir == gap dir)
	Filled          int     `json:"filled"`               // gap filled intraday
	Bin             string  `json:"bin"`                  // gap bin label
	Open            float64 `json:"open,omitempty"`
	Close           float64 `json:"close,omitempty"`
	PrevClose       float64 `json:"prev_close,omitempty"`
	DayOfWeek       string  `json:"dow,omitempty"`        // Mon..Fri

	// New: 0–15m snapshot (to 09:45 ET)
	Ret15mPct     float64 `json:"ret_15m_pct,omitempty"`     // (09:45 - 09:30) / 09:30 * 100
	FilledBy0945  int     `json:"filled_by_0945,omitempty"`  // gap filled within first 15m
}

// Daily (close-to-open) bin stats
type BinStat struct {
	Label            string  `json:"label"`
	Count            int     `json:"count"`
	ContinuationRate float64 `json:"continuation_rate"`
	GapFillRate      float64 `json:"gap_fill_rate"`
	FadeAvg          float64 `json:"fade_avg"`
	FollowAvg        float64 `json:"follow_avg"`
	Recommendation   string  `json:"recommendation"` // FOLLOW | FADE | NEUTRAL
}

type SideStat struct {
	Count            int     `json:"count"`
	ContinuationRate float64 `json:"continuation_rate"`
	FadeAvg          float64 `json:"fade_avg"`
	FollowAvg        float64 `json:"follow_avg"`
}

type DowStat struct {
	Count            int     `json:"count"`
	ContinuationRate float64 `json:"continuation_rate"`
	FadeAvg          float64 `json:"fade_avg"`
	FollowAvg        float64 `json:"follow_avg"`
}

type Summary struct {
	Sessions         int     `json:"sessions"`
	ContinuationRate float64 `json:"continuation_rate"`
	GapUps           int     `json:"gap_ups"`
	GapDowns         int     `json:"gap_downs"`
	MeanGap          float64 `json:"mean_gap"`
	MaxGapUp         float64 `json:"max_gap_up"`
	MaxGapDown       float64 `json:"max_gap_down"`
	FadeAvg          float64 `json:"fade_avg"`
	FollowAvg        float64 `json:"follow_avg"`
	BestStrategy     string  `json:"best_strategy"`
	ExpectedReturn   float64 `json:"expected_return"`
}

// New: 0–15m summary + bins
type Summary15 struct {
	Sessions             int     `json:"sessions"`
	ContinuationRate     float64 `json:"continuation_rate"`       // to 09:45
	FadeAvg              float64 `json:"fade_avg"`                // avg % per trade (0–15m)
	FollowAvg            float64 `json:"follow_avg"`              // avg % per trade (0–15m)
	BestStrategy         string  `json:"best_strategy"`           // FADE/FOLLOW/NEUTRAL (0–15m)
	ExpectedReturn       float64 `json:"expected_return"`         // best strategy expected (0–15m)
	GapFillBy0945Rate    float64 `json:"gap_fill_by_0945_rate"`   // %
}

type BinStat15 struct {
	Label               string  `json:"label"`
	Count               int     `json:"count"`
	ContinuationRate    float64 `json:"continuation_rate"`      // to 09:45
	GapFillBy0945Rate   float64 `json:"gap_fill_by_0945_rate"`  // %
	FadeAvg             float64 `json:"fade_avg"`               // 0–15m
	FollowAvg           float64 `json:"follow_avg"`             // 0–15m
	Recommendation      string  `json:"recommendation"`         // FOLLOW | FADE | NEUTRAL
}

type AnalyzeResponse struct {
	Success bool       `json:"success"`
	Error   string     `json:"error,omitempty"`
	Ticker  string     `json:"ticker"`
	Years   int        `json:"years"`
	MinGap  float64    `json:"min_gap"`
	Data    []GapPoint `json:"data"`

	// Daily (close-to-open) analytics
	Summary  Summary            `json:"summary"`
	Bins     []BinStat          `json:"bins"`
	ByDOW    map[string]DowStat `json:"by_dow"`
	UpSide   SideStat           `json:"gap_up"`
	DownSide SideStat           `json:"gap_down"`

	CumDates  []string  `json:"cum_dates"`
	CumFade   []float64 `json:"cum_fade"`
	CumFollow []float64 `json:"cum_follow"`

	// New: 0–15m analytics
	Summary15  Summary15           `json:"summary_15m"`
	Bins15     []BinStat15         `json:"bins_15m"`
	ByDOW15    map[string]DowStat  `json:"by_dow_15m"`
	UpSide15   SideStat            `json:"gap_up_15m"`
	DownSide15 SideStat            `json:"gap_down_15m"`
}

// ========================= Helpers =========================

func sign(x float64) int {
	if x > 0 {
		return 1
	}
	if x < 0 {
		return -1
	}
	return 0
}

func pct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return (a / b * 100.0)
}

func toNY(t time.Time) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return t.In(loc)
}

func dateNY(tms int64) string {
	return toNY(time.UnixMilli(tms)).Format("2006-01-02")
}

func weekdayNY(tms int64) string {
	return toNY(time.UnixMilli(tms)).Weekday().String()[:3] // Mon Tue Wed Thu Fri
}

// Daily bars (RTH) — unadjusted for literal tape gaps
func fetchPolygonDaily(ticker, from, to string) ([]polygonBar, error) {
	url := fmt.Sprintf(
		"https://api.polygon.io/v2/aggs/ticker/%s/range/1/day/%s/%s?adjusted=false&sort=asc&apiKey=%s",
		ticker, from, to, polygonAPIKey,
	)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("polygon: %s", resp.Status)
	}
	var pr polygonResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Results, nil
}

// New: 15-minute bars — we use the first (09:30–09:45) each day
func fetchPolygon15Min(ticker, from, to string) ([]polygonBar, error) {
	url := fmt.Sprintf(
		"https://api.polygon.io/v2/aggs/ticker/%s/range/15/minute/%s/%s?adjusted=false&sort=asc&limit=50000&apiKey=%s",
		ticker, from, to, polygonAPIKey,
	)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("polygon (15m): %s", resp.Status)
	}
	var pr polygonResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Results, nil
}

func openBrowser(u string) {
	// Prefer Google Chrome; fall back to xdg-open
	if err := exec.Command("google-chrome", "--new-tab", u).Start(); err != nil {
		_ = exec.Command("chromium-browser", "--new-tab", u).Start()
		_ = exec.Command("xdg-open", u).Start()
	}
}

// ========================= Core Analysis =========================

type gapBin struct {
	min float64
	max float64
	lab string
}

func defaultBins(minGap float64) []gapBin {
	start := minGap
	if start < 0.1 {
		start = 0.1
	}
	return []gapBin{
		{min: start, max: 0.5, lab: fmt.Sprintf("%.1f–0.5%%", start)},
		{min: 0.5, max: 1.0, lab: "0.5–1.0%"},
		{min: 1.0, max: 1.5, lab: "1.0–1.5%"},
		{min: 1.5, max: 99.0, lab: ">1.5%"},
	}
}

func labelFor(absGap float64, bins []gapBin) string {
	for _, b := range bins {
		if absGap >= b.min && absGap < b.max {
			return b.lab
		}
	}
	return "other"
}

func analyzeGaps(daily []polygonBar, m15 []polygonBar, minGap float64, years int, ticker string) AnalyzeResponse {
	resp := AnalyzeResponse{
		Success: true,
		Ticker:  ticker,
		Years:   years,
		MinGap:  minGap,
	}
	if len(daily) < 2 {
		resp.Success = false
		resp.Error = "not enough data"
		return resp
	}

	// Build a map: YYYY-MM-DD (NY) -> first 15m bar (09:30–09:45)
	first15 := map[string]polygonBar{}
	for _, b := range m15 {
		ny := toNY(time.UnixMilli(b.T))
		if ny.Hour() == 9 && ny.Minute() == 30 {
			first15[ny.Format("2006-01-02")] = b
		}
	}

	bins := defaultBins(minGap)
	type agg struct {
		count, cont, filled int
		sumFade, sumFollow  float64
	}
	type agg15 struct {
		count, cont, filledBy0945 int
		sumFade, sumFollow        float64
	}

	binAgg := map[string]*agg{}
	binAgg15 := map[string]*agg15{}
	for _, b := range bins {
		binAgg[b.lab] = &agg{}
		binAgg15[b.lab] = &agg15{}
	}

	upAgg := agg{}
	downAgg := agg{}
	dowAgg := map[string]*agg{"Mon": {}, "Tue": {}, "Wed": {}, "Thu": {}, "Fri": {}}

	upAgg15 := agg15{}
	downAgg15 := agg15{}
	dowAgg15 := map[string]*agg15{"Mon": {}, "Tue": {}, "Wed": {}, "Thu": {}, "Fri": {}}

	points := make([]GapPoint, 0, len(daily)-1)

	var fadeSum, followSum float64
	var contCount int
	var upCount, downCount int
	var meanAbsGap float64
	var maxGapUp, maxGapDown float64
	var cumFade, cumFollow float64
	var cumDates []string
	var cumFadeArr, cumFollowArr []float64

	// New: 0–15m aggregates
	var fadeSum15, followSum15 float64
	var contCount15, filledBy0945Count, sessions15 int

	for i := 1; i < len(daily); i++ {
		prev := daily[i-1]
		day := daily[i]

		prevClose := prev.C
		open := day.O
		close := day.C

		if prevClose <= 0 || open <= 0 {
			continue
		}
		gapPct := (open - prevClose) / prevClose * 100.0
		if math.Abs(gapPct) < minGap {
			continue
		}
		dr := (close - open) / open * 100.0
		dir := sign(gapPct)
		same := 0
		if sign(dr) == dir && dir != 0 && dr != 0 {
			same = 1
		}
		// Daily gap fill
		filled := 0
		if (dir == 1 && day.L <= prevClose) || (dir == -1 && day.H >= prevClose) {
			filled = 1
		}

		absGap := math.Abs(gapPct)
		bin := labelFor(absGap, bins)
		dow := weekdayNY(day.T)

		// Strategy returns per trade (close-to-open window)
		followRet := float64(dir) * dr
		fadeRet := -float64(dir) * dr

		if dir == 1 {
			upCount++
		} else if dir == -1 {
			downCount++
		}
		if same == 1 {
			contCount++
		}
		meanAbsGap += absGap
		if gapPct > maxGapUp {
			maxGapUp = gapPct
		}
		if gapPct < maxGapDown {
			maxGapDown = gapPct
		}

		followSum += followRet
		fadeSum += fadeRet

		cumDates = append(cumDates, dateNY(day.T))
		cumFollow += followRet
		cumFade += fadeRet
		cumFollowArr = append(cumFollowArr, round3(cumFollow))
		cumFadeArr = append(cumFadeArr, round3(cumFade))

		// bin & side & DOW aggregates — daily
		if ba := binAgg[bin]; ba != nil {
			ba.count++
			ba.sumFollow += followRet
			ba.sumFade += fadeRet
			if same == 1 {
				ba.cont++
			}
			if filled == 1 {
				ba.filled++
			}
		}
		if dir == 1 {
			upAgg.count++
			upAgg.sumFollow += followRet
			upAgg.sumFade += fadeRet
			if same == 1 {
				upAgg.cont++
			}
		} else {
			downAgg.count++
			downAgg.sumFollow += followRet
			downAgg.sumFade += fadeRet
			if same == 1 {
				downAgg.cont++
			}
		}
		if da := dowAgg[dow]; da != nil {
			da.count++
			da.sumFollow += followRet
			da.sumFade += fadeRet
			if same == 1 {
				da.cont++
			}
		}

		// ==== New: 0–15m window (to 09:45 ET) ====
		dateStr := dateNY(day.T)
		var ret15 float64
		var cont15, filled0945 int
		if fb, ok := first15[dateStr]; ok && open > 0 {
			ret15 = (fb.C - open) / open * 100.0
			if sign(ret15) == dir && dir != 0 && ret15 != 0 {
				cont15 = 1
			}
			if (dir == 1 && fb.L <= prevClose) || (dir == -1 && fb.H >= prevClose) {
				filled0945 = 1
			}

			followRet15 := float64(dir) * ret15
			fadeRet15 := -float64(dir) * ret15

			followSum15 += followRet15
			fadeSum15 += fadeRet15
			contCount15 += cont15
			filledBy0945Count += filled0945
			sessions15++

			// bin/side/dow aggregates — 0–15m
			if ba := binAgg15[bin]; ba != nil {
				ba.count++
				ba.sumFollow += followRet15
				ba.sumFade += fadeRet15
				if cont15 == 1 {
					ba.cont++
				}
				if filled0945 == 1 {
					ba.filledBy0945++
				}
			}
			if dir == 1 {
				upAgg15.count++
				upAgg15.sumFollow += followRet15
				upAgg15.sumFade += fadeRet15
				if cont15 == 1 {
					upAgg15.cont++
				}
				if filled0945 == 1 {
					upAgg15.filledBy0945++
				}
			} else {
				downAgg15.count++
				downAgg15.sumFollow += followRet15
				downAgg15.sumFade += fadeRet15
				if cont15 == 1 {
					downAgg15.cont++
				}
				if filled0945 == 1 {
					downAgg15.filledBy0945++
				}
			}
			if da := dowAgg15[dow]; da != nil {
				da.count++
				da.sumFollow += float64(dir) * ret15
				da.sumFade += -float64(dir) * ret15
				if cont15 == 1 {
					da.cont++
				}
				if filled0945 == 1 {
					da.filledBy0945++
				}
			}
		}

		points = append(points, GapPoint{
			Date:           dateStr,
			GapPct:         round3(gapPct),
			DailyReturnPct: round3(dr),
			Direction:      dir,
			SameDir:        same,
			Filled:         filled,
			Bin:            bin,
			Open:           open,
			Close:          close,
			PrevClose:      prevClose,
			DayOfWeek:      dow,

			Ret15mPct:    round3(ret15),
			FilledBy0945: filled0945,
		})
	}

	// Build response pieces — daily
	resp.Data = points
	resp.CumDates = cumDates
	resp.CumFade = cumFadeArr
	resp.CumFollow = cumFollowArr

	total := len(points)
	var contRate float64
	if total > 0 {
		contRate = float64(contCount) / float64(total) * 100.0
	}
	var fadeAvg, followAvg float64
	if total > 0 {
		fadeAvg = fadeSum / float64(total)
		followAvg = followSum / float64(total)
	}
	best := "NEUTRAL"
	exp := 0.0
	if followAvg > fadeAvg {
		best = "FOLLOW"
		exp = followAvg
	} else if fadeAvg > followAvg {
		best = "FADE"
		exp = fadeAvg
	}
	meanAbsGapPct := 0.0
	if total > 0 {
		meanAbsGapPct = meanAbsGap / float64(total)
	}

	resp.Summary = Summary{
		Sessions:         total,
		ContinuationRate: round1(contRate),
		GapUps:           upCount,
		GapDowns:         downCount,
		MeanGap:          round2(meanAbsGapPct),
		MaxGapUp:         round2(maxGapUp),
		MaxGapDown:       round2(maxGapDown),
		FadeAvg:          round3(fadeAvg),
		FollowAvg:        round3(followAvg),
		BestStrategy:     best,
		ExpectedReturn:   round3(exp),
	}

	// Bins — daily
	outBins := make([]BinStat, 0, len(bins))
	for _, b := range bins {
		ba := binAgg[b.lab]
		if ba == nil || ba.count == 0 {
			outBins = append(outBins, BinStat{Label: b.lab})
			continue
		}
		cr := float64(ba.cont) / float64(ba.count) * 100.0
		gr := float64(ba.filled) / float64(ba.count) * 100.0
		fa := ba.sumFade / float64(ba.count)
		fo := ba.sumFollow / float64(ba.count)
		rec := "NEUTRAL"
		if cr > 60 {
			rec = "FOLLOW"
		} else if cr < 40 {
			rec = "FADE"
		}
		outBins = append(outBins, BinStat{
			Label:            b.lab,
			Count:            ba.count,
			ContinuationRate: round1(cr),
			GapFillRate:      round1(gr),
			FadeAvg:          round3(fa),
			FollowAvg:        round3(fo),
			Recommendation:   rec,
		})
	}
	sort.Slice(outBins, func(i, j int) bool { return i < j })
	resp.Bins = outBins

	resp.UpSide = SideStat{
		Count:            upAgg.count,
		ContinuationRate: rate(upAgg.cont, upAgg.count),
		FadeAvg:          avg(upAgg.sumFade, upAgg.count),
		FollowAvg:        avg(upAgg.sumFollow, upAgg.count),
	}
	resp.DownSide = SideStat{
		Count:            downAgg.count,
		ContinuationRate: rate(downAgg.cont, downAgg.count),
		FadeAvg:          avg(downAgg.sumFade, downAgg.count),
		FollowAvg:        avg(downAgg.sumFollow, downAgg.count),
	}
	resp.ByDOW = map[string]DowStat{}
	for k, v := range dowAgg {
		resp.ByDOW[k] = DowStat{
			Count:            v.count,
			ContinuationRate: rate(v.cont, v.count),
			FadeAvg:          avg(v.sumFade, v.count),
			FollowAvg:        avg(v.sumFollow, v.count),
		}
	}

	// ===== New: 0–15m summary/bins/splits =====
	var contRate15, fadeAvg15, followAvg15, fill0945Rate float64
	if sessions15 > 0 {
		contRate15 = float64(contCount15) / float64(sessions15) * 100.0
		fadeAvg15 = fadeSum15 / float64(sessions15)
		followAvg15 = followSum15 / float64(sessions15)
		fill0945Rate = float64(filledBy0945Count) / float64(sessions15) * 100.0
	}
	best15 := "NEUTRAL"
	exp15 := 0.0
	if followAvg15 > fadeAvg15 {
		best15 = "FOLLOW"
		exp15 = followAvg15
	} else if fadeAvg15 > followAvg15 {
		best15 = "FADE"
		exp15 = fadeAvg15
	}

	resp.Summary15 = Summary15{
		Sessions:          sessions15,
		ContinuationRate:  round1(contRate15),
		FadeAvg:           round3(fadeAvg15),
		FollowAvg:         round3(followAvg15),
		BestStrategy:      best15,
		ExpectedReturn:    round3(exp15),
		GapFillBy0945Rate: round1(fill0945Rate),
	}

	// Bins — 0–15m
	outBins15 := make([]BinStat15, 0, len(bins))
	for _, b := range bins {
		ba := binAgg15[b.lab]
		if ba == nil || ba.count == 0 {
			outBins15 = append(outBins15, BinStat15{Label: b.lab})
			continue
		}
		cr := float64(ba.cont) / float64(ba.count) * 100.0
		gr := float64(ba.filledBy0945) / float64(ba.count) * 100.0
		fa := ba.sumFade / float64(ba.count)
		fo := ba.sumFollow / float64(ba.count)
		rec := "NEUTRAL"
		if cr > 60 {
			rec = "FOLLOW"
		} else if cr < 40 {
			rec = "FADE"
		}
		outBins15 = append(outBins15, BinStat15{
			Label:             b.lab,
			Count:             ba.count,
			ContinuationRate:  round1(cr),
			GapFillBy0945Rate: round1(gr),
			FadeAvg:           round3(fa),
			FollowAvg:         round3(fo),
			Recommendation:    rec,
		})
	}
	sort.Slice(outBins15, func(i, j int) bool { return i < j })
	resp.Bins15 = outBins15

	resp.UpSide15 = SideStat{
		Count:            upAgg15.count,
		ContinuationRate: rate(upAgg15.cont, upAgg15.count),
		FadeAvg:          avg(upAgg15.sumFade, upAgg15.count),
		FollowAvg:        avg(upAgg15.sumFollow, upAgg15.count),
	}
	resp.DownSide15 = SideStat{
		Count:            downAgg15.count,
		ContinuationRate: rate(downAgg15.cont, downAgg15.count),
		FadeAvg:          avg(downAgg15.sumFade, downAgg15.count),
		FollowAvg:        avg(downAgg15.sumFollow, downAgg15.count),
	}

	resp.ByDOW15 = map[string]DowStat{}
	for k, v := range dowAgg15 {
		resp.ByDOW15[k] = DowStat{
			Count:            v.count,
			ContinuationRate: rate(v.cont, v.count),
			FadeAvg:          avg(v.sumFade, v.count),
			FollowAvg:        avg(v.sumFollow, v.count),
		}
	}

	return resp
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return round1(float64(n) / float64(d) * 100.0)
}
func avg(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return round3(sum / float64(n))
}
func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// ========================= HTTP Handlers =========================

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func handleAnalyze(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	if ticker == "" {
		http.Error(w, "ticker required", http.StatusBadRequest)
		return
	}
	years := 3
	if y := strings.TrimSpace(q.Get("years")); y != "" {
		if v, err := strconv.Atoi(y); err == nil && v >= 1 && v <= 5 {
			years = v
		}
	}
	minGap := 0.3
	if mg := strings.TrimSpace(q.Get("minGap")); mg != "" {
		if v, err := strconv.ParseFloat(mg, 64); err == nil && v > 0 && v < 20 {
			minGap = v
		}
	}

	now := time.Now()
	from := now.AddDate(-years, 0, 0).Format("2006-01-02")
	to := now.Format("2006-01-02")

	daily, err := fetchPolygonDaily(ticker, from, to)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	m15, err := fetchPolygon15Min(ticker, from, to)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	resp := analyzeGaps(daily, m15, minGap, years, ticker)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ========================= Main =========================

func main() {
	_ = godotenv.Load()
	flag.Parse()

	if *apiKeyFlag != "" {
		polygonAPIKey = *apiKeyFlag
	} else {
		polygonAPIKey = os.Getenv("POLYGON_API_KEY")
	}
	if polygonAPIKey == "" {
		log.Fatal("Missing POLYGON_API_KEY (flag or .env)")
	}

	if *portFlag != 0 {
		listenPort = *portFlag
	} else if p := os.Getenv("PORT"); p != "" {
		fmt.Sscanf(p, "%d", &listenPort)
	}
	if listenPort == 0 {
		listenPort = 8083
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/gaps", handleAnalyze)

	addr := fmt.Sprintf(":%d", listenPort)
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://localhost" + addr)
	}()
	log.Printf("Gap Analyzer running on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
