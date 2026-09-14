package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/sys/unix"
)

const (
	serverAddr          = ":8080"
	clientMetricsAddr   = ":9090"
	ebpfMetricsAddr     = ":9091"
	localTarget         = "http://localhost:8080"
	scenarioDuration    = 30 * time.Second
	scenarioRate        = 100
	scenarioBulkBytes   = 50 << 20
	scenarioBulkStreams = 4
	serverChunkBytes    = 1 << 20
	ebpfCollectInterval = 250 * time.Millisecond
	containerBPFObject  = "/poc/notsent_bpf.o"
	localBPFObject      = "notsent_bpf.o"
)

type result struct {
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	DeadlineCanceled int     `json:"deadline_canceled"`
	GeneratorOffered int     `json:"generator_offered"`
	GeneratorStarted int     `json:"generator_started"`
	GeneratorDrops   int     `json:"generator_drops"`
	BulkBytes        int64   `json:"bulk_bytes"`
	BulkThroughput   float64 `json:"bulk_throughput_bytes_per_second"`
	P50Millis        float64 `json:"p50_ms"`
	P95Millis        float64 `json:"p95_ms"`
	P99Millis        float64 `json:"p99_ms"`
	MaxMillis        float64 `json:"max_ms"`
}

type config struct {
	Mode                string
	Addr                string
	Target              string
	MetricsAddr         string
	Variant             string
	BPFObject           string
	Duration            time.Duration
	Rate                int
	BulkBytes           int
	BulkStreams         int
	ChunkBytes          int
	EBPFCollectInterval time.Duration
}

type tcpConnMeta struct {
	SocketCookie   uint64
	RawLowatBefore uint32
	RawLowatAfter  uint32
	NetnsLowat     uint32
	EffectiveLowat uint32
	LocalAddr      string
	RemoteAddr     string
}

type tcpConnMetaKey struct{}

func main() {
	if err := run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}

	switch cfg.Mode {
	case "client":
		return runClientSuite(ctx, cfg)
	case "server":
		return runServer(cfg)
	case "ebpf":
		return runNotsent(ctx, cfg)
	default:
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
}

func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("poc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := config{
		Mode:                "client",
		Variant:             "local",
		Addr:                serverAddr,
		Duration:            scenarioDuration,
		Rate:                scenarioRate,
		BulkBytes:           scenarioBulkBytes,
		BulkStreams:         scenarioBulkStreams,
		ChunkBytes:          serverChunkBytes,
		EBPFCollectInterval: ebpfCollectInterval,
	}
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "client, server, or ebpf")
	fs.StringVar(&cfg.Variant, "variant", cfg.Variant, "experiment variant label")
	if err := fs.Parse(args[1:]); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	cfg.Target = targetForVariant(cfg.Variant)
	switch cfg.Mode {
	case "client":
		cfg.MetricsAddr = clientMetricsAddr
	case "server":
		cfg.MetricsAddr = ""
	case "ebpf":
		cfg.MetricsAddr = ebpfMetricsAddr
		cfg.BPFObject = bpfObjectPath()
	default:
		return config{}, fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	return cfg, nil
}

func targetForVariant(variant string) string {
	switch variant {
	case "baseline":
		return "http://server-baseline:8080"
	case "notsent-128k":
		return "http://server-notsent-128k:8080"
	case "notsent-4k":
		return "http://server-notsent-4k:8080"
	default:
		return localTarget
	}
}

func bpfObjectPath() string {
	if _, err := os.Stat(containerBPFObject); err == nil {
		return containerBPFObject
	}
	return localBPFObject
}

func runServer(cfg config) error {
	if cfg.ChunkBytes <= 0 {
		return fmt.Errorf("chunk-bytes must be positive")
	}

	netnsLowat, err := readNetnsTCPNotsentLowat()
	if err != nil {
		return fmt.Errorf("read netns tcp_notsent_lowat: %w", err)
	}

	server, err := newServer(cfg.Addr, cfg.BulkBytes, cfg.ChunkBytes, netnsLowat)
	if err != nil {
		return err
	}
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			log.Printf("conn_state=new remote=%s local=%s", conn.RemoteAddr(), conn.LocalAddr())
		}
	}
	log.Printf("listening on %s", cfg.Addr)
	return server.ListenAndServe()
}

func newServer(addr string, bulkBytes int, chunkBytes int, netnsLowat uint32) (*http.Server, error) {
	schedulerMetaCh := make(chan *tcpConnMeta, 16)
	mux := http.NewServeMux()
	logScenarioSocket := func(w http.ResponseWriter, r *http.Request) bool {
		meta, ok := tcpConnMetaFromContext(r.Context())
		if !ok {
			http.Error(w, "missing tcp conn metadata", http.StatusInternalServerError)
			log.Printf("phase=scenario_socket error=missing_tcp_conn_metadata")
			return false
		}
		log.Printf(
			"phase=scenario_socket socket_cookie=%d raw_lowat_before=%d raw_lowat_after=%d netns_lowat=%d effective_lowat=%d remote=%s local=%s",
			meta.SocketCookie,
			meta.RawLowatBefore,
			meta.RawLowatAfter,
			meta.NetnsLowat,
			meta.EffectiveLowat,
			meta.RemoteAddr,
			meta.LocalAddr,
		)
		return true
	}
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-PoC-Phase") == "scenario-warmup" || r.URL.Query().Get("poc_phase") == "scenario-warmup" {
			if !logScenarioSocket(w, r) {
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/scenario-socket", func(w http.ResponseWriter, r *http.Request) {
		if !logScenarioSocket(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/bulk", func(w http.ResponseWriter, r *http.Request) {
		size := bulkBytes
		if raw := r.URL.Query().Get("bytes"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				http.Error(w, "invalid bytes parameter", http.StatusBadRequest)
				return
			}
			size = parsed
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(size))

		chunk := make([]byte, chunkBytes)
		for i := range chunk {
			chunk[i] = byte(i)
		}
		for remaining := size; remaining > 0; {
			n := min(remaining, len(chunk))
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	})

	h2Server := &http2.Server{
		NewWriteScheduler: func() http2.WriteScheduler {
			meta := <-schedulerMetaCh
			if meta != nil {
				log.Printf(
					"phase=http2_scheduler scheduler=small_first socket_cookie=%d remote=%s local=%s",
					meta.SocketCookie,
					meta.RemoteAddr,
					meta.LocalAddr,
				)
			} else {
				log.Printf("phase=http2_scheduler scheduler=small_first")
			}
			return newSmallFirstWriteScheduler()
		},
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(mux, h2Server),
		ReadHeaderTimeout: 5 * time.Second,
	}
	server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		meta, err := buildTCPConnMeta(conn, netnsLowat)
		if err != nil {
			log.Fatalf("inspect accepted tcp connection: %v", err)
		}
		schedulerMetaCh <- meta
		return context.WithValue(ctx, tcpConnMetaKey{}, meta)
	}
	return server, nil
}

func runClientSuite(ctx context.Context, cfg config) error {
	controlClient := h2cClient(nil)
	scenarioDialCount := &atomic.Int64{}
	scenarioClient := h2cClient(scenarioDialCount)
	metrics := newAppMetrics(cfg.Variant)
	metricsServer, err := startMetricsServer(cfg.MetricsAddr, metrics.registry)
	if err != nil {
		return err
	}
	if metricsServer != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
	}

	latencyResult, err := measureLatency(ctx, controlClient, cfg.Target, cfg.Duration, cfg.Rate, metrics, "latency_client")
	if err != nil {
		return err
	}
	log.Printf(
		"phase=latency_client variant=%s requests=%d errors=%d deadline_canceled=%d generator_offered=%d generator_started=%d generator_drops=%d p50_ms=%.3f p95_ms=%.3f p99_ms=%.3f max_ms=%.3f",
		cfg.Variant,
		latencyResult.Requests,
		latencyResult.Errors,
		latencyResult.DeadlineCanceled,
		latencyResult.GeneratorOffered,
		latencyResult.GeneratorStarted,
		latencyResult.GeneratorDrops,
		latencyResult.P50Millis,
		latencyResult.P95Millis,
		latencyResult.P99Millis,
		latencyResult.MaxMillis,
	)

	if transport, ok := controlClient.Transport.(*http2.Transport); ok {
		transport.CloseIdleConnections()
	}

	warmupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := doRequestWithHeaders(warmupCtx, scenarioClient, cfg.Target+"/scenario-socket", http.Header{
		"X-PoC-Phase": []string{"scenario-warmup"},
	}); err != nil {
		return fmt.Errorf("establish scenario connection: %w", err)
	}

	scenarioStart := time.Now().UnixNano()
	log.Printf("phase=scenario_window event=start variant=%s unix_ns=%d", cfg.Variant, scenarioStart)
	scenarioResult, err := runScenario(ctx, scenarioClient, cfg.Target, cfg.Duration, cfg.Rate, cfg.BulkBytes, cfg.BulkStreams, metrics)
	scenarioEnd := time.Now().UnixNano()
	log.Printf("phase=scenario_window event=end variant=%s unix_ns=%d", cfg.Variant, scenarioEnd)
	if err != nil {
		return err
	}
	scenarioConnections := scenarioDialCount.Load()
	log.Printf(
		"phase=scenario variant=%s requests=%d errors=%d deadline_canceled=%d generator_offered=%d generator_started=%d generator_drops=%d bulk_bytes=%d bulk_throughput_bytes_per_second=%.2f p50_ms=%.3f p95_ms=%.3f p99_ms=%.3f max_ms=%.3f scenario_tcp_connections=%d",
		cfg.Variant,
		scenarioResult.Requests,
		scenarioResult.Errors,
		scenarioResult.DeadlineCanceled,
		scenarioResult.GeneratorOffered,
		scenarioResult.GeneratorStarted,
		scenarioResult.GeneratorDrops,
		scenarioResult.BulkBytes,
		scenarioResult.BulkThroughput,
		scenarioResult.P50Millis,
		scenarioResult.P95Millis,
		scenarioResult.P99Millis,
		scenarioResult.MaxMillis,
		scenarioConnections,
	)
	return nil
}

func runScenario(ctx context.Context, client *http.Client, target string, duration time.Duration, rate int, bulkBytes int, bulkStreams int, metrics *appMetrics) (result, error) {
	if bulkStreams < 0 {
		return result{}, fmt.Errorf("bulk-streams must be non-negative")
	}

	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var bulkTotal atomic.Int64
	var bulkErrs atomic.Int64
	var bulkDeadlineCanceled atomic.Int64
	var wg sync.WaitGroup
	for range bulkStreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				n, err := downloadBulk(ctx, client, target, bulkBytes, nil)
				if n > 0 {
					bulkTotal.Add(n)
					metrics.observeBulkBytes("scenario", n)
				}
				if err != nil {
					if isDeadlineCanceled(ctx, err) {
						bulkDeadlineCanceled.Add(1)
					} else {
						bulkErrs.Add(1)
					}
					return
				}
			}
		}()
	}

	start := time.Now()
	res, err := measureLatency(ctx, client, target, duration, rate, metrics, "scenario")
	wg.Wait()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return result{}, err
	}
	res.BulkBytes = bulkTotal.Load()
	res.Errors += int(bulkErrs.Load())
	res.DeadlineCanceled += int(bulkDeadlineCanceled.Load())
	if elapsed := time.Since(start).Seconds(); elapsed > 0 {
		res.BulkThroughput = float64(res.BulkBytes) / elapsed
		metrics.setBulkThroughput("scenario", res.BulkThroughput)
	}
	return res, nil
}

type appMetrics struct {
	registry       *prometheus.Registry
	variant        string
	pingRequests   *prometheus.CounterVec
	pingErrors     *prometheus.CounterVec
	pingLatency    *prometheus.HistogramVec
	bulkBytes      *prometheus.CounterVec
	bulkThroughput *prometheus.GaugeVec
}

func newAppMetrics(variant string) *appMetrics {
	m := &appMetrics{
		registry: prometheus.NewRegistry(),
		variant:  variant,
		pingRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcpqueue",
			Name:      "ping_requests_total",
			Help:      "Total ping requests attempted by the PoC client.",
		}, []string{"phase", "variant"}),
		pingErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcpqueue",
			Name:      "ping_errors_total",
			Help:      "Total ping requests that failed or returned a non-200 response.",
		}, []string{"phase", "variant"}),
		pingLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "tcpqueue",
			Name:      "ping_latency_seconds",
			Help:      "Observed ping request latency in seconds.",
			Buckets:   []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"phase", "variant"}),
		bulkBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "tcpqueue",
			Name:      "bulk_bytes_total",
			Help:      "Total bytes read from bulk responses.",
		}, []string{"phase", "variant"}),
		bulkThroughput: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "bulk_throughput_bytes_per_second",
			Help:      "Most recent measured bulk throughput in bytes per second.",
		}, []string{"phase", "variant"}),
	}
	m.registry.MustRegister(m.pingRequests, m.pingErrors, m.pingLatency, m.bulkBytes, m.bulkThroughput)
	return m
}

func startMetricsServer(addr string, registry *prometheus.Registry) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server stopped: %v", err)
		}
	}()
	return server, nil
}

func (m *appMetrics) observePingAttempt(phase string) {
	m.pingRequests.WithLabelValues(phase, m.variant).Inc()
}

func (m *appMetrics) observePingError(phase string) {
	m.pingErrors.WithLabelValues(phase, m.variant).Inc()
}

func (m *appMetrics) observePingLatency(phase string, latency time.Duration) {
	m.pingLatency.WithLabelValues(phase, m.variant).Observe(latency.Seconds())
}

func (m *appMetrics) observeBulk(phase string, bytes int64, throughput float64) {
	m.observeBulkBytes(phase, bytes)
	m.setBulkThroughput(phase, throughput)
}

func (m *appMetrics) observeBulkBytes(phase string, bytes int64) {
	m.bulkBytes.WithLabelValues(phase, m.variant).Add(float64(bytes))
}

func (m *appMetrics) setBulkThroughput(phase string, throughput float64) {
	m.bulkThroughput.WithLabelValues(phase, m.variant).Set(throughput)
}

func h2cClient(dialCount *atomic.Int64) *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				conn, err := d.DialContext(ctx, network, addr)
				if err == nil && dialCount != nil {
					dialCount.Add(1)
				}
				return conn, err
			},
		},
	}
}

func measureLatency(ctx context.Context, client *http.Client, target string, duration time.Duration, rate int, metrics *appMetrics, phase string) (result, error) {
	if rate <= 0 {
		return result{}, fmt.Errorf("rate must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	period := time.Second / time.Duration(rate)
	if period <= 0 {
		return result{}, fmt.Errorf("rate %d is too high", rate)
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	latencies := make([]time.Duration, 0, int(math.Ceil(duration.Seconds()*float64(rate))))
	var (
		errs             int
		deadlineCanceled int
		generatorOffered int
		generatorStarted int
		generatorDrops   int
		mu               sync.Mutex
		wg               sync.WaitGroup
	)
	inFlight := make(chan struct{}, rate)
	recordLatency := func(latency time.Duration) {
		mu.Lock()
		latencies = append(latencies, latency)
		mu.Unlock()
	}

	recordError := func() {
		mu.Lock()
		errs++
		mu.Unlock()
	}

	recordDeadlineCanceled := func() {
		mu.Lock()
		deadlineCanceled++
		mu.Unlock()
	}

	recordGeneratorOffer := func(started bool) {
		mu.Lock()
		generatorOffered++
		if started {
			generatorStarted++
		} else {
			generatorDrops++
		}
		mu.Unlock()
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			mu.Lock()
			snapshot := append([]time.Duration(nil), latencies...)
			errorCount := errs
			deadlineCanceledCount := deadlineCanceled
			generatorOfferedCount := generatorOffered
			generatorStartedCount := generatorStarted
			generatorDropsCount := generatorDrops
			mu.Unlock()
			summary := summarize(snapshot, errorCount, deadlineCanceledCount)
			summary.GeneratorOffered = generatorOfferedCount
			summary.GeneratorStarted = generatorStartedCount
			summary.GeneratorDrops = generatorDropsCount
			return summary, nil
		case <-ticker.C:
			select {
			case inFlight <- struct{}{}:
				recordGeneratorOffer(true)
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						<-inFlight
					}()

					metrics.observePingAttempt(phase)
					latency, err := doPing(ctx, client, target)
					if err != nil {
						if isDeadlineCanceled(ctx, err) {
							recordDeadlineCanceled()
						} else {
							recordError()
							metrics.observePingError(phase)
						}
						return
					}

					recordLatency(latency)
					metrics.observePingLatency(phase, latency)
				}()
			default:
				recordGeneratorOffer(false)
			}
		}
	}
}

func doPing(ctx context.Context, client *http.Client, target string) (time.Duration, error) {
	return doPingWithHeaders(ctx, client, target, nil)
}

func doPingWithHeaders(ctx context.Context, client *http.Client, target string, headers http.Header) (time.Duration, error) {
	return doRequestWithHeaders(ctx, client, target+"/ping", headers)
}

func doRequestWithHeaders(ctx context.Context, client *http.Client, url string, headers http.Header) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}

	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ping request returned %s", resp.Status)
	}
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return time.Since(start), nil
}

func downloadBulk(ctx context.Context, client *http.Client, target string, bytes int, headers http.Header) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/bulk?bytes=%d", target, bytes), nil)
	if err != nil {
		return 0, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bulk request returned %s", resp.Status)
	}
	return io.Copy(io.Discard, resp.Body)
}

func summarize(latencies []time.Duration, errs int, deadlineCanceled int) result {
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})
	return result{
		Requests:         len(latencies),
		Errors:           errs,
		DeadlineCanceled: deadlineCanceled,
		P50Millis:        percentileMillis(latencies, 50),
		P95Millis:        percentileMillis(latencies, 95),
		P99Millis:        percentileMillis(latencies, 99),
		MaxMillis:        percentileMillis(latencies, 100),
	}
}

func isDeadlineCanceled(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func readNetnsTCPNotsentLowat() (uint32, error) {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/tcp_notsent_lowat")
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func buildTCPConnMeta(conn net.Conn, netnsLowat uint32) (*tcpConnMeta, error) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("expected *net.TCPConn, got %T", conn)
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("get syscall conn: %w", err)
	}

	meta := &tcpConnMeta{
		NetnsLowat: netnsLowat,
		LocalAddr:  conn.LocalAddr().String(),
		RemoteAddr: conn.RemoteAddr().String(),
	}

	var controlErr error
	if err := rawConn.Control(func(fd uintptr) {
		fdInt := int(fd)
		rawBefore, err := unix.GetsockoptInt(fdInt, unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
		if err != nil {
			controlErr = fmt.Errorf("get TCP_NOTSENT_LOWAT before: %w", err)
			return
		}
		if err := unix.SetsockoptInt(fdInt, unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 0); err != nil {
			controlErr = fmt.Errorf("set TCP_NOTSENT_LOWAT=0: %w", err)
			return
		}
		rawAfter, err := unix.GetsockoptInt(fdInt, unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
		if err != nil {
			controlErr = fmt.Errorf("get TCP_NOTSENT_LOWAT after: %w", err)
			return
		}
		cookie, err := unix.GetsockoptUint64(fdInt, unix.SOL_SOCKET, unix.SO_COOKIE)
		if err != nil {
			controlErr = fmt.Errorf("get SO_COOKIE: %w", err)
			return
		}

		meta.RawLowatBefore = uint32(rawBefore)
		meta.RawLowatAfter = uint32(rawAfter)
		meta.SocketCookie = cookie
		if meta.RawLowatAfter != 0 {
			meta.EffectiveLowat = meta.RawLowatAfter
		} else {
			meta.EffectiveLowat = netnsLowat
		}
	}); err != nil {
		return nil, fmt.Errorf("inspect TCP connection: %w", err)
	}
	if controlErr != nil {
		return nil, controlErr
	}
	if meta.RawLowatAfter != 0 {
		return nil, fmt.Errorf("TCP_NOTSENT_LOWAT did not remain 0 after override, got %d", meta.RawLowatAfter)
	}
	return meta, nil
}

func tcpConnMetaFromContext(ctx context.Context) (*tcpConnMeta, bool) {
	meta, ok := ctx.Value(tcpConnMetaKey{}).(*tcpConnMeta)
	return meta, ok
}

func percentileMillis(latencies []time.Duration, percentile float64) float64 {
	if len(latencies) == 0 {
		return 0
	}
	if percentile >= 100 {
		return float64(latencies[len(latencies)-1].Microseconds()) / 1000
	}
	rank := int(math.Ceil(percentile/100*float64(len(latencies)))) - 1
	rank = max(0, min(rank, len(latencies)-1))
	return float64(latencies[rank].Microseconds()) / 1000
}

type timeSeriesRow struct {
	SampleUnixNs    int64
	ElapsedMs       float64
	Run             string
	Variant         string
	Phase           string
	CgroupID        string
	SocketCookie    string
	TCPNotsentBytes uint64
}

type exactTimeSeries struct {
	Rows              []timeSeriesRow
	RawSamples        int
	ScenarioSamples   int
	SampledMaxNotsent uint64
}

func parseScenarioWindow(path string, variant string) (int64, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}

	var start, end int64
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "phase=scenario_window") || !strings.Contains(line, "variant="+variant) {
			continue
		}
		switch {
		case strings.Contains(line, "event=start"):
			if start != 0 {
				return 0, 0, fmt.Errorf("duplicate scenario start marker in %s", path)
			}
			ts, err := extractInt64Field(line, "unix_ns")
			if err != nil {
				return 0, 0, err
			}
			start = ts
		case strings.Contains(line, "event=end"):
			if end != 0 {
				return 0, 0, fmt.Errorf("duplicate scenario end marker in %s", path)
			}
			ts, err := extractInt64Field(line, "unix_ns")
			if err != nil {
				return 0, 0, err
			}
			end = ts
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if start == 0 || end == 0 {
		return 0, 0, fmt.Errorf("missing scenario window markers in %s", path)
	}
	if end <= start {
		return 0, 0, fmt.Errorf("scenario end must be after start in %s", path)
	}
	return start, end, nil
}

func loadExactTimeSeries(rawPath, cgroupID, socketCookie, run, variant string, scenarioStart, scenarioEnd int64) (exactTimeSeries, error) {
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return exactTimeSeries{}, err
	}
	if len(raw) == 0 {
		return exactTimeSeries{}, fmt.Errorf("raw time-series file %s is empty", rawPath)
	}

	var (
		rows                []timeSeriesRow
		sampleMarkers       int
		completedSamples    int
		lastSampleUnixNs    int64
		sawExactSocket      bool
		sampledMaxNotsent   uint64
		block               bytes.Buffer
		blockOpen           bool
		currentSampleUnixNs int64
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "SAMPLE "):
			if blockOpen {
				return exactTimeSeries{}, fmt.Errorf("missing END_SAMPLE before next SAMPLE in %s", rawPath)
			}
			ts, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "SAMPLE ")), 10, 64)
			if err != nil {
				return exactTimeSeries{}, fmt.Errorf("parse sample timestamp %q: %w", line, err)
			}
			if sampleMarkers > 0 && ts < lastSampleUnixNs {
				return exactTimeSeries{}, fmt.Errorf("sample timestamps moved backwards in %s", rawPath)
			}
			currentSampleUnixNs = ts
			lastSampleUnixNs = ts
			sampleMarkers++
			block.Reset()
			blockOpen = true
		case line == "END_SAMPLE":
			if !blockOpen {
				return exactTimeSeries{}, fmt.Errorf("END_SAMPLE without SAMPLE in %s", rawPath)
			}
			row, ok, err := extractTimeSeriesRow(block.Bytes(), currentSampleUnixNs, run, variant, cgroupID, socketCookie, scenarioStart, scenarioEnd)
			if err != nil {
				return exactTimeSeries{}, err
			}
			if ok {
				sawExactSocket = true
				rows = append(rows, row)
				if row.TCPNotsentBytes > sampledMaxNotsent {
					sampledMaxNotsent = row.TCPNotsentBytes
				}
			}
			completedSamples++
			blockOpen = false
		default:
			if blockOpen {
				block.WriteString(line)
				block.WriteByte('\n')
			} else if strings.TrimSpace(line) != "" {
				return exactTimeSeries{}, fmt.Errorf("unexpected content outside SAMPLE in %s: %q", rawPath, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return exactTimeSeries{}, err
	}
	if blockOpen && completedSamples == 0 {
		return exactTimeSeries{}, fmt.Errorf("unterminated sample block in %s", rawPath)
	}
	if sampleMarkers == 0 {
		return exactTimeSeries{}, fmt.Errorf("no SAMPLE markers found in %s", rawPath)
	}
	if !sawExactSocket {
		return exactTimeSeries{}, fmt.Errorf("no exact socket series found in %s for cgroup_id=%s socket_cookie=%s", rawPath, cgroupID, socketCookie)
	}

	scenarioSamples := 0
	for _, row := range rows {
		if row.Phase == "scenario" {
			scenarioSamples++
		}
	}
	return exactTimeSeries{
		Rows:              rows,
		RawSamples:        completedSamples,
		ScenarioSamples:   scenarioSamples,
		SampledMaxNotsent: sampledMaxNotsent,
	}, nil
}

func extractTimeSeriesRow(block []byte, sampleUnixNs int64, run, variant, cgroupID, socketCookie string, scenarioStart, scenarioEnd int64) (timeSeriesRow, bool, error) {
	values := make(map[string]uint64)
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(block))
	scanner.Buffer(make([]byte, 4*1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		metricName, labels, value, err := parseMetricLine(line)
		if err != nil {
			return timeSeriesRow{}, false, err
		}
		if labels["cgroup_id"] != cgroupID || labels["socket_cookie"] != socketCookie {
			continue
		}
		if seen[metricName] {
			return timeSeriesRow{}, false, fmt.Errorf("duplicate %s series for cgroup_id=%s socket_cookie=%s", metricName, cgroupID, socketCookie)
		}
		seen[metricName] = true
		values[metricName] = uint64(value)
	}
	if err := scanner.Err(); err != nil {
		return timeSeriesRow{}, false, err
	}

	notsent, ok := values["tcpqueue_tcp_notsent_bytes"]
	if !ok {
		return timeSeriesRow{}, false, nil
	}

	phase := "pre_scenario"
	if sampleUnixNs >= scenarioStart && sampleUnixNs <= scenarioEnd {
		phase = "scenario"
	} else if sampleUnixNs > scenarioEnd {
		phase = "post_scenario"
	}

	elapsedMs := float64(sampleUnixNs-scenarioStart) / 1e6
	return timeSeriesRow{
		SampleUnixNs:    sampleUnixNs,
		ElapsedMs:       elapsedMs,
		Run:             run,
		Variant:         variant,
		Phase:           phase,
		CgroupID:        cgroupID,
		SocketCookie:    socketCookie,
		TCPNotsentBytes: notsent,
	}, true, nil
}

func parseMetricLine(line string) (string, map[string]string, float64, error) {
	valueStart := strings.LastIndexByte(line, ' ')
	if valueStart < 0 {
		return "", nil, 0, fmt.Errorf("invalid metric line %q", line)
	}
	metricPart := strings.TrimSpace(line[:valueStart])
	valuePart := strings.TrimSpace(line[valueStart+1:])
	if valuePart == "" {
		return "", nil, 0, fmt.Errorf("missing metric value in %q", line)
	}
	value, err := strconv.ParseFloat(valuePart, 64)
	if err != nil {
		return "", nil, 0, fmt.Errorf("parse metric value in %q: %w", line, err)
	}

	metricName := metricPart
	labels := map[string]string{}
	if i := strings.IndexByte(metricPart, '{'); i >= 0 {
		if !strings.HasSuffix(metricPart, "}") {
			return "", nil, 0, fmt.Errorf("invalid metric labels in %q", line)
		}
		metricName = metricPart[:i]
		labelsPart := metricPart[i+1 : len(metricPart)-1]
		if labelsPart != "" {
			for _, item := range strings.Split(labelsPart, ",") {
				key, rawValue, ok := strings.Cut(item, "=")
				if !ok {
					return "", nil, 0, fmt.Errorf("invalid label %q in %q", item, line)
				}
				labels[strings.TrimSpace(key)] = strings.Trim(rawValue, "\"")
			}
		}
	}

	return metricName, labels, value, nil
}

func extractInt64Field(line, name string) (int64, error) {
	value := extractFieldValue(line, name)
	if value == "" {
		return 0, fmt.Errorf("missing %s in %q", name, line)
	}
	return strconv.ParseInt(value, 10, 64)
}

func extractFieldValue(line, name string) string {
	prefix := name + "="
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, prefix) {
			return strings.TrimPrefix(field, prefix)
		}
	}
	return ""
}

type fifo[T any] struct {
	values []T
}

func (q *fifo[T]) push(v T) {
	q.values = append(q.values, v)
}

func (q *fifo[T]) empty() bool {
	return len(q.values) == 0
}

func (q *fifo[T]) peek() (T, bool) {
	var zero T
	if len(q.values) == 0 {
		return zero, false
	}
	return q.values[0], true
}

func (q *fifo[T]) pop() (T, bool) {
	var zero T
	if len(q.values) == 0 {
		return zero, false
	}
	v := q.values[0]
	q.values[0] = zero
	q.values = q.values[1:]
	return v, true
}

type streamQueue struct {
	frames fifo[http2.FrameWriteRequest]
}

func (q *streamQueue) push(wr http2.FrameWriteRequest) {
	q.frames.push(wr)
}

func (q *streamQueue) empty() bool {
	return q.frames.empty()
}

func (q *streamQueue) peek() (http2.FrameWriteRequest, bool) {
	return q.frames.peek()
}

func (q *streamQueue) pop() (http2.FrameWriteRequest, bool) {
	return q.frames.pop()
}

type frameChoice struct {
	control   bool
	streamID  uint32
	dataSize  int
	orderRank int
}

type streamCandidate struct {
	streamID   uint32
	dataSize   int
	orderRank  int
	orderIndex int
}

func rankFrameChoices(choices []frameChoice) []frameChoice {
	out := append([]frameChoice(nil), choices...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].control != out[j].control {
			return out[i].control
		}
		if out[i].dataSize != out[j].dataSize {
			return out[i].dataSize < out[j].dataSize
		}
		return out[i].orderRank < out[j].orderRank
	})
	return out
}

func rankStreamCandidates(candidates []streamCandidate) []streamCandidate {
	out := append([]streamCandidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].dataSize != out[j].dataSize {
			return out[i].dataSize < out[j].dataSize
		}
		return out[i].orderRank < out[j].orderRank
	})
	return out
}

func firstProgressingChoice(choices []frameChoice, blocked map[uint32]bool) (frameChoice, bool) {
	for _, choice := range rankFrameChoices(choices) {
		if !blocked[choice.streamID] {
			return choice, true
		}
	}
	return frameChoice{}, false
}

func removeStreamID(order []uint32, nextTie int, streamID uint32) ([]uint32, int, bool) {
	idx := -1
	for i, id := range order {
		if id == streamID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return order, nextTie, false
	}
	order = append(order[:idx], order[idx+1:]...)
	if len(order) == 0 {
		return order, 0, true
	}
	if nextTie > idx {
		nextTie--
	}
	if nextTie >= len(order) {
		nextTie = 0
	}
	if nextTie < 0 {
		nextTie = 0
	}
	return order, nextTie, true
}

type smallFirstWriteScheduler struct {
	control fifo[http2.FrameWriteRequest]
	streams map[uint32]*streamQueue
	order   []uint32
	nextTie int
}

func newSmallFirstWriteScheduler() http2.WriteScheduler {
	return &smallFirstWriteScheduler{
		streams: make(map[uint32]*streamQueue),
	}
}

func (ws *smallFirstWriteScheduler) OpenStream(streamID uint32, options http2.OpenStreamOptions) {
	if ws.streams[streamID] != nil {
		panic(fmt.Errorf("stream %d already opened", streamID))
	}
	ws.streams[streamID] = &streamQueue{}
	ws.order = append(ws.order, streamID)
	_ = options
}

func (ws *smallFirstWriteScheduler) CloseStream(streamID uint32) {
	if ws.streams[streamID] == nil {
		return
	}
	delete(ws.streams, streamID)
	var ok bool
	ws.order, ws.nextTie, ok = removeStreamID(ws.order, ws.nextTie, streamID)
	if !ok {
		ws.nextTie = 0
	}
}

func (ws *smallFirstWriteScheduler) AdjustStream(streamID uint32, priority http2.PriorityParam) {
	_ = streamID
	_ = priority
}

func (ws *smallFirstWriteScheduler) Push(wr http2.FrameWriteRequest) {
	if wr.StreamID() == 0 {
		ws.control.push(wr)
		return
	}
	q := ws.streams[wr.StreamID()]
	if q == nil {
		if wr.DataSize() > 0 {
			panic("add DATA on non-open stream")
		}
		ws.control.push(wr)
		return
	}
	q.push(wr)
}

func (ws *smallFirstWriteScheduler) Pop() (http2.FrameWriteRequest, bool) {
	if wr, ok := ws.control.pop(); ok {
		return wr, true
	}
	if len(ws.order) == 0 {
		return http2.FrameWriteRequest{}, false
	}

	candidates := ws.streamCandidates()
	if len(candidates) == 0 {
		return http2.FrameWriteRequest{}, false
	}
	for _, cand := range rankStreamCandidates(candidates) {
		if wr, ok := ws.consumeCandidate(cand); ok {
			ws.nextTie = ws.advanceTie(cand.orderIndex)
			return wr, true
		}
	}
	return http2.FrameWriteRequest{}, false
}

func (ws *smallFirstWriteScheduler) streamCandidates() []streamCandidate {
	if len(ws.order) == 0 {
		return nil
	}
	start := ws.nextTie % len(ws.order)
	candidates := make([]streamCandidate, 0, len(ws.order))
	for offset := 0; offset < len(ws.order); offset++ {
		orderIndex := (start + offset) % len(ws.order)
		streamID := ws.order[orderIndex]
		q := ws.streams[streamID]
		if q == nil || q.empty() {
			continue
		}
		head, _ := q.peek()
		candidates = append(candidates, streamCandidate{
			streamID:   streamID,
			dataSize:   head.DataSize(),
			orderRank:  offset,
			orderIndex: orderIndex,
		})
	}
	return candidates
}

func (ws *smallFirstWriteScheduler) consumeCandidate(cand streamCandidate) (http2.FrameWriteRequest, bool) {
	q := ws.streams[cand.streamID]
	if q == nil || q.empty() {
		return http2.FrameWriteRequest{}, false
	}

	head, _ := q.peek()
	if head.DataSize() == 0 {
		q.pop()
		return head, true
	}

	consumed, rest, result := head.Consume(math.MaxInt32)
	switch result {
	case 0:
		return http2.FrameWriteRequest{}, false
	case 1:
		q.pop()
		return consumed, true
	case 2:
		q.frames.values[0] = rest
		return consumed, true
	default:
		panic(fmt.Sprintf("unexpected Consume result %d", result))
	}
}

func (ws *smallFirstWriteScheduler) advanceTie(orderIndex int) int {
	if len(ws.order) == 0 {
		return 0
	}
	next := orderIndex + 1
	if next >= len(ws.order) {
		next = 0
	}
	return next
}
