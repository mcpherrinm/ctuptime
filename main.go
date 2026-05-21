package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

const googleCSV = "https://www.gstatic.com/ct/compliance/endpoint_uptime_24h.csv"
const LastModHeader = "Last-Modified"

type point struct {
	endpoint string
	uptime   string
}

func load(ctx context.Context) (map[string][]point, time.Time, error) {
	c := http.Client{}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleCSV, nil)
	if err != nil {
		return nil, time.Time{}, err
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	lastModified, err := http.ParseTime(resp.Header.Get(LastModHeader))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing %s: %w", LastModHeader, err)
	}

	points := make(map[string][]point)

	reader := csv.NewReader(resp.Body)

	header, err := reader.Read()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing CSV header: %w", err)
	}
	if header[0] != "LOG_URL" || header[1] != "ENDPOINT" || header[2] != "UPTIME" {
		return nil, time.Time{}, fmt.Errorf("invalid CSV header: %q", header)
	}

	for {
		row, err := reader.Read()

		if err == io.EOF {
			return points, lastModified, nil
		}

		if err != nil {
			return nil, time.Time{}, err
		}

		if len(row) != 3 {
			return nil, time.Time{}, fmt.Errorf("unexpected row length %d", len(row))
		}

		log := row[0]
		points[log] = append(points[log], point{
			endpoint: row[1],
			uptime:   row[2],
		})
	}
}

const timestampHeader = "timestamp"

func write(log string, points []point, t time.Time) error {
	file := filepath.Join("data", url.PathEscape(log)+".csv")

	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	var headers []string
	if info.Size() == 0 {
		headers = make([]string, 0, len(points)+1)
		headers = append(headers, timestampHeader)
		for _, p := range points {
			headers = append(headers, p.endpoint)
		}
		w := csv.NewWriter(f)
		if err := w.Write(headers); err != nil {
			return fmt.Errorf("writing header to %s: %w", file, err)
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return fmt.Errorf("flushing header to %s: %w", file, err)
		}
	} else {
		r := csv.NewReader(f)
		var lastRow []string
		for {
			row, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading %s: %w", file, err)
			}
			if headers == nil {
				headers = row
			} else {
				lastRow = row
			}
		}

		if lastRow != nil {
			tsIdx := -1
			for i, h := range headers {
				if h == timestampHeader {
					tsIdx = i
					break
				}
			}
			if tsIdx == -1 {
				return fmt.Errorf("no %s column in %s", timestampHeader, file)
			}
			tsSec, err := strconv.ParseInt(lastRow[tsIdx], 10, 64)
			if err != nil {
				return fmt.Errorf("parsing timestamp from %s: %w", file, err)
			}
			if t.Sub(time.Unix(tsSec, 0)) < time.Minute {
				return nil
			}
		}
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	uptimes := make(map[string]string, len(points))
	for _, p := range points {
		uptimes[p.endpoint] = p.uptime
	}

	row := make([]string, len(headers))
	for i, h := range headers {
		if h == timestampHeader {
			row[i] = strconv.FormatInt(t.Unix(), 10)
			continue
		}
		if u, ok := uptimes[h]; ok {
			row[i] = u
		}
	}

	w := csv.NewWriter(f)
	if err := w.Write(row); err != nil {
		return fmt.Errorf("writing row to %s: %w", file, err)
	}
	w.Flush()
	return w.Error()
}

func writeIndex(data map[string][]point, t time.Time) error {
	f, err := os.OpenFile("index.json", os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	idx := make(map[string]int64)
	if err := json.NewDecoder(f).Decode(&idx); err != nil {
		return err
	}

	for log := range data {
		idx[filepath.Join("data", url.PathEscape(log)+".csv")] = t.Unix()
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(idx)
}

func writeMetrics(data map[string][]point, t time.Time) error {
	f, err := os.Create("metrics")
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, `# HELP ct_uptime_timestamp Unix timestamp of the last CT endpoint uptime data update.
# TYPE ct_uptime_timestamp gauge
ct_uptime_timestamp %d
# HELP ct_uptime CT log endpoint uptime over the past 24 hours.
# TYPE ct_uptime gauge
`, t.Unix())
	logs := make([]string, 0, len(data))
	for log := range data {
		logs = append(logs, log)
	}
	slices.Sort(logs)
	for _, log := range logs {
		points := slices.Clone(data[log])
		slices.SortFunc(points, func(a, b point) int {
			return cmp.Compare(a.endpoint, b.endpoint)
		})
		for _, p := range points {
			fmt.Fprintf(f, "ct_uptime{log=%q,endpoint=%q} %s\n", log, p.endpoint, p.uptime)
		}
	}

	return f.Close()
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	data, lastMod, err := load(ctx)
	if err != nil {
		slog.Error("failed fetching", slog.String("error", err.Error()))
		return
	}

	slog.Info("loaded data", slog.Time("last_modified", lastMod), slog.Int("logs", len(data)))

	failed := false
	for log, points := range data {
		err = write(log, points, lastMod)
		if err != nil {
			slog.Error("failed writing", slog.String("error", err.Error()))
			failed = true
		}
	}
	if err := writeIndex(data, lastMod); err != nil {
		slog.Error("failed writing index", slog.String("error", err.Error()))
		failed = true
	}
	if err := writeMetrics(data, lastMod); err != nil {
		slog.Error("failed writing metrics", slog.String("error", err.Error()))
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}
