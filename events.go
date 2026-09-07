package main

import (
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Incident event detection.
//
// Google's CSV reports each endpoint's uptime over the trailing 24 hours. An
// endpoint is "down" while that number is below uptimeThreshold. Consecutive
// down samples form a run. A run triggers an event when it has lasted
// sustainedDuration, or when it began within repeatWindow of the previous run
// for the same endpoint. Triggers are grouped per operator, and an operator
// gets at most one event per eventCooldown: later triggers fold into the open
// event, updating its endpoint list and lowest values without changing its
// timestamps. Each run is reported at most once.
const (
	uptimeThreshold   = 90.0
	sustainedDuration = 24 * time.Hour
	repeatWindow      = 24 * time.Hour
	eventCooldown     = 7 * 24 * time.Hour
	retention         = 90 * 24 * time.Hour
)

const (
	dataDir   = "data"
	stateFile = "events.json"
)

type sample struct {
	t time.Time
	v float64
}

// run is a maximal stretch of consecutive samples below the threshold.
type run struct {
	first, last int       // inclusive index range into series.samples
	prefixMin   []float64 // prefixMin[i] is the minimum of samples[first..first+i]
}

type series struct {
	log, endpoint string
	samples       []sample // sorted by time
	runIdx        []int    // per sample: index into runs, or -1
	runs          []run
}

func newSeries(log, endpoint string, samples []sample) *series {
	slices.SortFunc(samples, func(a, b sample) int { return a.t.Compare(b.t) })
	s := &series{log: log, endpoint: endpoint, samples: samples, runIdx: make([]int, len(samples))}
	for i, p := range samples {
		if p.v >= uptimeThreshold {
			s.runIdx[i] = -1
			continue
		}
		if i > 0 && s.runIdx[i-1] >= 0 {
			r := &s.runs[len(s.runs)-1]
			r.last = i
			r.prefixMin = append(r.prefixMin, min(r.prefixMin[len(r.prefixMin)-1], p.v))
		} else {
			s.runs = append(s.runs, run{first: i, last: i, prefixMin: []float64{p.v}})
		}
		s.runIdx[i] = len(s.runs) - 1
	}
	return s
}

// condition describes an endpoint that is below the threshold at some instant.
type condition struct {
	log, endpoint string
	start         time.Time // when the current run began
	min           float64   // lowest uptime seen so far in the run
	triggered     bool
}

// at reports the series' condition at time t, if it has a sample at exactly
// t and that sample is below the threshold. Samples after t are ignored, so
// history can be replayed.
func (s *series) at(t time.Time) (condition, bool) {
	i := sort.Search(len(s.samples), func(i int) bool { return !s.samples[i].t.Before(t) })
	if i == len(s.samples) || !s.samples[i].t.Equal(t) || s.runIdx[i] < 0 {
		return condition{}, false
	}
	r := s.runs[s.runIdx[i]]
	c := condition{
		log:      s.log,
		endpoint: s.endpoint,
		start:    s.samples[r.first].t,
		min:      r.prefixMin[i-r.first],
	}
	if t.Sub(c.start) >= sustainedDuration {
		c.triggered = true
	}
	if ri := s.runIdx[i]; ri > 0 {
		prev := s.samples[s.runs[ri-1].first].t
		if c.start.Sub(prev) <= repeatWindow {
			c.triggered = true
		}
	}
	return c, true
}

func runKey(c condition) string {
	return fmt.Sprintf("%s %s %d", c.log, c.endpoint, c.start.Unix())
}

// operatorFor groups logs the same way index.html does: by the last two
// labels of the log's hostname.
func operatorFor(log string) string {
	u, err := url.Parse(log)
	if err != nil || u.Hostname() == "" {
		return "(unknown)"
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) < 2 {
		return u.Hostname()
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// loadHistory reads every per-log CSV in dir into one series per endpoint.
func loadHistory(dir string) ([]*series, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return nil, err
	}
	var all []*series
	for _, file := range files {
		log, err := url.PathUnescape(strings.TrimSuffix(filepath.Base(file), ".csv"))
		if err != nil {
			return nil, fmt.Errorf("decoding log name from %s: %w", file, err)
		}
		ss, err := loadCSV(file, log)
		if err != nil {
			return nil, err
		}
		all = append(all, ss...)
	}
	slices.SortFunc(all, func(a, b *series) int {
		return cmp.Or(cmp.Compare(a.log, b.log), cmp.Compare(a.endpoint, b.endpoint))
	})
	return all, nil
}

func loadCSV(file, log string) ([]*series, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header of %s: %w", file, err)
	}
	tsIdx := slices.Index(header, timestampHeader)
	if tsIdx < 0 {
		return nil, fmt.Errorf("no %s column in %s", timestampHeader, file)
	}
	samples := make([][]sample, len(header))
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		if tsIdx >= len(row) {
			continue
		}
		ts, err := strconv.ParseInt(row[tsIdx], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing timestamp in %s: %w", file, err)
		}
		for i, cell := range row {
			if i == tsIdx || i >= len(header) || cell == "" {
				continue
			}
			v, err := strconv.ParseFloat(cell, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing %s %s at %d: %w", file, header[i], ts, err)
			}
			samples[i] = append(samples[i], sample{t: time.Unix(ts, 0).UTC(), v: v})
		}
	}
	var out []*series
	for i, h := range header {
		if i == tsIdx {
			continue
		}
		out = append(out, newSeries(log, h, samples[i]))
	}
	return out, nil
}

type event struct {
	ID        string    `json:"id"`
	Operator  string    `json:"operator"`
	Published time.Time `json:"published"`
	Since     time.Time `json:"since"`
	// Logs maps log URL to endpoint to the lowest uptime seen.
	Logs map[string]map[string]float64 `json:"logs"`
}

func (e *event) add(c condition) {
	if e.Logs == nil {
		e.Logs = make(map[string]map[string]float64)
	}
	eps, ok := e.Logs[c.log]
	if !ok {
		eps = make(map[string]float64)
		e.Logs[c.log] = eps
	}
	if cur, ok := eps[c.endpoint]; !ok || c.min < cur {
		eps[c.endpoint] = c.min
	}
	if c.start.Before(e.Since) {
		e.Since = c.start
	}
}

func (e *event) lowest() float64 {
	low := 100.0
	for _, eps := range e.Logs {
		for _, v := range eps {
			low = min(low, v)
		}
	}
	return low
}

type state struct {
	// Evaluated is the last data timestamp the detector has processed.
	Evaluated time.Time `json:"evaluated"`
	// Events are in order of publication, oldest first.
	Events []event `json:"events"`
	// Reported holds the start time of every run already included in an
	// event, keyed by runKey, so a run is never reported twice.
	Reported map[string]time.Time `json:"reported"`
}

func loadState(file string) (*state, error) {
	st := &state{Reported: make(map[string]time.Time)}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	if st.Reported == nil {
		st.Reported = make(map[string]time.Time)
	}
	return st, nil
}

func (st *state) save(file string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0644)
}

// openEvent returns the operator's event still within its cooldown, if any.
func (st *state) openEvent(op string, now time.Time) *event {
	for i := len(st.Events) - 1; i >= 0; i-- {
		e := &st.Events[i]
		if e.Operator == op {
			if now.Sub(e.Published) < eventCooldown {
				return e
			}
			return nil
		}
	}
	return nil
}

func eventID(op string, published time.Time) string {
	return fmt.Sprintf("tag:mcpherrinm.github.io,%s:ctuptime/%s/%d", published.Format("2006-01-02"), op, published.Unix())
}

// evaluate advances the state to the data timestamp now.
func (st *state) evaluate(all []*series, now time.Time) {
	byOp := make(map[string][]condition)
	for _, s := range all {
		if c, ok := s.at(now); ok {
			op := operatorFor(s.log)
			byOp[op] = append(byOp[op], c)
		}
	}
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	slices.Sort(ops)

	for _, op := range ops {
		conds := byOp[op]
		fresh := false
		for _, c := range conds {
			if _, seen := st.Reported[runKey(c)]; c.triggered && !seen {
				fresh = true
				break
			}
		}
		ev := st.openEvent(op, now)
		if ev == nil {
			if !fresh {
				continue
			}
			st.Events = append(st.Events, event{
				ID:        eventID(op, now),
				Operator:  op,
				Published: now,
				Since:     now,
			})
			ev = &st.Events[len(st.Events)-1]
		}
		for _, c := range conds {
			key := runKey(c)
			if _, seen := st.Reported[key]; !seen && !fresh {
				// Nothing new for this operator: only refresh runs already in the event.
				continue
			}
			st.Reported[key] = c.start
			ev.add(c)
		}
	}

	st.Evaluated = now
	st.prune(now)
}

func (st *state) prune(now time.Time) {
	cutoff := now.Add(-retention)
	st.Events = slices.DeleteFunc(st.Events, func(e event) bool { return e.Published.Before(cutoff) })
	for k, start := range st.Reported {
		if start.Before(cutoff) {
			delete(st.Reported, k)
		}
	}
}

// updateEvents replays every data timestamp newer than the saved state (or
// the last retention period, on first run) and rewrites the state and feed.
func updateEvents(dataDir, stateFile, feedFile string, now time.Time) error {
	all, err := loadHistory(dataDir)
	if err != nil {
		return err
	}
	st, err := loadState(stateFile)
	if err != nil {
		return err
	}
	after := st.Evaluated
	if after.IsZero() {
		after = now.Add(-retention)
	}

	seen := make(map[time.Time]bool)
	var times []time.Time
	for _, s := range all {
		for _, p := range s.samples {
			if p.t.After(after) && !p.t.After(now) && !seen[p.t] {
				seen[p.t] = true
				times = append(times, p.t)
			}
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	for _, t := range times {
		st.evaluate(all, t)
	}

	if err := st.save(stateFile); err != nil {
		return fmt.Errorf("saving %s: %w", stateFile, err)
	}
	if err := writeFeed(feedFile, st, now); err != nil {
		return fmt.Errorf("writing %s: %w", feedFile, err)
	}
	return nil
}
