# CT Uptime

This repository generates a static website with data recording CT uptime.

It uses [Google's CT endpoint CSV](https://www.gstatic.com/ct/compliance/endpoint_uptime_24h.csv)
as input.

It produces as outputs:

* A Prometheus-compatible `metrics` file, directly mapping from `endpoint_uptime_24h.csv`
* one CSV file per log, with columns for endpoints, and a row per timestamp
* index.json with a list of the CSV files
* `feed.xml`, an Atom feed of uptime incidents, and `events.json`, its state.
  An endpoint whose 24h uptime stays below 90% for 24 hours, or drops below
  90% twice within 24 hours, opens an event for its operator. An operator gets
  at most one event per week; later drops fold into the open event. Entry
  timestamps never change after publication so feed readers (notably Slack)
  do not repost them.

There's an index.html which renders it with uPlot.
