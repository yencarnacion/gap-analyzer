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
	T int64   `json:"t"` // ms epoch (for intraday: start of minute)
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
	Date            string  `json:"date"`                 // YYYY-MM-DD (NY session date)
	GapPct          float64 `json:"gap_pct"`              // (open-prevClose)/prevClose * 100
	DailyReturnPct  float64 `json:"daily_return_pct"`     // (close-open)/open * 100
	Direction       int     `json:"direction"`            // 1 gap-up, -1 gap-down
	SameDir         int     `json:"same_dir"`             // 1 continuation (close dir == gap dir)
	Filled          int     `json:"filled"`               // gap filled intraday (daily window)
	Bin             string  `json:"bin"`                  // gap bin label
	Open            float64 `json:"open,omitempty"`
	Close           float64 `json:"close,omitempty"`
	PrevClose       float64 `json:"prev_close,omitempty"`
	DayOfWeek       string  `json:"dow,omitempty"`        // Mon..Fri

	// 0–15m snapshot (to 09:45 ET) — from 1-minute bars
	Ret15mPct    float64 `json:"ret_15m_pct,omitempty"`     // (09:45 - 09:30) / 09:30 * 100
	FilledBy0945 int     `json:"filled_by_0945,omitempty"`  // gap filled within first 15m
}

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

type Summary15 struct {
	Sessions          int     `json:"sessions"`
	ContinuationRate  float64 `json:"continuation_rate"`       // to 09:45
	FadeAvg           float64 `json:"fade_avg"`                // avg % per trade (0–15m)
	FollowAvg         float64 `json:"follow_avg"`              // avg % per trade (0–15m)
	BestStrategy      string  `json:"best_strategy"`           // FADE/FOLLOW/NEUTRAL (0–15m)
	ExpectedReturn    float64 `json:"expected_return"`         // best strategy expected (0–15m)
	GapFillBy0945Rate float64 `json:"gap_fill_by_0945_rate"`   // %
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

	// Daily analytics
	Summary  Summary            `json:"summary"`
	Bins     []BinStat          `json:"bins"`
	ByDOW    map[string]DowStat `json:"by_dow"`
	UpSide   SideStat           `json:"gap_up"`
	DownSide SideStat           `json:"gap_down"`

	CumDates  []string  `json:"cum_dates"`
	CumFade   []float64 `json:"cum_fade"`
	CumFollow []float64 `json:"cum_follow"`

	// 0–15m analytics (from 1-minute bars)
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

// Polygon daily 't' is 00:00 UTC of the session; in NY this shows as previous calendar date.
// Shift +24h in NY to label by the actual RTH session date (date of the 09:30 open).
func sessionDateNYFromDaily(tms int64) string {
	return toNY(time.UnixMilli(tms)).Add(24 * time.Hour).Format("2006-01-02")
}
func sessionWeekdayNYFromDaily(tms int64) string {
	return toNY(time.UnixMilli(tms)).Add(24 * time.Hour).Weekday().String()[:3]
}

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
func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// ========================= Polygon fetchers =========================

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

// 1-minute bars for specific NY-session dates (from=to=date). Returns a map[YYYY-MM-DD][]minuteBars.
func fetchPolygon1MinForDates(ticker string, dates []string) (map[string][]polygonBar, error) {
	out := make(map[string][]polygonBar, len(dates))
	for i, d := range dates {
		url := fmt.Sprintf(
			"https://api.polygon.io/v2/aggs/ticker/%s/range/1/minute/%s/%s?adjusted=false&sort=asc&limit=50000&apiKey=%s",
			ticker, d, d, polygonAPIKey,
		)
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				// Skip this date if the provider returns an error for that day
				return
			}
			var pr polygonResp
			if err := json.NewDecoder(resp.Body).Decode(&pr); err == nil {
				out[d] = pr.Results
			}
		}()
		// Be nice to the API (mild pacing).
		if (i+1)%5 == 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return out, nil
}

func openBrowser(u string) {
	// Prefer Google Chrome; fall back to xdg-open
	if err := exec.Command("google-chrome", "--new-tab", u).Start(); err != nil {
		_ = exec.Command("chromium-browser", "--new-tab", u).Start()
		_ = exec.Command("xdg-open", u).Start()
	}
}

// ========================= Analysis =========================

// Pass 1: compute daily analytics and return the list of gap sessions we’ll need minute data for.
func analyzeDaily(daily []polygonBar, minGap float64, years int, ticker string) (AnalyzeResponse, []GapPoint) {
	resp := AnalyzeResponse{
		Success: true,
		Ticker:  ticker,
		Years:   years,
		MinGap:  minGap,
	}
	if len(daily) < 2 {
		resp.Success = false
		resp.Error = "not enough data"
		return resp, nil
	}

	bins := defaultBins(minGap)
	type agg struct {
		count, cont, filled int
		sumFade, sumFollow  float64
	}
	binAgg := map[string]*agg{}
	for _, b := range bins {
		binAgg[b.lab] = &agg{}
	}
	upAgg := agg{}
	downAgg := agg{}
	dowAgg := map[string]*agg{"Mon": {}, "Tue": {}, "Wed": {}, "Thu": {}, "Fri": {}}

	points := make([]GapPoint, 0, len(daily)-1)

	var fadeSum, followSum float64
	var contCount int
	var upCount, downCount int
	var meanAbsGap float64
	var maxGapUp, maxGapDown float64
	var cumFade, cumFollow float64
	var cumDates []string
	var cumFadeArr, cumFollowArr []float64

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
		filled := 0
		if (dir == 1 && day.L <= prevClose) || (dir == -1 && day.H >= prevClose) {
			filled = 1
		}

		absGap := math.Abs(gapPct)
		bin := labelFor(absGap, bins)
		dow := sessionWeekdayNYFromDaily(day.T)

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

		sessDate := sessionDateNYFromDaily(day.T)
		cumDates = append(cumDates, sessDate)
		cumFollow += followRet
		cumFade += fadeRet
		cumFollowArr = append(cumFollowArr, round3(cumFollow))
		cumFadeArr = append(cumFadeArr, round3(cumFade))

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

		points = append(points, GapPoint{
			Date:           sessDate,
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
		})
	}

	// Fill response (daily portion)
	total := len(points)
	var contRate, fadeAvg, followAvg float64
	if total > 0 {
		contRate = float64(contCount) / float64(total) * 100.0
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

	resp.Data = points
	resp.CumDates = cumDates
	resp.CumFade = cumFadeArr
	resp.CumFollow = cumFollowArr
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

	// Bins (daily)
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

	return resp, points
}

// Pass 2: compute 0–15m analytics from 1-minute bars for the selected gap dates.
func analyzeFirst15(resp *AnalyzeResponse, minutesByDate map[string][]polygonBar) {
	if resp == nil {
		return
	}
	pts := resp.Data
	if len(pts) == 0 {
		return
	}

	bins := defaultBins(resp.MinGap)
	type agg15 struct {
		count, cont, filledBy0945 int
		sumFade, sumFollow        float64
	}
	binAgg15 := map[string]*agg15{}
	for _, b := range bins {
		binAgg15[b.lab] = &agg15{}
	}
	upAgg15 := agg15{}
	downAgg15 := agg15{}
	dowAgg15 := map[string]*agg15{"Mon": {}, "Tue": {}, "Wed": {}, "Thu": {}, "Fri": {}}

	var fadeSum15, followSum15 float64
	var contCount15, filledBy0945Count, sessions15 int

	for i := range pts {
		p := &pts[i]
		mins := minutesByDate[p.Date]
		if len(mins) == 0 {
			// No intraday data for this date — leave 0–15m empty for this point
			continue
		}

		// Filter to RTH first 15 minutes: 09:30..09:44 (NY)
		rth := make([]polygonBar, 0, 16)
		for _, b := range mins {
			ny := toNY(time.UnixMilli(b.T))
			if ny.Hour() == 9 && ny.Minute() >= 30 && ny.Minute() <= 44 {
				rth = append(rth, b)
			}
		}
		if len(rth) == 0 {
			// Fallback: if provider stamps differently, try using the last minute whose time <= 09:45
			for _, b := range mins {
				ny := toNY(time.UnixMilli(b.T))
				if ny.Hour() == 9 && ny.Minute() <= 45 {
					rth = append(rth, b)
				}
			}
		}
		if len(rth) == 0 {
			continue
		}

		// 09:30 open (fallback to daily open if the 09:30 minute is missing)
		var open0930 float64
		for _, b := range rth {
			ny := toNY(time.UnixMilli(b.T))
			if ny.Minute() == 30 {
				open0930 = b.O
				break
			}
		}
		if open0930 == 0 {
			open0930 = p.Open // fallback to daily open
		}
		if open0930 <= 0 {
			continue
		}

		// 09:45 close ≈ close of the last minute before 09:45 (typically the 09:44 bar).
		close0945 := rth[len(rth)-1].C

		// Gap-fill by 09:45 within the rth slice
		filled0945 := 0
		if p.Direction == 1 {
			for _, b := range rth {
				if b.L <= p.PrevClose {
					filled0945 = 1
					break
				}
			}
		} else if p.Direction == -1 {
			for _, b := range rth {
				if b.H >= p.PrevClose {
					filled0945 = 1
					break
				}
			}
		}

		ret15 := (close0945 - open0930) / open0930 * 100.0
		cont15 := 0
		if sign(ret15) == p.Direction && p.Direction != 0 && ret15 != 0 {
			cont15 = 1
		}

		followRet15 := float64(p.Direction) * ret15
		fadeRet15 := -float64(p.Direction) * ret15

		followSum15 += followRet15
		fadeSum15 += fadeRet15
		contCount15 += cont15
		filledBy0945Count += filled0945
		sessions15++

		// Update per-bin / side / DOW aggregates
		ba := binAgg15[p.Bin]
		if ba == nil {
			ba = &agg15{}
			binAgg15[p.Bin] = ba
		}
		ba.count++
		ba.sumFollow += followRet15
		ba.sumFade += fadeRet15
		if cont15 == 1 {
			ba.cont++
		}
		if filled0945 == 1 {
			ba.filledBy0945++
		}

		if p.Direction == 1 {
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
		da := dowAgg15[p.DayOfWeek]
		if da == nil {
			da = &agg15{}
			dowAgg15[p.DayOfWeek] = da
		}
		da.count++
		da.sumFollow += followRet15
		da.sumFade += fadeRet15
		if cont15 == 1 {
			da.cont++
		}
		if filled0945 == 1 {
			da.filledBy0945++
		}

		// Write back per‑point snapshot
		p.Ret15mPct = round3(ret15)
		p.FilledBy0945 = filled0945
	}

	// Summaries
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

	// write back updated points
	resp.Data = pts
}

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

	// Step 1: daily analytics
	daily, err := fetchPolygonDaily(ticker, from, to)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	resp, points := analyzeDaily(daily, minGap, years, ticker)

	// Collect the specific session dates that passed the daily filter
	dates := make([]string, 0, len(points))
	seen := map[string]bool{}
	for _, p := range points {
		if !seen[p.Date] {
			seen[p.Date] = true
			dates = append(dates, p.Date)
		}
	}
	sort.Strings(dates)

	// Step 2: fetch 1m bars only for those dates
	minutesByDate, err := fetchPolygon1MinForDates(ticker, dates)
	if err != nil {
		// Don’t fail the entire request; return daily results with a clear error message
		resp.Success = false
		resp.Error = "intraday fetch failed: " + err.Error()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Step 3: compute 0–15m analytics from those 1m bars
	analyzeFirst15(&resp, minutesByDate)

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
