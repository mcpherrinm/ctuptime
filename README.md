# CT Uptime

This repository generates a static website with data recording CT uptime.

It uses [Google's CT endpoint CSV](https://www.gstatic.com/ct/compliance/endpoint_uptime_24h.csv)
as input.

It produces as outputs:

* A Prometheus-compatible `metrics` file, directly mapping from `endpoint_uptime_24h.csv`
* one CSV file per log, with columns for endpoints, and a row per timestamp
* index.json with a list of the CSV files

There's an index.html which renders it with uPlot.
