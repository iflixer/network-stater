// netload-reporter.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

const (
	procNetDev = "/proc/net/dev"
)

type Payload struct {
	Host             string  `json:"host"`
	Timestamp        string  `json:"timestamp"`
	IntervalSeconds  float64 `json:"interval_seconds"`
	RxBytesPerSec    float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec    float64 `json:"tx_bytes_per_sec"`
	RxBitsPerSec     float64 `json:"rx_bits_per_sec"`
	TxBitsPerSec     float64 `json:"tx_bits_per_sec"`
	TotalBytesPerSec float64 `json:"total_bytes_per_sec"`
	TotalBitsPerSec  float64 `json:"total_bits_per_sec"`
}

type counters struct {
	rx uint64
	tx uint64
}

func readTotals() (c counters, err error) {
	f, err := os.Open(procNetDev)
	if err != nil {
		return c, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		// пропускаем 2 заголовка
		if lineNum <= 2 {
			continue
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// формат: "eth0:  bytes    packets errs drop fifo frame compressed multicast ... |  tx: bytes packets ..."
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" || strings.HasPrefix(iface, "lo:") {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			return c, fmt.Errorf("unexpected /proc/net/dev format for %s", iface)
		}
		rxBytes, err1 := parseUint(fields[0])
		txBytes, err2 := parseUint(fields[8]) // 9-е поле после двоеточия — TX bytes
		if err1 != nil || err2 != nil {
			return c, errors.New("failed to parse counters")
		}
		c.rx += rxBytes
		c.tx += txBytes
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	return c, nil
}

func parseUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, 64)
}

func main() {
	err := godotenv.Load("../.env")
	if err != nil {
		log.Println("No .env file found")
	}

	reportURL := os.Getenv("REPORT_URL") // например: https://metrics.example.com/net
	if reportURL == "" {
		fmt.Fprintln(os.Stderr, "ERROR: set REPORT_URL env var")
		os.Exit(1)
	}
	apiKey := os.Getenv("API_KEY") // опционально, для авторизации
	interval := time.Minute        // 1 минута
	if v := os.Getenv("INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}

	host, _ := os.Hostname()
	host = filepath.Base(host)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// начальный замер
	prev, err := readTotals()
	if err != nil {
		fmt.Fprintf(os.Stderr, "readTotals init: %v\n", err)
		os.Exit(1)
	}
	prevAt := time.Now()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	fmt.Printf("netload-reporter started: reportURL=%s interval=%s host=%s\n", reportURL, interval, host)

	for {
		select {
		case <-ctx.Done():
			fmt.Println("shutdown")
			return
		case <-ticker.C:
			now := time.Now()
			cur, err := readTotals()
			if err != nil {
				fmt.Fprintf(os.Stderr, "readTotals: %v\n", err)
				continue
			}
			elapsed := now.Sub(prevAt).Seconds()
			if elapsed <= 0 {
				continue
			}

			// считаем скорость
			var drx, dtx float64
			if cur.rx >= prev.rx {
				drx = float64(cur.rx - prev.rx)
			}
			if cur.tx >= prev.tx {
				dtx = float64(cur.tx - prev.tx)
			}
			rxBps := drx / elapsed
			txBps := dtx / elapsed

			payload := Payload{
				Host:             host,
				Timestamp:        now.UTC().Format(time.RFC3339Nano),
				IntervalSeconds:  elapsed,
				RxBytesPerSec:    rxBps,
				TxBytesPerSec:    txBps,
				RxBitsPerSec:     rxBps * 8,
				TxBitsPerSec:     txBps * 8,
				TotalBytesPerSec: rxBps + txBps,
				TotalBitsPerSec:  (rxBps + txBps) * 8,
			}

			body, _ := json.Marshal(payload)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reportURL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "POST %s: %v\n", reportURL, err)
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode >= 300 {
					fmt.Fprintf(os.Stderr, "POST %s: status %s\n", reportURL, resp.Status)
				}
			}

			// сдвигаем окно
			prev = cur
			prevAt = now
		}
	}
}
