// netload-reporter.go
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
const avgWindow = 5 * time.Minute

type Payload struct {
	Host             string  `json:"host"`
	NodeName         string  `json:"node_name,omitempty"`
	Timestamp        int64   `json:"timestamp"`
	IntervalSeconds  float64 `json:"interval_seconds"`
	RxBytesPerSec    float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec    float64 `json:"tx_bytes_per_sec"`
	RxBitsPerSec     float64 `json:"rx_bits_per_sec"`
	TxBitsPerSec     float64 `json:"tx_bits_per_sec"`
	TotalBytesPerSec float64 `json:"total_bytes_per_sec"`
	TotalBitsPerSec  float64 `json:"total_bits_per_sec"`

	// 5-минутное скользящее среднее
	RxBytesPerSec5m    float64 `json:"rx_bytes_per_sec_5m"`
	TxBytesPerSec5m    float64 `json:"tx_bytes_per_sec_5m"`
	TotalBytesPerSec5m float64 `json:"total_bytes_per_sec_5m"`
	RxBitsPerSec5m     float64 `json:"rx_bits_per_sec_5m"`
	TxBitsPerSec5m     float64 `json:"tx_bits_per_sec_5m"`
	TotalBitsPerSec5m  float64 `json:"total_bits_per_sec_5m"`
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

		// считаем только uplink-и вида en*, всё остальное (lo, cni0, flannel, veth и т.д.) — пропускаем
		if iface == "lo" || !strings.HasPrefix(iface, "en") {
			continue
		}

		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			return c, fmt.Errorf("unexpected format for %s", iface)
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64) // Receive bytes
		tx, err2 := strconv.ParseUint(fields[8], 10, 64) // Transmit bytes
		if err1 != nil || err2 != nil {
			return c, fmt.Errorf("parse counters failed for %s", iface)
		}
		c.rx += rx
		c.tx += tx
	}
	return c, sc.Err()
}

// ---- скользящее окно по накопителям ----

type histEntry struct {
	t     time.Time
	cumRx float64
	cumTx float64
}

func pruneOld(history []histEntry, now time.Time) []histEntry {
	cut := now.Add(-avgWindow)
	// оставляем самую старую точку, если она единственная
	i := 0
	for i < len(history)-1 && history[i].t.Before(cut) {
		i++
	}
	return history[i:]
}

func main() {
	if err := godotenv.Load("../.env"); err != nil {
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

	// накопители с момента старта процесса
	var cumRx, cumTx float64
	history := []histEntry{{t: prevAt, cumRx: 0, cumTx: 0}}

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

			// обновляем накопители и историю
			cumRx += drx
			cumTx += dtx
			history = append(history, histEntry{t: now, cumRx: cumRx, cumTx: cumTx})
			history = pruneOld(history, now)

			// 5-минутное среднее (если истории < ~2 точек, просто берём текущие bps)
			var rx5m, tx5m float64
			old := history[0]
			dt5 := now.Sub(old.t).Seconds()
			if dt5 > 0 {
				rx5m = (cumRx - old.cumRx) / dt5
				tx5m = (cumTx - old.cumTx) / dt5
			} else {
				rx5m = rxBps
				tx5m = txBps
			}

			pl := Payload{
				Host:             host,
				NodeName:         nodeName,
				Timestamp:        now.UTC().Unix(),
				IntervalSeconds:  sec,
				RxBytesPerSec:    rxBps,
				TxBytesPerSec:    txBps,
				RxBitsPerSec:     rxBps * 8,
				TxBitsPerSec:     txBps * 8,
				TotalBytesPerSec: rxBps + txBps,
				TotalBitsPerSec:  (rxBps + txBps) * 8,

				RxBytesPerSec5m:    rx5m,
				TxBytesPerSec5m:    tx5m,
				TotalBytesPerSec5m: rx5m + tx5m,
				RxBitsPerSec5m:     rx5m * 8,
				TxBitsPerSec5m:     tx5m * 8,
				TotalBitsPerSec5m:  (rx5m + tx5m) * 8,
			}

			body, _ := json.Marshal(pl)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reportURL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}

			log.Printf("reporting: rx=%.1fB/s tx=%.1fB/s | 5m avg rx=%.1fB/s tx=%.1fB/s to %s\n",
				pl.RxBytesPerSec, pl.TxBytesPerSec, pl.RxBytesPerSec5m, pl.TxBytesPerSec5m, reportURL)

			//log.Printf("body: %s", body)

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
