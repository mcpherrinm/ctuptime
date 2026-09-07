package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func hours(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }

// mk builds a series with one sample per hour from values.
func mk(log, endpoint string, values ...float64) *series {
	samples := make([]sample, len(values))
	for i, v := range values {
		samples[i] = sample{t: hours(float64(i)), v: v}
	}
	return newSeries(log, endpoint, samples)
}

// rep returns v repeated n times.
func rep(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func cat(parts ...[]float64) []float64 {
	var out []float64
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// replay evaluates every hour from 0 through the last sample of the longest series.
func replay(st *state, all ...*series) {
	n := 0
	for _, s := range all {
		n = max(n, len(s.samples))
	}
	for i := 0; i < n; i++ {
		st.evaluate(all, hours(float64(i)))
	}
}

func newState() *state { return &state{Reported: make(map[string]time.Time)} }

func TestRuns(t *testing.T) {
	s := mk("https://a.example.com/", "get-sth", 100, 80, 70, 95, 100, 50, 89.9, 100)
	if len(s.runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(s.runs))
	}
	if r := s.runs[0]; r.first != 1 || r.last != 2 || r.prefixMin[1] != 70 {
		t.Errorf("run 0 = %+v", r)
	}
	if r := s.runs[1]; r.first != 5 || r.last != 6 || r.prefixMin[0] != 50 || r.prefixMin[1] != 50 {
		t.Errorf("run 1 = %+v", r)
	}
	if _, ok := s.at(hours(0)); ok {
		t.Error("healthy sample reported as a condition")
	}
	if _, ok := s.at(hours(1.5)); ok {
		t.Error("time with no sample reported as a condition")
	}
	c, ok := s.at(hours(2))
	if !ok || c.min != 70 || !c.start.Equal(hours(1)) || c.triggered {
		t.Errorf("at(2) = %+v, %v", c, ok)
	}
}

func TestSustainedTrigger(t *testing.T) {
	s := mk("https://a.example.com/", "get-sth", rep(85, 30)...)
	st := newState()
	for i := 0; i < 24; i++ {
		st.evaluate([]*series{s}, hours(float64(i)))
		if len(st.Events) != 0 {
			t.Fatalf("event after %dh, want none before 24h", i)
		}
	}
	st.evaluate([]*series{s}, hours(24))
	if len(st.Events) != 1 {
		t.Fatalf("got %d events at 24h, want 1", len(st.Events))
	}
	e := st.Events[0]
	if e.Operator != "example.com" || !e.Published.Equal(hours(24)) || !e.Since.Equal(hours(0)) {
		t.Errorf("event = %+v", e)
	}
	if got := e.Logs["https://a.example.com/"]["get-sth"]; got != 85 {
		t.Errorf("min = %v, want 85", got)
	}
	if e.ID != "tag:mcpherrinm.github.io,2026-08-02:ctuptime/example.com/1785628800" {
		t.Errorf("id = %q", e.ID)
	}
}

func TestRepeatTrigger(t *testing.T) {
	// Down at 0-1, healthy, down again starting at 10: two drops within 24h.
	s := mk("https://a.example.com/", "get-sth", cat(rep(60, 2), rep(100, 8), rep(70, 2), rep(100, 5))...)
	st := newState()
	replay(st, s)
	if len(st.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(st.Events))
	}
	e := st.Events[0]
	if !e.Published.Equal(hours(10)) || !e.Since.Equal(hours(10)) {
		t.Errorf("published %v since %v, want both at 10h", e.Published, e.Since)
	}
	if got := e.Logs["https://a.example.com/"]["get-sth"]; got != 70 {
		t.Errorf("min = %v, want 70 (the current run only)", got)
	}

	// Two drops 30h apart do not count.
	s = mk("https://a.example.com/", "get-sth", cat(rep(60, 2), rep(100, 28), rep(70, 2), rep(100, 5))...)
	st = newState()
	replay(st, s)
	if len(st.Events) != 0 {
		t.Fatalf("got %d events for drops 30h apart, want 0", len(st.Events))
	}
}

func TestBelowThresholdEndpointsAreIncluded(t *testing.T) {
	long := mk("https://a.example.com/", "get-sth", rep(85, 30)...)
	brief := mk("https://b.example.com/", "add-chain", cat(rep(100, 24), rep(40, 1), rep(100, 5))...)
	fine := mk("https://c.example.com/", "get-roots", rep(95, 30)...)
	st := newState()
	replay(st, long, brief, fine)
	if len(st.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(st.Events))
	}
	e := st.Events[0]
	if len(e.Logs) != 2 || e.Logs["https://b.example.com/"]["add-chain"] != 40 {
		t.Errorf("logs = %v, want both a and b", e.Logs)
	}
	if e.lowest() != 40 {
		t.Errorf("lowest = %v", e.lowest())
	}
}

func TestCooldownFoldsIntoOpenEvent(t *testing.T) {
	a := mk("https://a.example.com/", "get-sth", cat(rep(85, 30), rep(100, 24*10))...)
	// b drops 3 days in and keeps dropping lower.
	b := mk("https://b.example.com/", "get-sth", cat(rep(100, 72), rep(80, 24), rep(20, 3), rep(100, 24*7))...)
	st := newState()
	replay(st, a, b)
	if len(st.Events) != 1 {
		t.Fatalf("got %d events, want 1 (b folded into a's)", len(st.Events))
	}
	e := st.Events[0]
	if !e.Published.Equal(hours(24)) {
		t.Errorf("published = %v, want unchanged 24h", e.Published)
	}
	if got := e.Logs["https://b.example.com/"]["get-sth"]; got != 20 {
		t.Errorf("b min = %v, want 20 (refreshed while run continues)", got)
	}
}

func TestNewEventAfterCooldown(t *testing.T) {
	a := mk("https://a.example.com/", "get-sth", cat(rep(85, 30), rep(100, 24*10))...)
	b := mk("https://b.example.com/", "get-sth", cat(rep(100, 24*8), rep(80, 25), rep(100, 24))...)
	st := newState()
	replay(st, a, b)
	if len(st.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(st.Events))
	}
	if _, ok := st.Events[1].Logs["https://a.example.com/"]; ok {
		t.Error("second event includes a, whose run ended long before")
	}
}

func TestRunReportedOnce(t *testing.T) {
	// A single three-week outage produces one event, not one per week.
	a := mk("https://a.example.com/", "get-sth", rep(0, 24*21)...)
	st := newState()
	replay(st, a)
	if len(st.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(st.Events))
	}
}

func TestDifferentOperatorsAreIndependent(t *testing.T) {
	a := mk("https://a.example.com/", "get-sth", rep(85, 30)...)
	b := mk("https://log.other.org/2026h1/", "get-sth", rep(85, 30)...)
	st := newState()
	replay(st, a, b)
	if len(st.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(st.Events))
	}
	if st.Events[0].Operator != "example.com" || st.Events[1].Operator != "other.org" {
		t.Errorf("operators = %s, %s", st.Events[0].Operator, st.Events[1].Operator)
	}
}

func TestPrune(t *testing.T) {
	a := mk("https://a.example.com/", "get-sth", rep(85, 30)...)
	st := newState()
	replay(st, a)
	st.evaluate([]*series{a}, hours(24*92))
	if len(st.Events) != 0 || len(st.Reported) != 0 {
		t.Errorf("state not pruned: %d events, %d reported", len(st.Events), len(st.Reported))
	}
}

func TestOperatorFor(t *testing.T) {
	cases := map[string]string{
		"https://ct.googleapis.com/logs/eu1/xenon2026h2/":                     "googleapis.com",
		"https://coachandhorses2026h2.staging.certificate.transparency.goog/": "transparency.goog",
		"https://gouda2026h1.log.ct.ipng.ch/":                                 "ipng.ch",
		"not a url":                                                           "(unknown)",
	}
	for in, want := range cases {
		if got := operatorFor(in); got != want {
			t.Errorf("operatorFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFeedRendering(t *testing.T) {
	st := newState()
	st.Events = []event{{
		ID:        "tag:mcpherrinm.github.io,2026-08-05:ctuptime/sectigo.com/1754386080",
		Operator:  "sectigo.com",
		Published: time.Date(2026, 8, 5, 11, 28, 0, 0, time.UTC),
		Since:     time.Date(2026, 8, 4, 11, 28, 0, 0, time.UTC),
		Logs: map[string]map[string]float64{
			"https://elephant2026h2.ct.sectigo.com/": {"add-chain": 8.3333, "get-entries": 52.8571, "add-pre-chain": 16.6667},
			"https://tiger2026h2.ct.sectigo.com/":    {"get-sth-consistency": 66.6667},
		},
	}}
	feed := renderFeed(st, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if feed.Updated != "2026-08-05T11:28:00Z" {
		t.Errorf("feed updated = %s", feed.Updated)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("got %d entries", len(feed.Entries))
	}
	e := feed.Entries[0]
	if want := "sectigo.com: 2 logs below 90.00% uptime, lowest 8.33%"; e.Title != want {
		t.Errorf("title = %q, want %q", e.Title, want)
	}
	wantSummary := "Since 2026-08-04 11:28 UTC:\n" +
		"elephant2026h2.ct.sectigo.com: add-chain 8.33%, add-pre-chain 16.67%, get-entries 52.86%\n" +
		"tiger2026h2.ct.sectigo.com: get-sth-consistency 66.67%"
	if e.Summary.Body != wantSummary {
		t.Errorf("summary =\n%s\nwant\n%s", e.Summary.Body, wantSummary)
	}
	if !strings.HasPrefix(e.Content.Body, "<p>Since 2026-08-04 11:28 UTC:</p>\n<p>elephant2026h2") {
		t.Errorf("content = %q", e.Content.Body)
	}
	if e.Published != e.Updated || e.Published != "2026-08-05T11:28:00Z" {
		t.Errorf("published %s updated %s", e.Published, e.Updated)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "feed.xml")
	if err := writeFeed(file, st, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var back atomFeed
	if err := xml.Unmarshal(b, &back); err != nil {
		t.Fatalf("feed does not round-trip: %v", err)
	}
	if back.Entries[0].Summary.Body != wantSummary || back.Xmlns != "http://www.w3.org/2005/Atom" {
		t.Errorf("round-tripped feed differs: %+v", back)
	}
}

func TestUpdateEventsReplaysAndPersists(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("timestamp,add-chain,get-sth\n")
	for i := 0; i < 48; i++ {
		v := "100.0000"
		if i >= 10 {
			v = "42.5000"
		}
		sb.WriteString(strings.Join([]string{strconv.FormatInt(hours(float64(i)).Unix(), 10), v, "100.0000"}, ",") + "\n")
	}
	csv := filepath.Join(data, "https:%2F%2Fa.example.com%2F.csv")
	if err := os.WriteFile(csv, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, "events.json")
	feedFile := filepath.Join(dir, "feed.xml")

	// First run: replays all history within retention.
	if err := updateEvents(data, stateFile, feedFile, hours(47)); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Events) != 1 || !st.Events[0].Published.Equal(hours(34)) || !st.Evaluated.Equal(hours(47)) {
		t.Fatalf("state after first run: %+v", st)
	}
	if st.Events[0].Logs["https://a.example.com/"]["add-chain"] != 42.5 {
		t.Errorf("logs = %v", st.Events[0].Logs)
	}

	// Second run with no new data is a no-op.
	if err := updateEvents(data, stateFile, feedFile, hours(47)); err != nil {
		t.Fatal(err)
	}
	st2, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Events) != 1 || len(st2.Reported) != 1 {
		t.Errorf("state after no-op run: %+v", st2)
	}
	b, err := os.ReadFile(feedFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<title>example.com: 1 log below 90.00% uptime, lowest 42.50%</title>") {
		t.Errorf("feed:\n%s", b)
	}
}
