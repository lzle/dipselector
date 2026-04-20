# dipselector

[![Go Reference](https://pkg.go.dev/badge/github.com/lzle/dipselector.svg)](https://pkg.go.dev/github.com/lzle/dipselector)
[![Go Report Card](https://goreportcard.com/badge/github.com/lzle/dipselector)](https://goreportcard.com/report/github.com/lzle/dipselector)

**dipselector** is a small Go library that picks a concrete IP for a hostname using **DNS**, **EWMA-smoothed throughput**, and **failure-rate penalties**, with periodic refresh and decay so flaky addresses are gradually deprioritized.

Package import path: `github.com/lzle/dipselector` (package name: `selector`).

## Features

- **ChooseIP** — select the best IP for a domain from the current candidate set (triggers DNS resolution when the cache is cold or empty).
- **ReportSample** — record each request’s outcome (success/failure) and observed speed (bytes/sec) for `(domain, ip)` to update internal stats.
- **Background loop** (until `context` cancel) — periodic DNS resync, counter decay, and structured logging via [logrus](https://github.com/sirupsen/logrus).
- **DNS churn handling** — merges resolver results with cached IPs using simple retention rules (see implementation).

## Requirements

- Go **1.23+** (see `go.mod`).

## Installation

```bash
go get github.com/lzle/dipselector
```

## Quick start

```go
package main

import (
	"context"
	"time"

	selector "github.com/lzle/dipselector"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &selector.IPConfig{
		DNSRefresh:    5 * time.Minute,
		DecayInterval: 3 * time.Minute,
		DNSTimeout:    10 * time.Second,
	}
	sel := selector.NewSelector(ctx, cfg)

	domain := "example.com"
	ip, err := sel.ChooseIP(domain)
	if err != nil {
		panic(err)
	}

	requestOK := false // set from your HTTP/TCP client (timeout, reset, 5xx, etc.)

	if requestOK {
		bytesPerSec := 1_048_576.0 // measure from your transfer
		if err := sel.ReportSample(domain, ip, bytesPerSec, true); err != nil {
			panic(err)
		}
	} else {
		// Failure: still report so fail rate affects future ChooseIP scores.
		// Speed is ignored when success is false (EWMA uses 0 for this sample).
		if err := sel.ReportSample(domain, ip, 0, false); err != nil {
			panic(err)
		}
	}
}
```

> **Note:** Call `ChooseIP` (or otherwise ensure the domain has been resolved) before `ReportSample`, or you will get a “domain/IP not found” error.

## How selection works

For each IP, an internal **score** is computed (higher is better):

`score = speedEWMA - FailRatioFactor * failRate`

`failRate` is `failCount / max(successCount + failCount, 1)`. `ChooseIP` returns the IP with the largest score.

## Configuration (`IPConfig`)

| Field | Role |
|--------|------|
| `DNSRefresh` | Interval for background DNS refresh of known domains. |
| `DecayInterval` | Interval for decaying success/fail counters and optional idle boost. |
| `DNSTimeout` | Timeout for `LookupHost` during resolution. |
| `Alpha` | EWMA smoothing factor for observed speed (`0 < Alpha < 1`). |
| `FailRatioFactor` | Weight of failure rate in the score (larger ⇒ stronger penalty). |
| `ChanceTime` | If an IP has not been updated for longer than this, decay may slightly increase its `speedEWMA` (exploration). |
| `InitMaxSpeedEWMA` / `MinSpeedEWMA` | Clamp range for the speed EWMA. |

If `cfg` is `nil`, `NewSelector` uses built-in defaults (see `NewSelector` in `selector.go`).

## Lifecycle

Pass a **`context.Context`** that you cancel when the process or subsystem shuts down. Canceling the context stops the background tickers created by `NewSelector`.

## Logging

The library logs through **logrus** (resolution, merges, periodic metric dumps). Configure logrus in your application as usual (level, formatter, output).

## Testing

```bash
go test -race ./...
```

## Contributing

Issues and pull requests are welcome. Please run tests with `-race` before submitting changes.

## License

Specify a license in a `LICENSE` file in this repository (this tree does not ship one yet).
