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
	T int64   `json:"t"` // ms
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
	Date            string  `json:"date"`               // YYYY-MM-DD (NY)
	GapPct          float64 `json:"gap_pct"`            // (open-prevClose)/prevClose * 100
	DailyReturnPct  float64 `json:"daily_return_pct"`   // (close-open)/open * 100
	Direction       int     `json:"direction"`          // 1 gap-up, -1 gap-down
	SameDir         int     `json:"same_dir"`           // 1 continuation, 0 reversal/flat
	Filled          int     `json:"filled"`             // 1 gap filled intraday, else 0
	Bin             string  `json:"bin"`                // gap bin label
	Open            float64 `json:"open,omitempty"`
	Close           float64 `json:"close,omitempty"`
	PrevClose       float64 `json:"prev_close,omitempty"`
	DayOfWeek       string  `json:"dow,omitempty"`      // Mon..Fri
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

type AnalyzeResponse struct {
	Success bool       `json:"success"`
	Error   string     `json:"error,omitempty"`
	Ticker  string     `json:"ticker"`
	Years   int        `json:"years"`
	MinGap  float64    `json:"min_gap"`
	Data    []GapPoint `json:"data"`

	Summary Summary            `json:"summary"`
	Bins    []BinStat          `json:"bins"`
	ByDOW   map[string]DowStat `json:"by_dow"`
	UpSide  SideStat           `json:"gap_up"`
	DownSide SideStat          `json:"gap_down"`

	CumDates  []string  `json:"cum_dates"`
	CumFade   []float64 `json:"cum_fade"`
	CumFollow []float64 `json:"cum_follow"`
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

func fetchPolygonDaily(ticker, from, to string) ([]polygonBar, error) {
        // If you want the most literal gap (prior close print → open print), change adjusted=true to adjusted=false
        //url := fmt.Sprintf("https://api.polygon.io/v2/aggs/ticker/%s/range/1/day/%s/%s?adjusted=true&sort=asc&apiKey=%s",
	url := fmt.Sprintf("https://api.polygon.io/v2/aggs/ticker/%s/range/1/day/%s/%s?adjusted=false&sort=asc&apiKey=%s",
		ticker, from, to, polygonAPIKey)
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
	// Dynamic first edge from UI minGap; rest like spec (0.3‑0.5, 0.5‑1, 1‑1.5, >1.5)
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

func analyzeGaps(bars []polygonBar, minGap float64, years int, ticker string) AnalyzeResponse {
	resp := AnalyzeResponse{
		Success: true,
		Ticker:  ticker,
		Years:   years,
		MinGap:  minGap,
	}
	if len(bars) < 2 {
		resp.Success = false
		resp.Error = "not enough data"
		return resp
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
	dowAgg := map[string]*agg{
		"Mon": {}, "Tue": {}, "Wed": {}, "Thu": {}, "Fri": {},
	}

	// Time-sorted daily bars (Polygon asc)
	points := make([]GapPoint, 0, len(bars)-1)

	var fadeSum, followSum float64
	var contCount int
	var upCount, downCount int
	var meanAbsGap float64
	var maxGapUp, maxGapDown float64
	var cumFade, cumFollow float64
	var cumDates []string
	var cumFadeArr, cumFollowArr []float64

	for i := 1; i < len(bars); i++ {
		prev := bars[i-1]
		day := bars[i]

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
		// gap fill: for gap-up, low <= prevClose; for gap-down, high >= prevClose
		filled := 0
		if (dir == 1 && day.L <= prevClose) || (dir == -1 && day.H >= prevClose) {
			filled = 1
		}

		absGap := math.Abs(gapPct)
		bin := labelFor(absGap, bins)
		dow := weekdayNY(day.T)

		// Strategy returns per trade
		followRet := float64(dir) * dr
		fadeRet := -float64(dir) * dr

		// accumulate
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

		// bin & side & DOW aggregates
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
			Date:           dateNY(day.T),
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

	// Build response pieces
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

	// Bins
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
		// Simple thresholds like brief: >60 Follow, <40 Fade
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
	// Stable order
	sort.Slice(outBins, func(i, j int) bool { return i < j })
	resp.Bins = outBins

	// Side splits
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

	// Day-of-week splits
	resp.ByDOW = map[string]DowStat{}
	for k, v := range dowAgg {
		resp.ByDOW[k] = DowStat{
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

	bars, err := fetchPolygonDaily(ticker, from, to)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	resp := analyzeGaps(bars, minGap, years, ticker)
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
