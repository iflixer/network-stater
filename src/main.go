// netload-reporter.go
// sends network usage to http
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const defaultProcNetDev = "/proc/net/dev"

type Payload struct {
	Host             string  `json:"host"`
	NodeName         string  `json:"node_name,omitempty"`
	Timestamp        string  `json:"timestamp"`
	IntervalSeconds  float64 `json:"interval_seconds"`
	RxBytesPerSec    float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec    float64 `json:"tx_bytes_per_sec"`
	RxBitsPerSec     float64 `json:"rx_bits_per_sec"`
	TxBitsPerSec     float64 `json:"tx_bits_per_sec"`
	TotalBytesPerSec float64 `json:"total_bytes_per_sec"`
	TotalBitsPerSec  float64 `json:"total_bits_per_sec"`
}

type counters struct{ rx, tx uint64 }

func procNetDevPath() string {
	if p := os.Getenv("PROC_NET_DEV"); p != "" {
		return p
	}
	return defaultProcNetDev
}

func readTotals() (c counters, err error) {
	f, err := os.Open(procNetDevPath())
	if err != nil {
		return c, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for lineNum := 0; sc.Scan(); lineNum++ {
		if lineNum < 2 {
			continue
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			return c, fmt.Errorf("unexpected format for %s", iface)
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			return c, fmt.Errorf("parse counters failed for %s", iface)
		}
		c.rx += rx
		c.tx += tx
	}
	return c, sc.Err()
}

func main() {
	err := godotenv.Load("../.env")
	if err != nil {
		log.Println("No .env file found")
	}

	reportURL := os.Getenv("REPORT_URL")
	if reportURL == "" {
		fmt.Fprintln(os.Stderr, "REPORT_URL is required")
		os.Exit(1)
	}
	apiKey := os.Getenv("API_KEY")
	nodeName := os.Getenv("NODE_NAME")

	interval := time.Minute
	if v := os.Getenv("INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}

	host, _ := os.Hostname()
	host = filepath.Base(host)

	client := &http.Client{Timeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	prev, err := readTotals()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init readTotals: %v\n", err)
		os.Exit(1)
	}
	prevAt := time.Now()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			cur, err := readTotals()
			if err != nil {
				fmt.Fprintf(os.Stderr, "readTotals: %v\n", err)
				continue
			}
			sec := now.Sub(prevAt).Seconds()
			if sec <= 0 {
				continue
			}
			var drx, dtx float64
			if cur.rx >= prev.rx {
				drx = float64(cur.rx - prev.rx)
			}
			if cur.tx >= prev.tx {
				dtx = float64(cur.tx - prev.tx)
			}
			rxBps := drx / sec
			txBps := dtx / sec

			pl := Payload{
				Host:             host,
				NodeName:         nodeName,
				Timestamp:        now.UTC().Format(time.RFC3339Nano),
				IntervalSeconds:  sec,
				RxBytesPerSec:    rxBps,
				TxBytesPerSec:    txBps,
				RxBitsPerSec:     rxBps * 8,
				TxBitsPerSec:     txBps * 8,
				TotalBytesPerSec: rxBps + txBps,
				TotalBitsPerSec:  (rxBps + txBps) * 8,
			}

			body, _ := json.Marshal(pl)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reportURL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}

			log.Printf("reporting: rx=%.1fB/s tx=%.1fB/s to %s\n", pl.RxBytesPerSec, pl.TxBytesPerSec, reportURL)

			resp, err := client.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "POST %s: %v\n", reportURL, err)
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					fmt.Fprintf(os.Stderr, "POST %s: status %s\n", reportURL, resp.Status)
				}
			}

			prev, prevAt = cur, now
		}
	}
}
