package main

import (
	"cmp"
	"encoding/xml"
	"fmt"
	"html"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	siteURL  = "https://mcpherrinm.github.io/ctuptime/"
	feedFile = "feed.xml"
)

// The feed is written for Slack's RSS app as much as for real readers: it
// bookmarks by date and reposts anything with a newer one, so an entry's
// timestamps never change after publication; it shows the title as a link
// plus the description with HTML stripped, so the title carries the key facts
// and the body is a few short plain lines.

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Xmlns   string      `xml:"xmlns,attr"`
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Author  atomAuthor  `xml:"author"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
	Type string `xml:"type,attr,omitempty"`
}

type atomEntry struct {
	ID        string   `xml:"id"`
	Title     string   `xml:"title"`
	Link      atomLink `xml:"link"`
	Published string   `xml:"published"`
	Updated   string   `xml:"updated"`
	Summary   atomText `xml:"summary"`
	Content   atomText `xml:"content"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

func formatPct(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64) + "%"
}

// logLabel strips the scheme and trailing slash from a log URL.
func logLabel(log string) string {
	log = strings.TrimPrefix(log, "https://")
	log = strings.TrimPrefix(log, "http://")
	return strings.TrimSuffix(log, "/")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func entryTitle(e *event) string {
	return fmt.Sprintf("%s: %s below %s uptime, lowest %s",
		e.Operator, plural(len(e.Logs), "log"), formatPct(uptimeThreshold), formatPct(e.lowest()))
}

// entryLines is the entry body, one log per line, worst endpoints first.
func entryLines(e *event) []string {
	lines := []string{fmt.Sprintf("Since %s:", e.Since.UTC().Format("2006-01-02 15:04 MST"))}
	logs := make([]string, 0, len(e.Logs))
	for log := range e.Logs {
		logs = append(logs, log)
	}
	slices.Sort(logs)
	for _, log := range logs {
		type ep struct {
			name string
			min  float64
		}
		eps := make([]ep, 0, len(e.Logs[log]))
		for name, v := range e.Logs[log] {
			eps = append(eps, ep{name, v})
		}
		slices.SortFunc(eps, func(a, b ep) int {
			return cmp.Or(cmp.Compare(a.min, b.min), cmp.Compare(a.name, b.name))
		})
		parts := make([]string, len(eps))
		for i, p := range eps {
			parts[i] = p.name + " " + formatPct(p.min)
		}
		lines = append(lines, logLabel(log)+": "+strings.Join(parts, ", "))
	}
	return lines
}

func renderFeed(st *state, now time.Time) atomFeed {
	feed := atomFeed{
		Xmlns:   "http://www.w3.org/2005/Atom",
		Title:   "CT Uptime incidents",
		ID:      siteURL + feedFile,
		Updated: now.UTC().Format(time.RFC3339),
		Author:  atomAuthor{Name: "CT Uptime"},
		Links: []atomLink{
			{Href: siteURL + feedFile, Rel: "self", Type: "application/atom+xml"},
			{Href: siteURL, Rel: "alternate", Type: "text/html"},
		},
	}
	events := slices.Clone(st.Events)
	slices.SortFunc(events, func(a, b event) int { return b.Published.Compare(a.Published) })
	if len(events) > 0 {
		feed.Updated = events[0].Published.UTC().Format(time.RFC3339)
	}
	for i := range events {
		e := &events[i]
		lines := entryLines(e)
		htmlLines := make([]string, len(lines))
		for i, l := range lines {
			htmlLines[i] = "<p>" + html.EscapeString(l) + "</p>"
		}
		ts := e.Published.UTC().Format(time.RFC3339)
		feed.Entries = append(feed.Entries, atomEntry{
			ID:        e.ID,
			Title:     entryTitle(e),
			Link:      atomLink{Href: siteURL, Rel: "alternate", Type: "text/html"},
			Published: ts,
			Updated:   ts,
			Summary:   atomText{Type: "text", Body: strings.Join(lines, "\n")},
			Content:   atomText{Type: "html", Body: strings.Join(htmlLines, "\n")},
		})
	}
	return feed
}

func writeFeed(file string, st *state, now time.Time) error {
	b, err := xml.MarshalIndent(renderFeed(st, now), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append([]byte(xml.Header), append(b, '\n')...), 0644)
}
