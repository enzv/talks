package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	notsentProgramName = "trace_tcp_sendmsg"
	notsentMapName     = "notsent_bytes"
)

type notsentKey struct {
	CgroupID     uint64
	SocketCookie uint64
	PID          uint32
	Padding      uint32
	Comm         [16]byte
}

func runNotsent(ctx context.Context, cfg config) error {
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("warn: unable to raise memlock limit: %v", err)
	}

	spec, err := ebpf.LoadCollectionSpec(cfg.BPFObject)
	if err != nil {
		return fmt.Errorf("load eBPF object %q: %w", cfg.BPFObject, err)
	}

	objects := struct {
		TraceTCPSendmsg           *ebpf.Program `ebpf:"trace_tcp_sendmsg"`
		NotsentBytes              *ebpf.Map     `ebpf:"notsent_bytes"`
		NotsentBytesMax           *ebpf.Map     `ebpf:"notsent_bytes_max"`
		InflightOrUnackedBytes    *ebpf.Map     `ebpf:"inflight_or_unacked_bytes"`
		InflightOrUnackedBytesMax *ebpf.Map     `ebpf:"inflight_or_unacked_bytes_max"`
		OutstandingBytes          *ebpf.Map     `ebpf:"outstanding_bytes"`
		OutstandingBytesMax       *ebpf.Map     `ebpf:"outstanding_bytes_max"`
		SndWndBytes               *ebpf.Map     `ebpf:"snd_wnd_bytes"`
		SndWndBytesMax            *ebpf.Map     `ebpf:"snd_wnd_bytes_max"`
		SndCwndPackets            *ebpf.Map     `ebpf:"snd_cwnd_packets"`
		SndCwndPacketsMax         *ebpf.Map     `ebpf:"snd_cwnd_packets_max"`
		MssCacheBytes             *ebpf.Map     `ebpf:"mss_cache_bytes"`
		PacketsOut                *ebpf.Map     `ebpf:"packets_out"`
		TotalRetrans              *ebpf.Map     `ebpf:"total_retrans"`
	}{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return fmt.Errorf("load eBPF collection: %w", err)
	}
	defer objects.TraceTCPSendmsg.Close()
	defer objects.NotsentBytes.Close()
	defer objects.NotsentBytesMax.Close()
	defer objects.InflightOrUnackedBytes.Close()
	defer objects.InflightOrUnackedBytesMax.Close()
	defer objects.OutstandingBytes.Close()
	defer objects.OutstandingBytesMax.Close()
	defer objects.SndWndBytes.Close()
	defer objects.SndWndBytesMax.Close()
	defer objects.SndCwndPackets.Close()
	defer objects.SndCwndPacketsMax.Close()
	defer objects.MssCacheBytes.Close()
	defer objects.PacketsOut.Close()
	defer objects.TotalRetrans.Close()

	traceLink, err := link.AttachTracing(link.TracingOptions{
		Program:    objects.TraceTCPSendmsg,
		AttachType: ebpf.AttachTraceFExit,
	})
	if err != nil {
		return fmt.Errorf("attach tcp_sendmsg fexit: %w", err)
	}
	defer traceLink.Close()

	metrics := newNotsentMetrics(cfg.Variant)
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

	log.Printf("ebpf notsent collector attached; metrics=%s object=%s", cfg.MetricsAddr, cfg.BPFObject)
	ticker := time.NewTicker(cfg.EBPFCollectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := metrics.collect(
				objects.NotsentBytes,
				objects.NotsentBytesMax,
				objects.InflightOrUnackedBytes,
				objects.InflightOrUnackedBytesMax,
				objects.OutstandingBytes,
				objects.OutstandingBytesMax,
				objects.SndWndBytes,
				objects.SndWndBytesMax,
				objects.SndCwndPackets,
				objects.SndCwndPacketsMax,
				objects.MssCacheBytes,
				objects.PacketsOut,
				objects.TotalRetrans,
			); err != nil {
				return err
			}
		}
	}
}

type notsentMetrics struct {
	registry         *prometheus.Registry
	variant          string
	notsentBytes     *prometheus.GaugeVec
	maxBytes         *prometheus.GaugeVec
	inflightBytes    *prometheus.GaugeVec
	inflightMax      *prometheus.GaugeVec
	outstandingBytes *prometheus.GaugeVec
	outstandingMax   *prometheus.GaugeVec
	sndWndBytes      *prometheus.GaugeVec
	sndWndMax        *prometheus.GaugeVec
	sndCwndPackets   *prometheus.GaugeVec
	sndCwndMax       *prometheus.GaugeVec
	mssCacheBytes    *prometheus.GaugeVec
	packetsOut       *prometheus.GaugeVec
	totalRetrans     *prometheus.GaugeVec
}

func newNotsentMetrics(variant string) *notsentMetrics {
	m := &notsentMetrics{
		registry: prometheus.NewRegistry(),
		variant:  variant,
		notsentBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_notsent_bytes",
			Help:      "TCP bytes copied into the kernel send queue but not yet handed to IP.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		maxBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_notsent_bytes_max",
			Help:      "Maximum observed TCP bytes copied into the kernel send queue but not yet handed to IP.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		inflightBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_inflight_or_unacked_bytes",
			Help:      "TCP bytes already below snd_nxt but not yet acknowledged.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		inflightMax: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_inflight_or_unacked_bytes_max",
			Help:      "Maximum observed TCP bytes already below snd_nxt but not yet acknowledged.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		outstandingBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_outstanding_bytes",
			Help:      "TCP bytes written by the application but not yet acknowledged by the peer.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		outstandingMax: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_outstanding_bytes_max",
			Help:      "Maximum observed TCP bytes written by the application but not yet acknowledged by the peer.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		sndWndBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_snd_wnd_bytes",
			Help:      "TCP advertised send window for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		sndWndMax: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_snd_wnd_bytes_max",
			Help:      "Maximum observed TCP advertised send window for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		sndCwndPackets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_snd_cwnd_packets",
			Help:      "TCP congestion window in packets for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		sndCwndMax: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_snd_cwnd_packets_max",
			Help:      "Maximum observed TCP congestion window in packets for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		mssCacheBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_mss_cache_bytes",
			Help:      "TCP cached MSS in bytes for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		packetsOut: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_packets_out",
			Help:      "TCP packets_out for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
		totalRetrans: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "tcpqueue",
			Name:      "tcp_total_retrans",
			Help:      "TCP total retransmissions for the exact socket.",
		}, []string{"cgroup_id", "socket_cookie", "pid", "comm", "variant"}),
	}
	m.registry.MustRegister(
		m.notsentBytes,
		m.maxBytes,
		m.inflightBytes,
		m.inflightMax,
		m.outstandingBytes,
		m.outstandingMax,
		m.sndWndBytes,
		m.sndWndMax,
		m.sndCwndPackets,
		m.sndCwndMax,
		m.mssCacheBytes,
		m.packetsOut,
		m.totalRetrans,
	)
	return m
}

func (m *notsentMetrics) collect(
	currentValues, maxValues *ebpf.Map,
	inflightCurrent, inflightMax, outstandingCurrent, outstandingMax *ebpf.Map,
	sndWndCurrent, sndWndMax, sndCwndCurrent, sndCwndMax, mssCache, packetsOut, totalRetrans *ebpf.Map,
) error {
	if err := m.collectGauge(currentValues, m.notsentBytes); err != nil {
		return err
	}
	if err := m.collectGauge(maxValues, m.maxBytes); err != nil {
		return err
	}
	if err := m.collectGauge(inflightCurrent, m.inflightBytes); err != nil {
		return err
	}
	if err := m.collectGauge(inflightMax, m.inflightMax); err != nil {
		return err
	}
	if err := m.collectGauge(outstandingCurrent, m.outstandingBytes); err != nil {
		return err
	}
	if err := m.collectGauge(outstandingMax, m.outstandingMax); err != nil {
		return err
	}
	if err := m.collectGauge(sndWndCurrent, m.sndWndBytes); err != nil {
		return err
	}
	if err := m.collectGauge(sndWndMax, m.sndWndMax); err != nil {
		return err
	}
	if err := m.collectGauge(sndCwndCurrent, m.sndCwndPackets); err != nil {
		return err
	}
	if err := m.collectGauge(sndCwndMax, m.sndCwndMax); err != nil {
		return err
	}
	if err := m.collectGauge(mssCache, m.mssCacheBytes); err != nil {
		return err
	}
	if err := m.collectGauge(packetsOut, m.packetsOut); err != nil {
		return err
	}
	if err := m.collectGauge(totalRetrans, m.totalRetrans); err != nil {
		return err
	}
	return nil
}

func (m *notsentMetrics) collectGauge(values *ebpf.Map, gauge *prometheus.GaugeVec) error {
	var key notsentKey
	var value uint64
	iter := values.Iterate()
	for iter.Next(&key, &value) {
		cgroupID := strconv.FormatUint(key.CgroupID, 10)
		socketCookie := strconv.FormatUint(key.SocketCookie, 10)
		pid := strconv.FormatUint(uint64(key.PID), 10)
		comm := bpfString(key.Comm[:])
		gauge.WithLabelValues(cgroupID, socketCookie, pid, comm, m.variant).Set(float64(value))
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate notsent map: %w", err)
	}
	return nil
}

func bpfString(raw []byte) string {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(string(raw))
}
