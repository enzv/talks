package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	defaultNamespace   = "tcp-notsent-poc"
	defaultServerLabel = "app=tcp-notsent-server"
	defaultTCInterface = "eth0"
	defaultTCNetem     = "delay 40ms rate 100mbit limit 256"
	defaultReportFile  = "result.json"
)

var e2e = flag.Bool("e2e", false, "run tests that mutate the configured Kubernetes cluster")

type testLogger struct {
	t *testing.T
}

func (l *testLogger) Log(format string, args ...any) {
	l.t.Helper()
	l.t.Logf("\t"+format, args...)
}

func TestTCNetem(t *testing.T) {
	if !*e2e {
		t.Skip("set -e2e to apply tc netem to Kubernetes pods")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cluster, err := NewExistingCluster(&testLogger{t: t})
	if err != nil {
		t.Fatalf("NewExistingCluster() error = %v", err)
	}
	if version, err := cluster.serverVersion(ctx); err != nil {
		t.Fatalf("kubernetes cluster not available: %v", err)
	} else {
		cluster.log.Log("server_version=%s", version)
	}

	namespace := envString("POC_NAMESPACE", defaultNamespace)
	label := envString("POC_SERVER_LABEL", defaultServerLabel)
	iface := envString("POC_TC_INTERFACE", defaultTCInterface)
	netem := envString("POC_TC_NETEM", defaultTCNetem)

	pods, err := cluster.runningPods(ctx, namespace, label)
	if err != nil {
		t.Fatalf("server pods not available: %v", err)
	}
	if len(pods) == 0 {
		t.Fatalf("no running pods found in namespace %q with label %q", namespace, label)
	}

	for _, pod := range pods {
		cluster.log.Log("applying tc pod=%s dev=%s netem=%q", pod.Name, iface, netem)
		if err := cluster.replaceTCNetem(ctx, pod.Namespace, pod.Name, "server", iface, netem); err != nil {
			t.Fatalf("apply tc netem to pod %q: %v", pod.Name, err)
		}
	}
}

func TestE2E(t *testing.T) {
	if !*e2e {
		t.Skip("set -e2e to run the Kubernetes proof-of-concept")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 29*time.Minute)
	defer cancel()

	cluster, err := NewExistingCluster(&testLogger{t: t})
	if err != nil {
		t.Fatalf("NewExistingCluster() error = %v", err)
	}
	if version, err := cluster.serverVersion(ctx); err != nil {
		t.Fatalf("kubernetes cluster not available: %v", err)
	} else {
		cluster.log.Log("server_version=%s", version)
	}

	cfg, err := newE2EConfigFromEnv()
	if err != nil {
		t.Fatalf("e2e config: %v", err)
	}
	cfg.WorkDir = t.TempDir()
	runner := &e2eRunner{
		cluster: cluster,
		cfg:     cfg,
	}
	rows, err := runner.run(ctx)
	if err != nil {
		t.Fatalf("TestE2E() error = %v", err)
	}

	t.Log("\nvariant | scheduler | socket_cookie | raw_after | effective_lowat | isolated P99 | scenario P99 | max_notsent | bulk MB/s | TCP conns | qdisc backlog | drops | real_errors | max_sent_unacked | max_outstanding | qdisc_samples | series_samples")
	for _, row := range rows {
		t.Logf("%s | %s | %s | %s | %s | %.3f | %.3f | %d | %.2f | %d | %d/%d | %d | %d | %d | %d | %d | %d",
			row.Variant,
			row.ScenarioScheduler,
			row.ScenarioSocketCookie,
			row.SocketRawLowatAfter,
			row.SocketEffectiveLowat,
			row.IsolatedP99Millis,
			row.ScenarioP99Millis,
			row.MaxNotsentBytes,
			row.BulkMBps,
			row.TCPConnections,
			row.QdiscMaxBacklogBytes,
			row.QdiscMaxBacklogPackets,
			row.QdiscDropsDelta,
			row.ScenarioRealErrors,
			row.MaxSentUnackedBytes,
			row.MaxOutstandingBytes,
			row.QdiscSampleCount,
			row.EBPFTimeSeriesScenarioSamples,
		)
	}
}

type ExistingCluster struct {
	clientset *kubernetes.Clientset
	config    *rest.Config
	context   string
	log       *testLogger
}

func NewExistingCluster(log *testLogger) (*ExistingCluster, error) {
	kubeconfig := envString("KUBECONFIG", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
	kubeContext := os.Getenv("POC_KUBE_CONTEXT")
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return &ExistingCluster{
		clientset: clientset,
		config:    config,
		context:   kubeContext,
		log:       log,
	}, nil
}

func (c *ExistingCluster) serverVersion(ctx context.Context) (string, error) {
	version, err := c.clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return version.GitVersion, nil
}

func (c *ExistingCluster) runningPods(ctx context.Context, namespace string, label string) ([]corev1.Pod, error) {
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return nil, err
	}
	var running []corev1.Pod
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) {
			running = append(running, pod)
		}
	}
	return running, nil
}

func (c *ExistingCluster) replaceTCNetem(ctx context.Context, namespace, pod, container, iface, netem string) error {
	cmd := []string{"/usr/sbin/tc", "qdisc", "replace", "dev", iface, "root", "netem"}
	cmd = append(cmd, strings.Fields(netem)...)
	_, err := c.execPod(ctx, namespace, pod, container, cmd)
	return err
}

func (c *ExistingCluster) execPod(ctx context.Context, namespace, pod, container string, cmd []string) (string, error) {
	req := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil && stderr.Len() > 0 {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), err
}

func envString(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type e2eVariant struct {
	Name          string
	Job           string
	ExpectedLowat string
}

type e2eConfig struct {
	Namespace              string
	ServerLabel            string
	EBPFLabel              string
	TCInterface            string
	TCNetem                string
	EBPFCollectIntervalMS  int
	EBPFScrapeIntervalMS   int
	ReportFile             string
	WorkDir                string
	Variants               []e2eVariant
	RestartDeployments     []string
	DeleteScenarioJobNames []string
}

type e2eRunner struct {
	cluster *ExistingCluster
	cfg     e2eConfig
}

type e2eRow struct {
	Variant                       string
	KubernetesVersion             string
	KernelVersion                 string
	ServerPod                     string
	ServerNode                    string
	CollectorPod                  string
	ServerCgroupID                string
	TCQdisc                       string
	EffectiveLowat                string
	ScenarioSocketCookie          string
	SocketRawLowatBefore          string
	SocketRawLowatAfter           string
	SocketNetnsLowat              string
	SocketEffectiveLowat          string
	TCPConnections                int
	IsolatedRequests              int
	IsolatedRealErrors            int
	IsolatedDeadlineCanceled      int
	IsolatedP50Millis             float64
	IsolatedP95Millis             float64
	IsolatedP99Millis             float64
	IsolatedMaxMillis             float64
	ScenarioRequests              int
	ScenarioRealErrors            int
	ScenarioDeadlineCanceled      int
	ScenarioP50Millis             float64
	ScenarioP95Millis             float64
	ScenarioP99Millis             float64
	ScenarioMaxMillis             float64
	BulkBytes                     int64
	BulkMBps                      float64
	MaxNotsentBytes               uint64
	QdiscMaxBacklogBytes          uint64
	QdiscMaxBacklogPackets        uint64
	QdiscDropsDelta               uint64
	QdiscOverlimitsDelta          uint64
	NotsentAttribution            string
	ScenarioScheduler             string
	MaxSentUnackedBytes           uint64
	MaxOutstandingBytes           uint64
	QdiscSampleCount              int
	MaxSndWndBytes                uint64
	MaxSndCwndPackets             uint64
	MSSCacheBytes                 uint64
	PacketsOut                    uint64
	TotalRetrans                  uint64
	GeneratorOfferedRequests      int
	GeneratorStartedRequests      int
	GeneratorDrops                int
	EBPFCollectIntervalMS         int
	EBPFScrapeIntervalMS          int
	EBPFTimeSeriesRawSamples      int
	EBPFTimeSeriesScenarioSamples int
	TimeSeriesSampledMaxNotsent   uint64
	ScenarioWindowStartNS         int64
	ScenarioWindowEndNS           int64
	TimeSeries                    []timeSeriesRow
}

type qdiscStats struct {
	SampleCount       int
	MaxBacklogBytes   uint64
	MaxBacklogPackets uint64
	DropsDelta        uint64
	OverlimitsDelta   uint64
}

type e2eReport struct {
	OK       bool          `json:"ok"`
	Fail     string        `json:"fail,omitempty"`
	Time     string        `json:"time"`
	Env      reportEnv     `json:"env"`
	Config   reportConfig  `json:"config"`
	Baseline variantReport `json:"baseline"`
	Tuned128 variantReport `json:"128k"`
	Tuned4   variantReport `json:"4k"`
}

type reportEnv struct {
	Kubernetes string `json:"k8s"`
	Kernel     string `json:"kernel"`
	Go         string `json:"go"`
}

type reportConfig struct {
	Netem           string `json:"netem"`
	DurationSeconds int    `json:"duration_s"`
	PingHz          int    `json:"ping_hz"`
	BulkStreams     int    `json:"bulk_streams"`
	BulkBytes       int    `json:"bulk_bytes"`
	ChunkBytes      int    `json:"chunk_bytes"`
	Scheduler       string `json:"scheduler"`
}

type variantReport struct {
	Lowat           string         `json:"lowat"`
	Cgroup          string         `json:"cgroup"`
	Socket          string         `json:"socket"`
	RawLowat        string         `json:"raw_lowat"`
	Conns           int            `json:"conns"`
	IsolatedP99MS   float64        `json:"isolated_p99_ms"`
	ScenarioP99MS   float64        `json:"scenario_p99_ms"`
	MaxNotsent      uint64         `json:"max_notsent"`
	MaxSentUnacked  uint64         `json:"max_sent_unacked"`
	MaxOutstanding  uint64         `json:"max_outstanding"`
	QdiscSamples    int            `json:"qdisc_samples"`
	QdiscBacklog    uint64         `json:"qdisc_backlog"`
	QdiscDrops      uint64         `json:"qdisc_drops"`
	Requests        int            `json:"requests"`
	Errors          int            `json:"errors"`
	GeneratorDrops  int            `json:"generator_drops"`
	BulkMBps        float64        `json:"bulk_MBps"`
	WindowNS        [2]int64       `json:"window_ns"`
	SampledMax      uint64         `json:"sampled_max_notsent"`
	ScenarioSamples int            `json:"scenario_samples"`
	Series          []seriesSample `json:"series"`
}

type seriesSample struct {
	MS      int64  `json:"ms"`
	Notsent uint64 `json:"notsent"`
}

type e2ePaths struct {
	Dir               string
	QdiscRaw          string
	QdiscErr          string
	ClientLog         string
	ServerLog         string
	EBPFProm          string
	EBPFTimeSeriesRaw string
	EBPFTimeSeriesErr string
}

type kubectlSampler struct {
	cancel context.CancelFunc
	done   chan error
}

func newE2EConfigFromEnv() (e2eConfig, error) {
	variants, err := parseE2EVariants(envString("POC_VARIANT_ORDER", "baseline,notsent-128k,notsent-4k"))
	if err != nil {
		return e2eConfig{}, err
	}
	return e2eConfig{
		Namespace:             envString("POC_NAMESPACE", defaultNamespace),
		ServerLabel:           envString("POC_SERVER_LABEL", defaultServerLabel),
		EBPFLabel:             "app=tcp-notsent-ebpf",
		TCInterface:           envString("POC_TC_INTERFACE", defaultTCInterface),
		TCNetem:               envString("POC_TC_NETEM", defaultTCNetem),
		EBPFCollectIntervalMS: 250,
		EBPFScrapeIntervalMS:  1,
		ReportFile:            envString("POC_E2E_JSON", defaultReportFile),
		Variants:              variants,
		RestartDeployments: []string{
			"server-baseline",
			"server-notsent-128k",
			"server-notsent-4k",
		},
		DeleteScenarioJobNames: []string{
			"scenario-baseline",
			"scenario-notsent-128k",
			"scenario-notsent-4k",
		},
	}, nil
}

func parseE2EVariants(raw string) ([]e2eVariant, error) {
	var variants []e2eVariant
	for _, field := range strings.Split(raw, ",") {
		name := strings.TrimSpace(field)
		if name == "" {
			continue
		}
		variant, err := e2eVariantByName(name)
		if err != nil {
			return nil, err
		}
		variants = append(variants, variant)
	}
	if len(variants) == 0 {
		return nil, fmt.Errorf("POC_VARIANT_ORDER is empty")
	}
	return variants, nil
}

func e2eVariantByName(name string) (e2eVariant, error) {
	switch name {
	case "baseline":
		return e2eVariant{Name: name, Job: "scenario-baseline", ExpectedLowat: "4294967295"}, nil
	case "notsent-128k":
		return e2eVariant{Name: name, Job: "scenario-notsent-128k", ExpectedLowat: "131072"}, nil
	case "notsent-4k":
		return e2eVariant{Name: name, Job: "scenario-notsent-4k", ExpectedLowat: "4096"}, nil
	default:
		return e2eVariant{}, fmt.Errorf("unknown variant in POC_VARIANT_ORDER: %s", name)
	}
}

func (r *e2eRunner) run(ctx context.Context) ([]e2eRow, error) {
	if r.cfg.WorkDir == "" {
		return nil, fmt.Errorf("e2e workdir is empty")
	}
	if err := os.MkdirAll(r.cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if err := removeReport(r.cfg.ReportFile); err != nil {
		return nil, err
	}
	if err := r.cluster.ensureNamespace(ctx, r.cfg.Namespace); err != nil {
		return nil, err
	}
	if err := r.cluster.deleteJobs(ctx, r.cfg.Namespace, r.cfg.DeleteScenarioJobNames); err != nil {
		return nil, err
	}
	if _, err := r.cluster.kubectl(ctx, "apply", "-f", "kubernetes.yml"); err != nil {
		return nil, err
	}
	for _, deployment := range r.cfg.RestartDeployments {
		if err := r.cluster.restartRollout(ctx, r.cfg.Namespace, "deployment/"+deployment); err != nil {
			return nil, err
		}
	}
	if err := r.applyTCNetem(ctx); err != nil {
		return nil, err
	}

	var rows []e2eRow
	for _, variant := range r.cfg.Variants {
		row, err := r.runVariant(ctx, variant)
		if err != nil {
			report := buildE2EReport(r.cfg, rows)
			report.OK = false
			report.Fail = err.Error()
			_ = writeE2EReport(r.cfg.ReportFile, report)
			return rows, err
		}
		rows = append(rows, row)
	}

	report := buildE2EReport(r.cfg, rows)
	if fail := evaluateE2EReport(report); fail != "" {
		report.OK = false
		report.Fail = fail
		if err := writeE2EReport(r.cfg.ReportFile, report); err != nil {
			return rows, err
		}
		return rows, fmt.Errorf("e2e gates failed: %s", fail)
	}
	report.OK = true
	if err := writeE2EReport(r.cfg.ReportFile, report); err != nil {
		return rows, err
	}
	return rows, nil
}

func (r *e2eRunner) applyTCNetem(ctx context.Context) error {
	r.cluster.log.Log("applying controlled network bottleneck: %s", r.cfg.TCNetem)
	pods, err := r.cluster.runningPods(ctx, r.cfg.Namespace, r.cfg.ServerLabel)
	if err != nil {
		return fmt.Errorf("list server pods: %w", err)
	}
	if len(pods) == 0 {
		return fmt.Errorf("no running pods found in namespace %q with label %q", r.cfg.Namespace, r.cfg.ServerLabel)
	}
	for _, pod := range pods {
		if !podContainerReady(pod, "server") {
			return fmt.Errorf("ready pod %q does not have running ready server container", pod.Name)
		}
		r.cluster.log.Log("applying tc pod=%s dev=%s netem=%q", pod.Name, r.cfg.TCInterface, r.cfg.TCNetem)
		if err := r.cluster.replaceTCNetem(ctx, pod.Namespace, pod.Name, "server", r.cfg.TCInterface, r.cfg.TCNetem); err != nil {
			return fmt.Errorf("apply tc netem to pod %q: %w", pod.Name, err)
		}
	}
	return nil
}

func (r *e2eRunner) runVariant(ctx context.Context, variant e2eVariant) (e2eRow, error) {
	paths := newE2EPaths(r.cfg.WorkDir, variant.Name)
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return e2eRow{}, err
	}

	serverPod, err := r.cluster.waitRunningReadyPod(ctx, r.cfg.Namespace, "app=tcp-notsent-server,variant="+variant.Name, 2*time.Minute)
	if err != nil {
		return e2eRow{}, err
	}
	serverNode := serverPod.Spec.NodeName

	if err := r.cluster.restartRollout(ctx, r.cfg.Namespace, "daemonset/notsent-ebpf"); err != nil {
		return e2eRow{}, err
	}
	collectorPod, err := r.cluster.waitRunningReadyPodOnNode(ctx, r.cfg.Namespace, r.cfg.EBPFLabel, serverNode, 2*time.Minute)
	if err != nil {
		return e2eRow{}, err
	}

	serverQdisc, serverNetnsLowat, err := r.verifyServerNetwork(ctx, serverPod.Name, variant.ExpectedLowat)
	if err != nil {
		return e2eRow{}, err
	}
	serverCgroupID, err := r.cluster.execPod(ctx, r.cfg.Namespace, serverPod.Name, "server", []string{"sh", "-lc", "stat -c %i /sys/fs/cgroup"})
	if err != nil {
		return e2eRow{}, fmt.Errorf("read server cgroup id: %w", err)
	}
	serverCgroupID = strings.TrimSpace(serverCgroupID)
	serverKernelVersion, err := r.cluster.execPod(ctx, r.cfg.Namespace, serverPod.Name, "server", []string{"uname", "-r"})
	if err != nil {
		return e2eRow{}, fmt.Errorf("read server kernel version: %w", err)
	}
	serverKernelVersion = strings.TrimSpace(serverKernelVersion)
	kubernetesVersion, err := r.cluster.serverVersion(ctx)
	if err != nil {
		return e2eRow{}, fmt.Errorf("read kubernetes version: %w", err)
	}

	qdiscSampler, err := r.startQdiscSampler(ctx, serverPod.Name, paths.QdiscRaw, paths.QdiscErr)
	if err != nil {
		return e2eRow{}, err
	}
	if err := r.waitForCollectorReady(ctx, collectorPod.Name); err != nil {
		_ = qdiscSampler.Stop()
		return e2eRow{}, err
	}
	ebpfSampler, err := r.startEBPFTimeSeriesSampler(ctx, collectorPod.Name, paths.EBPFTimeSeriesRaw, paths.EBPFTimeSeriesErr)
	if err != nil {
		_ = qdiscSampler.Stop()
		return e2eRow{}, err
	}

	jobErr := r.cluster.runJob(ctx, r.cfg.Namespace, variant.Job, 20*time.Minute)
	_ = qdiscSampler.Stop()
	_ = ebpfSampler.Stop()
	if jobErr != nil {
		r.captureFailureLogs(ctx, variant.Job, serverPod.Name, collectorPod.Name, paths)
		return e2eRow{}, jobErr
	}

	qdisc, err := parseQdiscStatsFile(paths.QdiscRaw)
	if err != nil {
		return e2eRow{}, err
	}

	if err := r.captureLogs(ctx, variant.Job, serverPod.Name, collectorPod.Name, paths); err != nil {
		return e2eRow{}, err
	}
	row, err := r.parseVariantArtifacts(variant, paths, serverCgroupID, serverNetnsLowat, qdisc)
	if err != nil {
		return e2eRow{}, err
	}
	row.KubernetesVersion = kubernetesVersion
	row.KernelVersion = serverKernelVersion
	row.ServerPod = serverPod.Name
	row.ServerNode = serverNode
	row.CollectorPod = collectorPod.Name
	row.ServerCgroupID = serverCgroupID
	row.TCQdisc = serverQdisc
	return row, nil
}

func newE2EPaths(root, variant string) e2ePaths {
	dir := filepath.Join(root, variant)
	return e2ePaths{
		Dir:               dir,
		QdiscRaw:          filepath.Join(dir, "qdisc.txt"),
		QdiscErr:          filepath.Join(dir, "qdisc.err"),
		ClientLog:         filepath.Join(dir, "client.log"),
		ServerLog:         filepath.Join(dir, "server.log"),
		EBPFProm:          filepath.Join(dir, "ebpf.prom"),
		EBPFTimeSeriesRaw: filepath.Join(dir, "ebpf-timeseries.raw"),
		EBPFTimeSeriesErr: filepath.Join(dir, "ebpf-timeseries.err"),
	}
}

func (r *e2eRunner) verifyServerNetwork(ctx context.Context, serverPod, expectedLowat string) (string, string, error) {
	qdisc, err := r.cluster.execPod(ctx, r.cfg.Namespace, serverPod, "server", []string{"tc", "qdisc", "show", "dev", r.cfg.TCInterface})
	if err != nil {
		return "", "", fmt.Errorf("read tc qdisc: %w", err)
	}
	if !strings.Contains(qdisc, "qdisc netem") {
		return "", "", fmt.Errorf("expected netem qdisc on pod %s, got %q", serverPod, qdisc)
	}
	if !strings.Contains(qdisc, "delay 40") {
		return "", "", fmt.Errorf("expected 40ms delay on pod %s, got %q", serverPod, qdisc)
	}
	if !strings.Contains(qdisc, "rate 100Mbit") {
		return "", "", fmt.Errorf("expected 100Mbit rate on pod %s, got %q", serverPod, qdisc)
	}
	expectedLimit := netemLimit(r.cfg.TCNetem)
	if expectedLimit == "" {
		return "", "", fmt.Errorf("expected POC_TC_NETEM to include a limit, got %q", r.cfg.TCNetem)
	}
	if !strings.Contains(qdisc, "limit "+expectedLimit) {
		return "", "", fmt.Errorf("expected qdisc limit %s on pod %s, got %q", expectedLimit, serverPod, qdisc)
	}

	lowat, err := r.cluster.execPod(ctx, r.cfg.Namespace, serverPod, "server", []string{"cat", "/proc/sys/net/ipv4/tcp_notsent_lowat"})
	if err != nil {
		return "", "", fmt.Errorf("read tcp_notsent_lowat: %w", err)
	}
	lowat = strings.TrimSpace(lowat)
	if lowat != expectedLowat {
		return "", "", fmt.Errorf("expected tcp_notsent_lowat=%s for pod %s, got %s", expectedLowat, serverPod, lowat)
	}
	return strings.TrimSpace(qdisc), lowat, nil
}

func netemLimit(netem string) string {
	fields := strings.Fields(netem)
	for i, field := range fields {
		if field == "limit" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func (r *e2eRunner) startQdiscSampler(ctx context.Context, pod, rawPath, errPath string) (*kubectlSampler, error) {
	script := fmt.Sprintf("trap 'exit 0' TERM INT; while :; do printf 'SAMPLE %%s\\n' \"$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)\"; tc -s qdisc show dev %s; sleep 0.1; done", r.cfg.TCInterface)
	return r.startSampler(ctx, pod, "server", rawPath, errPath, script)
}

func (r *e2eRunner) startEBPFTimeSeriesSampler(ctx context.Context, pod, rawPath, errPath string) (*kubectlSampler, error) {
	sleepSeconds := fmt.Sprintf("%.3f", float64(r.cfg.EBPFScrapeIntervalMS)/1000)
	script := fmt.Sprintf("trap 'exit 0' TERM INT; while :; do ts=$(date +%%s%%N); printf 'SAMPLE %%s\\n' \"$ts\"; curl -fsS http://127.0.0.1:9091/metrics | awk '/^tcpqueue_tcp_notsent_bytes\\{|^tcpqueue_tcp_notsent_bytes_max\\{|^tcpqueue_tcp_inflight_or_unacked_bytes\\{|^tcpqueue_tcp_outstanding_bytes\\{/{print}'; printf 'END_SAMPLE\\n'; sleep %s; done", sleepSeconds)
	return r.startSampler(ctx, pod, "ebpf", rawPath, errPath, script)
}

func (r *e2eRunner) startSampler(ctx context.Context, pod, container, rawPath, errPath, script string) (*kubectlSampler, error) {
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		return nil, err
	}
	stdout, err := os.Create(rawPath)
	if err != nil {
		return nil, err
	}
	stderr, err := os.Create(errPath)
	if err != nil {
		stdout.Close()
		return nil, err
	}

	samplerCtx, cancel := context.WithCancel(ctx)
	req := r.cluster.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(r.cfg.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"sh", "-lc", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(r.cluster.config, "POST", req.URL())
	if err != nil {
		cancel()
		stdout.Close()
		stderr.Close()
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := executor.StreamWithContext(samplerCtx, remotecommand.StreamOptions{
			Stdout: stdout,
			Stderr: stderr,
		})
		_ = stdout.Close()
		_ = stderr.Close()
		done <- err
	}()

	select {
	case err := <-done:
		cancel()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("sampler for pod %s container %s exited immediately", pod, container)
	case <-time.After(250 * time.Millisecond):
		return &kubectlSampler{cancel: cancel, done: done}, nil
	}
}

func (s *kubectlSampler) Stop() error {
	s.cancel()
	select {
	case err := <-s.done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("sampler did not stop after cancellation")
	}
}

func (r *e2eRunner) waitForCollectorReady(ctx context.Context, pod string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := r.cluster.execPod(ctx, r.cfg.Namespace, pod, "ebpf", []string{"sh", "-lc", "curl -fsS http://127.0.0.1:9091/metrics >/dev/null"})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("collector pod %s did not become ready: %w", pod, err)
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (r *e2eRunner) captureFailureLogs(ctx context.Context, job, serverPod, collectorPod string, paths e2ePaths) {
	_ = r.captureLogs(ctx, job, serverPod, collectorPod, paths)
}

func (r *e2eRunner) captureLogs(ctx context.Context, job, serverPod, collectorPod string, paths e2ePaths) error {
	jobPod, err := r.cluster.jobPod(ctx, r.cfg.Namespace, job)
	if err != nil {
		return err
	}
	clientLog, err := r.cluster.podLogs(ctx, r.cfg.Namespace, jobPod.Name, "scenario")
	if err != nil {
		return fmt.Errorf("read client log: %w", err)
	}
	if err := os.WriteFile(paths.ClientLog, []byte(clientLog), 0o644); err != nil {
		return err
	}
	serverLog, err := r.cluster.podLogs(ctx, r.cfg.Namespace, serverPod, "server")
	if err != nil {
		return fmt.Errorf("read server log: %w", err)
	}
	if err := os.WriteFile(paths.ServerLog, []byte(serverLog), 0o644); err != nil {
		return err
	}
	ebpfProm, err := r.cluster.execPod(ctx, r.cfg.Namespace, collectorPod, "ebpf", []string{"curl", "-fsS", "http://127.0.0.1:9091/metrics"})
	if err != nil {
		return fmt.Errorf("scrape ebpf metrics: %w", err)
	}
	return os.WriteFile(paths.EBPFProm, []byte(ebpfProm), 0o644)
}

func (r *e2eRunner) parseVariantArtifacts(variant e2eVariant, paths e2ePaths, serverCgroupID, serverNetnsLowat string, qdisc qdiscStats) (e2eRow, error) {
	serverLog, err := os.ReadFile(paths.ServerLog)
	if err != nil {
		return e2eRow{}, err
	}
	clientLog, err := os.ReadFile(paths.ClientLog)
	if err != nil {
		return e2eRow{}, err
	}

	serverSocketLine, err := exactlyOneLine(string(serverLog), func(line string) bool {
		return strings.Contains(line, "phase=scenario_socket")
	})
	if err != nil {
		return e2eRow{}, fmt.Errorf("scenario socket line: %w", err)
	}
	scenarioSocketCookie := extractFieldValue(serverSocketLine, "socket_cookie")
	socketRawLowatBefore := extractFieldValue(serverSocketLine, "raw_lowat_before")
	socketRawLowatAfter := extractFieldValue(serverSocketLine, "raw_lowat_after")
	socketNetnsLowat := extractFieldValue(serverSocketLine, "netns_lowat")
	socketEffectiveLowat := extractFieldValue(serverSocketLine, "effective_lowat")
	if scenarioSocketCookie == "" || socketRawLowatBefore == "" || socketRawLowatAfter == "" || socketNetnsLowat == "" || socketEffectiveLowat == "" {
		return e2eRow{}, fmt.Errorf("incomplete scenario socket metadata: %s", serverSocketLine)
	}
	if socketRawLowatAfter != "0" {
		return e2eRow{}, fmt.Errorf("expected raw socket TCP_NOTSENT_LOWAT=0 for variant %s, got %s", variant.Name, socketRawLowatAfter)
	}
	if socketEffectiveLowat != variant.ExpectedLowat {
		return e2eRow{}, fmt.Errorf("expected effective socket lowat %s for variant %s, got %s", variant.ExpectedLowat, variant.Name, socketEffectiveLowat)
	}
	if socketNetnsLowat != serverNetnsLowat {
		return e2eRow{}, fmt.Errorf("socket netns lowat %s does not match verified namespace lowat %s", socketNetnsLowat, serverNetnsLowat)
	}

	schedulerLine, err := exactlyOneLine(string(serverLog), func(line string) bool {
		return strings.Contains(line, "phase=http2_scheduler") &&
			extractFieldValue(line, "scheduler") == "small_first" &&
			extractFieldValue(line, "socket_cookie") == scenarioSocketCookie
	})
	if err != nil {
		return e2eRow{}, fmt.Errorf("scenario scheduler line: %w", err)
	}
	scenarioScheduler := extractFieldValue(schedulerLine, "scheduler")

	scenarioStart, scenarioEnd, err := parseScenarioWindow(paths.ClientLog, variant.Name)
	if err != nil {
		return e2eRow{}, err
	}
	timeSeries, err := loadExactTimeSeries(paths.EBPFTimeSeriesRaw, serverCgroupID, scenarioSocketCookie, "", variant.Name, scenarioStart, scenarioEnd)
	if err != nil {
		return e2eRow{}, err
	}

	latencyLine, err := lastLineMatching(string(clientLog), func(line string) bool {
		return strings.Contains(line, "phase=latency_client variant=")
	})
	if err != nil {
		return e2eRow{}, fmt.Errorf("latency client summary: %w", err)
	}
	scenarioLine, err := lastLineMatching(string(clientLog), func(line string) bool {
		return strings.Contains(line, "phase=scenario variant=")
	})
	if err != nil {
		return e2eRow{}, fmt.Errorf("scenario summary: %w", err)
	}

	row, err := r.parseResultRow(variant, latencyLine, scenarioLine, scenarioSocketCookie, socketRawLowatBefore, socketRawLowatAfter, socketNetnsLowat, socketEffectiveLowat, scenarioScheduler, serverCgroupID, paths, qdisc, timeSeries, scenarioStart, scenarioEnd)
	if err != nil {
		return e2eRow{}, err
	}
	return row, nil
}

func (r *e2eRunner) parseResultRow(variant e2eVariant, latencyLine, scenarioLine, scenarioSocketCookie, socketRawLowatBefore, socketRawLowatAfter, socketNetnsLowat, socketEffectiveLowat, scenarioScheduler, serverCgroupID string, paths e2ePaths, qdisc qdiscStats, timeSeries exactTimeSeries, scenarioStart, scenarioEnd int64) (e2eRow, error) {
	bulkThroughput, err := parseLogFloat(scenarioLine, "bulk_throughput_bytes_per_second")
	if err != nil {
		return e2eRow{}, err
	}
	maxNotsent, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_notsent_bytes_max", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	sampledMax := timeSeries.SampledMaxNotsent
	if sampledMax > maxNotsent {
		return e2eRow{}, fmt.Errorf("sampled max notsent %d exceeded kernel max %d for variant %s", sampledMax, maxNotsent, variant.Name)
	}
	scenarioSamples := timeSeries.ScenarioSamples
	if scenarioSamples < 80 {
		return e2eRow{}, fmt.Errorf("exact-socket time-series produced only %d scenario samples for variant %s", scenarioSamples, variant.Name)
	}

	tcpConnections, err := parseLogInt(scenarioLine, "scenario_tcp_connections")
	if err != nil {
		return e2eRow{}, err
	}
	if tcpConnections != 1 {
		return e2eRow{}, fmt.Errorf("expected one scenario TCP connection for variant %s, got %d", variant.Name, tcpConnections)
	}
	scenarioErrors, err := parseLogInt(scenarioLine, "errors")
	if err != nil {
		return e2eRow{}, err
	}
	if scenarioErrors != 0 {
		return e2eRow{}, fmt.Errorf("expected zero real scenario request errors for variant %s, got %d", variant.Name, scenarioErrors)
	}
	generatorDrops, err := parseLogInt(scenarioLine, "generator_drops")
	if err != nil {
		return e2eRow{}, err
	}
	if generatorDrops != 0 {
		return e2eRow{}, fmt.Errorf("expected zero generator drops for variant %s, got %d", variant.Name, generatorDrops)
	}
	isolatedRequests, err := parseLogInt(latencyLine, "requests")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedErrors, err := parseLogInt(latencyLine, "errors")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedDeadlineCanceled, err := parseLogInt(latencyLine, "deadline_canceled")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedP50, err := parseLogFloat(latencyLine, "p50_ms")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedP95, err := parseLogFloat(latencyLine, "p95_ms")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedP99, err := parseLogFloat(latencyLine, "p99_ms")
	if err != nil {
		return e2eRow{}, err
	}
	isolatedMax, err := parseLogFloat(latencyLine, "max_ms")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioRequests, err := parseLogInt(scenarioLine, "requests")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioDeadlineCanceled, err := parseLogInt(scenarioLine, "deadline_canceled")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioP50, err := parseLogFloat(scenarioLine, "p50_ms")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioP95, err := parseLogFloat(scenarioLine, "p95_ms")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioP99, err := parseLogFloat(scenarioLine, "p99_ms")
	if err != nil {
		return e2eRow{}, err
	}
	scenarioMax, err := parseLogFloat(scenarioLine, "max_ms")
	if err != nil {
		return e2eRow{}, err
	}
	bulkBytes, err := parseLogInt(scenarioLine, "bulk_bytes")
	if err != nil {
		return e2eRow{}, err
	}
	maxSentUnacked, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_inflight_or_unacked_bytes_max", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	maxOutstanding, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_outstanding_bytes_max", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	maxSndWnd, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_snd_wnd_bytes_max", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	maxSndCwnd, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_snd_cwnd_packets_max", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	mssCache, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_mss_cache_bytes", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	packetsOut, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_packets_out", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	totalRetrans, err := scrapeSocketMetric(paths.EBPFProm, "tcpqueue_tcp_total_retrans", serverCgroupID, scenarioSocketCookie)
	if err != nil {
		return e2eRow{}, err
	}
	generatorOffered, err := parseLogInt(scenarioLine, "generator_offered")
	if err != nil {
		return e2eRow{}, err
	}
	generatorStarted, err := parseLogInt(scenarioLine, "generator_started")
	if err != nil {
		return e2eRow{}, err
	}
	collectIntervalMS := r.cfg.EBPFCollectIntervalMS
	scrapeIntervalMS := r.cfg.EBPFScrapeIntervalMS
	rawSamples := timeSeries.RawSamples

	row := e2eRow{
		Variant:                       variant.Name,
		EffectiveLowat:                socketEffectiveLowat,
		ScenarioSocketCookie:          scenarioSocketCookie,
		SocketRawLowatBefore:          socketRawLowatBefore,
		SocketRawLowatAfter:           socketRawLowatAfter,
		SocketNetnsLowat:              socketNetnsLowat,
		SocketEffectiveLowat:          socketEffectiveLowat,
		TCPConnections:                tcpConnections,
		IsolatedRequests:              isolatedRequests,
		IsolatedRealErrors:            isolatedErrors,
		IsolatedDeadlineCanceled:      isolatedDeadlineCanceled,
		IsolatedP50Millis:             isolatedP50,
		IsolatedP95Millis:             isolatedP95,
		IsolatedP99Millis:             isolatedP99,
		IsolatedMaxMillis:             isolatedMax,
		ScenarioRequests:              scenarioRequests,
		ScenarioRealErrors:            scenarioErrors,
		ScenarioDeadlineCanceled:      scenarioDeadlineCanceled,
		ScenarioP50Millis:             scenarioP50,
		ScenarioP95Millis:             scenarioP95,
		ScenarioP99Millis:             scenarioP99,
		ScenarioMaxMillis:             scenarioMax,
		BulkBytes:                     int64(bulkBytes),
		BulkMBps:                      bulkThroughput / 1000000,
		MaxNotsentBytes:               maxNotsent,
		QdiscMaxBacklogBytes:          qdisc.MaxBacklogBytes,
		QdiscMaxBacklogPackets:        qdisc.MaxBacklogPackets,
		QdiscDropsDelta:               qdisc.DropsDelta,
		QdiscOverlimitsDelta:          qdisc.OverlimitsDelta,
		NotsentAttribution:            "socket_cookie",
		ScenarioScheduler:             scenarioScheduler,
		MaxSentUnackedBytes:           maxSentUnacked,
		MaxOutstandingBytes:           maxOutstanding,
		QdiscSampleCount:              qdisc.SampleCount,
		MaxSndWndBytes:                maxSndWnd,
		MaxSndCwndPackets:             maxSndCwnd,
		MSSCacheBytes:                 mssCache,
		PacketsOut:                    packetsOut,
		TotalRetrans:                  totalRetrans,
		GeneratorOfferedRequests:      generatorOffered,
		GeneratorStartedRequests:      generatorStarted,
		GeneratorDrops:                generatorDrops,
		EBPFCollectIntervalMS:         collectIntervalMS,
		EBPFScrapeIntervalMS:          scrapeIntervalMS,
		EBPFTimeSeriesRawSamples:      rawSamples,
		EBPFTimeSeriesScenarioSamples: scenarioSamples,
		TimeSeriesSampledMaxNotsent:   sampledMax,
		ScenarioWindowStartNS:         scenarioStart,
		ScenarioWindowEndNS:           scenarioEnd,
		TimeSeries:                    timeSeries.Rows,
	}
	if row.EBPFCollectIntervalMS != r.cfg.EBPFCollectIntervalMS || row.EBPFScrapeIntervalMS != r.cfg.EBPFScrapeIntervalMS {
		return e2eRow{}, fmt.Errorf("unexpected eBPF timing metadata for variant %s: collect=%d scrape=%d", variant.Name, row.EBPFCollectIntervalMS, row.EBPFScrapeIntervalMS)
	}
	return row, nil
}

func (c *ExistingCluster) ensureNamespace(ctx context.Context, namespace string) error {
	_, err := c.clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = c.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *ExistingCluster) deleteJobs(ctx context.Context, namespace string, jobs []string) error {
	for _, job := range jobs {
		err := c.clientset.BatchV1().Jobs(namespace).Delete(ctx, job, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s: %w", job, err)
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		allGone := true
		for _, job := range jobs {
			_, err := c.clientset.BatchV1().Jobs(namespace).Get(ctx, job, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return fmt.Errorf("check job deletion %s: %w", job, err)
			default:
				allGone = false
			}
		}
		if allGone {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for old scenario jobs to be deleted")
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (c *ExistingCluster) kubectl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", c.kubectlArgs(args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return stdout.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return stdout.String(), fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

func (c *ExistingCluster) kubectlArgs(args ...string) []string {
	if c.context == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--context", c.context)
	out = append(out, args...)
	return out
}

func (c *ExistingCluster) restartRollout(ctx context.Context, namespace, resource string) error {
	c.log.Log("restarting %s", resource)
	if _, err := c.kubectl(ctx, "rollout", "restart", resource, "-n", namespace); err != nil {
		return err
	}
	if _, err := c.kubectl(ctx, "rollout", "status", resource, "-n", namespace, "--timeout=10m"); err != nil {
		return err
	}
	return nil
}

func (c *ExistingCluster) waitRunningReadyPod(ctx context.Context, namespace, label string, timeout time.Duration) (corev1.Pod, error) {
	return c.waitRunningReadyPodWhere(ctx, namespace, label, timeout, func(corev1.Pod) bool { return true })
}

func (c *ExistingCluster) waitRunningReadyPodOnNode(ctx context.Context, namespace, label, node string, timeout time.Duration) (corev1.Pod, error) {
	return c.waitRunningReadyPodWhere(ctx, namespace, label, timeout, func(pod corev1.Pod) bool {
		return pod.Spec.NodeName == node
	})
}

func (c *ExistingCluster) waitRunningReadyPodWhere(ctx context.Context, namespace, label string, timeout time.Duration, match func(corev1.Pod) bool) (corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			return corev1.Pod{}, err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && podReady(pod) && match(pod) {
				return pod, nil
			}
		}
		if time.Now().After(deadline) {
			return corev1.Pod{}, fmt.Errorf("timed out waiting for ready pod label=%q", label)
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return corev1.Pod{}, err
		}
	}
}

func podReady(pod corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func podContainerReady(pod corev1.Pod, name string) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name && status.Ready && status.State.Running != nil {
			return true
		}
	}
	return false
}

func (c *ExistingCluster) runJob(ctx context.Context, namespace, job string, timeout time.Duration) error {
	c.log.Log("starting job %s", job)
	if _, err := c.clientset.BatchV1().Jobs(namespace).Patch(ctx, job, types.MergePatchType, []byte(`{"spec":{"suspend":false}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("unsuspend job %s: %w", job, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		current, err := c.clientset.BatchV1().Jobs(namespace).Get(ctx, job, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %s: %w", job, err)
		}
		for _, cond := range current.Status.Conditions {
			if cond.Type == "Complete" && cond.Status == corev1.ConditionTrue {
				return nil
			}
			if cond.Type == "Failed" && cond.Status == corev1.ConditionTrue {
				return fmt.Errorf("job %s failed: %s", job, cond.Message)
			}
		}
		if current.Status.Failed > 0 {
			return fmt.Errorf("job %s failed pods=%d", job, current.Status.Failed)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for job %s", job)
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (c *ExistingCluster) jobPod(ctx context.Context, namespace, job string) (corev1.Pod, error) {
	selectors := []string{
		"batch.kubernetes.io/job-name=" + job,
		"job-name=" + job,
	}
	for _, selector := range selectors {
		pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return corev1.Pod{}, err
		}
		if len(pods.Items) > 0 {
			sort.Slice(pods.Items, func(i, j int) bool {
				return pods.Items[i].CreationTimestamp.Before(&pods.Items[j].CreationTimestamp)
			})
			return pods.Items[len(pods.Items)-1], nil
		}
	}
	return corev1.Pod{}, fmt.Errorf("no pod found for job %s", job)
}

func (c *ExistingCluster) podLogs(ctx context.Context, namespace, pod, container string) (string, error) {
	req := c.clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseQdiscStatsFile(path string) (qdiscStats, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return qdiscStats{}, err
	}
	if len(data) == 0 {
		return qdiscStats{}, fmt.Errorf("qdisc sampler produced no output")
	}

	var stats qdiscStats
	var firstDrops, firstOverlimits, lastDrops, lastOverlimits int64 = -1, -1, -1, -1
	var sawBacklog bool
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "SAMPLE ") {
			stats.SampleCount++
			continue
		}
		if bytes, packets, ok := parseQdiscBacklog(line); ok {
			sawBacklog = true
			if bytes > stats.MaxBacklogBytes {
				stats.MaxBacklogBytes = bytes
			}
			if packets > stats.MaxBacklogPackets {
				stats.MaxBacklogPackets = packets
			}
		}
		if drops, overlimits, ok := parseQdiscCounters(line); ok {
			if firstDrops < 0 {
				firstDrops = int64(drops)
			}
			if firstOverlimits < 0 {
				firstOverlimits = int64(overlimits)
			}
			lastDrops = int64(drops)
			lastOverlimits = int64(overlimits)
		}
	}
	if err := scanner.Err(); err != nil {
		return qdiscStats{}, err
	}
	if stats.SampleCount == 0 {
		return qdiscStats{}, fmt.Errorf("qdisc sampler produced zero samples")
	}
	if !sawBacklog {
		return qdiscStats{}, fmt.Errorf("failed to parse tc qdisc backlog from sampler output")
	}
	if firstDrops < 0 || firstOverlimits < 0 || lastDrops < 0 || lastOverlimits < 0 {
		return qdiscStats{}, fmt.Errorf("failed to parse tc qdisc counters from sampler output")
	}
	if lastDrops < firstDrops || lastOverlimits < firstOverlimits {
		return qdiscStats{}, fmt.Errorf("tc qdisc counters moved backwards")
	}
	stats.DropsDelta = uint64(lastDrops - firstDrops)
	stats.OverlimitsDelta = uint64(lastOverlimits - firstOverlimits)
	return stats, nil
}

func parseQdiscBacklog(line string) (uint64, uint64, bool) {
	fields := strings.Fields(line)
	for i, field := range fields {
		if field != "backlog" || i+2 >= len(fields) {
			continue
		}
		bytes, err := parseQdiscUnit(fields[i+1], "b")
		if err != nil {
			return 0, 0, false
		}
		packets, err := parseQdiscUnit(fields[i+2], "p")
		if err != nil {
			return 0, 0, false
		}
		return bytes, packets, true
	}
	return 0, 0, false
}

func parseQdiscCounters(line string) (uint64, uint64, bool) {
	cleaned := strings.NewReplacer("(", "", ")", "", ",", "").Replace(line)
	fields := strings.Fields(cleaned)
	var drops, overlimits uint64
	var sawDrops, sawOverlimits bool
	for i, field := range fields {
		if field == "dropped" && i+1 < len(fields) {
			value, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			drops = value
			sawDrops = true
		}
		if field == "overlimits" && i+1 < len(fields) {
			value, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			overlimits = value
			sawOverlimits = true
		}
	}
	return drops, overlimits, sawDrops && sawOverlimits
}

func parseQdiscUnit(token, suffix string) (uint64, error) {
	if !strings.HasSuffix(token, suffix) {
		return 0, fmt.Errorf("qdisc token %q missing suffix %q", token, suffix)
	}
	return strconv.ParseUint(strings.TrimSuffix(token, suffix), 10, 64)
}

func exactlyOneLine(text string, match func(string) bool) (string, error) {
	var out string
	var count int
	for _, line := range strings.Split(text, "\n") {
		if match(line) {
			count++
			out = line
		}
	}
	if count != 1 {
		return "", fmt.Errorf("got %d matches, want 1", count)
	}
	return out, nil
}

func lastLineMatching(text string, match func(string) bool) (string, error) {
	var out string
	for _, line := range strings.Split(text, "\n") {
		if match(line) {
			out = line
		}
	}
	if out == "" {
		return "", fmt.Errorf("no matching line found")
	}
	return out, nil
}

func parseLogInt(line, name string) (int, error) {
	value := extractFieldValue(line, name)
	if value == "" {
		return 0, fmt.Errorf("missing %s in %q", name, line)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", name, value, err)
	}
	return int(n), nil
}

func parseLogFloat(line, name string) (float64, error) {
	value := extractFieldValue(line, name)
	if value == "" {
		return 0, fmt.Errorf("missing %s in %q", name, line)
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", name, value, err)
	}
	return n, nil
}

func scrapeSocketMetric(path, metric, cgroupID, socketCookie string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var matches int
	var value uint64
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, sample, err := parseMetricLine(line)
		if err != nil {
			return 0, err
		}
		if name != metric || labels["cgroup_id"] != cgroupID || labels["socket_cookie"] != socketCookie {
			continue
		}
		if sample < 0 {
			return 0, fmt.Errorf("metric %s was negative: %v", metric, sample)
		}
		matches++
		value = uint64(sample)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if matches != 1 {
		return 0, fmt.Errorf("expected exactly one %s series for cgroup_id=%s socket_cookie=%s, got %d", metric, cgroupID, socketCookie, matches)
	}
	return value, nil
}

func removeReport(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("report path is empty")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale report %q: %w", path, err)
	}
	return nil
}

func writeE2EReport(path string, report e2eReport) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("report path is empty")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func buildE2EReport(cfg e2eConfig, rows []e2eRow) e2eReport {
	report := e2eReport{
		Time: time.Now().Format(time.RFC3339Nano),
		Config: reportConfig{
			Netem:           cfg.TCNetem,
			DurationSeconds: 30,
			PingHz:          100,
			BulkStreams:     4,
			BulkBytes:       50 << 20,
			ChunkBytes:      1 << 20,
			Scheduler:       "small_first",
		},
	}
	if len(rows) > 0 {
		report.Env = reportEnv{
			Kubernetes: rows[0].KubernetesVersion,
			Kernel:     rows[0].KernelVersion,
			Go:         runtime.Version(),
		}
	}
	for _, row := range rows {
		v := row.variantReport()
		switch row.Variant {
		case "baseline":
			report.Baseline = v
		case "notsent-128k":
			report.Tuned128 = v
		case "notsent-4k":
			report.Tuned4 = v
		}
	}
	return report
}

func (r e2eRow) variantReport() variantReport {
	return variantReport{
		Lowat:           r.SocketEffectiveLowat,
		Cgroup:          r.ServerCgroupID,
		Socket:          r.ScenarioSocketCookie,
		RawLowat:        r.SocketRawLowatAfter,
		Conns:           r.TCPConnections,
		IsolatedP99MS:   r.IsolatedP99Millis,
		ScenarioP99MS:   r.ScenarioP99Millis,
		MaxNotsent:      r.MaxNotsentBytes,
		MaxSentUnacked:  r.MaxSentUnackedBytes,
		MaxOutstanding:  r.MaxOutstandingBytes,
		QdiscSamples:    r.QdiscSampleCount,
		QdiscBacklog:    r.QdiscMaxBacklogBytes,
		QdiscDrops:      r.QdiscDropsDelta,
		Requests:        r.ScenarioRequests,
		Errors:          r.ScenarioRealErrors,
		GeneratorDrops:  r.GeneratorDrops,
		BulkMBps:        r.BulkMBps,
		WindowNS:        [2]int64{r.ScenarioWindowStartNS, r.ScenarioWindowEndNS},
		SampledMax:      r.TimeSeriesSampledMaxNotsent,
		ScenarioSamples: r.EBPFTimeSeriesScenarioSamples,
		Series:          scenarioSeries(r.TimeSeries),
	}
}

func scenarioSeries(rows []timeSeriesRow) []seriesSample {
	out := make([]seriesSample, 0, len(rows))
	for _, row := range rows {
		if row.Phase != "scenario" {
			continue
		}
		out = append(out, seriesSample{
			MS:      int64(row.ElapsedMs + 0.5),
			Notsent: row.TCPNotsentBytes,
		})
	}
	return out
}

func evaluateE2EReport(report e2eReport) string {
	variants := []struct {
		name string
		row  variantReport
	}{
		{name: "baseline", row: report.Baseline},
		{name: "128k", row: report.Tuned128},
		{name: "4k", row: report.Tuned4},
	}
	expectedLowat := map[string]string{
		"baseline": "4294967295",
		"128k":     "131072",
		"4k":       "4096",
	}
	for _, variant := range variants {
		if variant.row.Socket == "" {
			return fmt.Sprintf("%s result is missing", variant.name)
		}
		if variant.row.Conns != 1 {
			return fmt.Sprintf("%s used %d scenario TCP connections", variant.name, variant.row.Conns)
		}
		if variant.row.RawLowat != "0" {
			return fmt.Sprintf("%s raw TCP_NOTSENT_LOWAT=%s", variant.name, variant.row.RawLowat)
		}
		if variant.row.Lowat != expectedLowat[variant.name] {
			return fmt.Sprintf("%s effective lowat=%s", variant.name, variant.row.Lowat)
		}
		if variant.row.QdiscSamples == 0 {
			return fmt.Sprintf("%s qdisc sampler produced zero samples", variant.name)
		}
		if variant.row.ScenarioSamples < 80 {
			return fmt.Sprintf("%s eBPF time series has %d scenario samples", variant.name, variant.row.ScenarioSamples)
		}
		if variant.row.SampledMax > variant.row.MaxNotsent {
			return fmt.Sprintf("%s sampled max %d exceeds kernel max %d", variant.name, variant.row.SampledMax, variant.row.MaxNotsent)
		}
		if variant.row.Errors != 0 {
			return fmt.Sprintf("%s reported %d real request errors", variant.name, variant.row.Errors)
		}
		if variant.row.GeneratorDrops != 0 {
			return fmt.Sprintf("%s reported %d generator drops", variant.name, variant.row.GeneratorDrops)
		}
	}

	baseline := report.Baseline
	tuned := report.Tuned128
	if baseline.ScenarioP99MS < 2*baseline.IsolatedP99MS {
		return fmt.Sprintf("baseline scenario P99 %.3fms is less than 2x isolated P99 %.3fms", baseline.ScenarioP99MS, baseline.IsolatedP99MS)
	}
	if baseline.MaxNotsent < 1<<20 {
		return fmt.Sprintf("baseline max_notsent=%d is below 1MiB", baseline.MaxNotsent)
	}
	if tuned.MaxNotsent > baseline.MaxNotsent*40/100 {
		return fmt.Sprintf("128k max_notsent=%d did not fall by at least 60%% from baseline %d", tuned.MaxNotsent, baseline.MaxNotsent)
	}
	if tuned.ScenarioP99MS > baseline.ScenarioP99MS*0.5 && tuned.ScenarioP99MS > tuned.IsolatedP99MS*1.25 {
		return fmt.Sprintf("128k scenario P99 %.3fms did not meet latency recovery gate", tuned.ScenarioP99MS)
	}
	if tuned.BulkMBps < baseline.BulkMBps*0.85 {
		return fmt.Sprintf("128k throughput %.2fMB/s is below 85%% of baseline %.2fMB/s", tuned.BulkMBps, baseline.BulkMBps)
	}
	if baseline.MaxNotsent <= tuned.MaxNotsent {
		return "baseline notsent is not larger than 128k notsent"
	}
	if baseline.ScenarioP99MS <= tuned.ScenarioP99MS {
		return "baseline scenario P99 is not worse than 128k scenario P99"
	}
	return ""
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestParseConfigUsesCanonicalPoCValues(t *testing.T) {
	cfg, err := parseConfig([]string{"poc"})
	if err != nil {
		t.Fatalf("parseConfig() default error = %v", err)
	}
	if got, want := cfg.Mode, "client"; got != want {
		t.Fatalf("Mode = %q, want %q", got, want)
	}
	if got, want := cfg.Target, localTarget; got != want {
		t.Fatalf("Target = %q, want %q", got, want)
	}
	if got, want := cfg.MetricsAddr, clientMetricsAddr; got != want {
		t.Fatalf("MetricsAddr = %q, want %q", got, want)
	}
	if got, want := cfg.Duration, scenarioDuration; got != want {
		t.Fatalf("Duration = %v, want %v", got, want)
	}
	if got, want := cfg.Rate, scenarioRate; got != want {
		t.Fatalf("Rate = %d, want %d", got, want)
	}
	if got, want := cfg.BulkBytes, scenarioBulkBytes; got != want {
		t.Fatalf("BulkBytes = %d, want %d", got, want)
	}
	if got, want := cfg.BulkStreams, scenarioBulkStreams; got != want {
		t.Fatalf("BulkStreams = %d, want %d", got, want)
	}
	if got, want := cfg.ChunkBytes, serverChunkBytes; got != want {
		t.Fatalf("ChunkBytes = %d, want %d", got, want)
	}
	if got, want := cfg.EBPFCollectInterval, ebpfCollectInterval; got != want {
		t.Fatalf("EBPFCollectInterval = %v, want %v", got, want)
	}
}

func TestParseConfigKeepsOnlyRoleAndVariantFlags(t *testing.T) {
	cfg, err := parseConfig([]string{"poc", "-mode=ebpf", "-variant=node"})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if got, want := cfg.MetricsAddr, ebpfMetricsAddr; got != want {
		t.Fatalf("ebpf MetricsAddr = %q, want %q", got, want)
	}
	if cfg.BPFObject == "" {
		t.Fatalf("BPFObject is empty")
	}

	cfg, err = parseConfig([]string{"poc", "-variant=notsent-128k"})
	if err != nil {
		t.Fatalf("parseConfig() variant error = %v", err)
	}
	if got, want := cfg.Target, "http://server-notsent-128k:8080"; got != want {
		t.Fatalf("Target = %q, want %q", got, want)
	}
}

func TestParseConfigRejectsRemovedFlags(t *testing.T) {
	for _, arg := range []string{
		"-addr=:8081",
		"-target=http://example.invalid",
		"-metrics-addr=:19090",
		"-bpf-object=/tmp/notsent.o",
		"-duration=1s",
		"-rate=1",
		"-bulk-bytes=1",
		"-bulk-streams=1",
		"-chunk-bytes=1",
		"-ebpf-collect-interval=1s",
	} {
		t.Run(arg, func(t *testing.T) {
			if _, err := parseConfig([]string{"poc", arg}); err == nil {
				t.Fatalf("parseConfig(%q) error = nil, want failure", arg)
			}
		})
	}
}

func TestParseE2EVariants(t *testing.T) {
	variants, err := parseE2EVariants("baseline,notsent-128k,notsent-4k")
	if err != nil {
		t.Fatalf("parseE2EVariants() error = %v", err)
	}
	if got, want := len(variants), 3; got != want {
		t.Fatalf("variants = %d, want %d", got, want)
	}
	if got, want := variants[1].Job, "scenario-notsent-128k"; got != want {
		t.Fatalf("variant job = %q, want %q", got, want)
	}
	if _, err := parseE2EVariants("baseline,nope"); err == nil {
		t.Fatalf("parseE2EVariants() unknown variant error = nil, want failure")
	}
}

func TestNewE2EConfigFromEnvUsesReportFile(t *testing.T) {
	t.Setenv("POC_E2E_JSON", "")
	t.Setenv("POC_VARIANT_ORDER", "")

	cfg, err := newE2EConfigFromEnv()
	if err != nil {
		t.Fatalf("newE2EConfigFromEnv() error = %v", err)
	}
	if got, want := cfg.ReportFile, defaultReportFile; got != want {
		t.Fatalf("ReportFile = %q, want %q", got, want)
	}
}

func TestBuildE2EReportIncludesTime(t *testing.T) {
	report := buildE2EReport(e2eConfig{}, nil)
	if report.Time == "" {
		t.Fatalf("report time is empty")
	}
	if _, err := time.Parse(time.RFC3339Nano, report.Time); err != nil {
		t.Fatalf("report time %q is not RFC3339Nano: %v", report.Time, err)
	}
}

func TestParseQdiscStatsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qdisc.txt")
	raw := `SAMPLE 1
qdisc netem 805c: root refcnt 17 limit 256 delay 40ms rate 100Mbit
 Sent 530 bytes 7 pkt (dropped 2, overlimits 10 requeues 0)
 backlog 74b 1p requeues 0
SAMPLE 2
qdisc netem 805c: root refcnt 17 limit 256 delay 40ms rate 100Mbit
 Sent 530 bytes 7 pkt (dropped 5, overlimits 19 requeues 0)
 backlog 1900b 7p requeues 0
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write qdisc fixture: %v", err)
	}
	stats, err := parseQdiscStatsFile(path)
	if err != nil {
		t.Fatalf("parseQdiscStatsFile() error = %v", err)
	}
	if got, want := stats.SampleCount, 2; got != want {
		t.Fatalf("sample count = %d, want %d", got, want)
	}
	if got, want := stats.MaxBacklogBytes, uint64(1900); got != want {
		t.Fatalf("max backlog bytes = %d, want %d", got, want)
	}
	if got, want := stats.MaxBacklogPackets, uint64(7); got != want {
		t.Fatalf("max backlog packets = %d, want %d", got, want)
	}
	if got, want := stats.DropsDelta, uint64(3); got != want {
		t.Fatalf("drops delta = %d, want %d", got, want)
	}
	if got, want := stats.OverlimitsDelta, uint64(9); got != want {
		t.Fatalf("overlimits delta = %d, want %d", got, want)
	}
}

func TestWriteE2EReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	report := e2eReport{
		OK: true,
		Baseline: variantReport{
			Lowat:  "4294967295",
			Socket: "123",
			Series: []seriesSample{{MS: 0, Notsent: 10}},
		},
	}
	if err := writeE2EReport(path, report); err != nil {
		t.Fatalf("writeE2EReport() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var got e2eReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if !got.OK {
		t.Fatalf("report OK = false, want true")
	}
	if got.Baseline.Series[0].Notsent != 10 {
		t.Fatalf("baseline series notsent = %d, want 10", got.Baseline.Series[0].Notsent)
	}
}

func TestEvaluateE2EReport(t *testing.T) {
	report := e2eReport{
		Baseline: variantReport{
			Lowat:           "4294967295",
			Socket:          "11",
			RawLowat:        "0",
			Conns:           1,
			IsolatedP99MS:   40,
			ScenarioP99MS:   340,
			MaxNotsent:      3 << 20,
			QdiscSamples:    500,
			ScenarioSamples: 120,
			BulkMBps:        11.8,
		},
		Tuned128: variantReport{
			Lowat:           "131072",
			Socket:          "22",
			RawLowat:        "0",
			Conns:           1,
			IsolatedP99MS:   40,
			ScenarioP99MS:   160,
			MaxNotsent:      195480,
			QdiscSamples:    500,
			ScenarioSamples: 120,
			BulkMBps:        11.8,
		},
		Tuned4: variantReport{
			Lowat:           "4096",
			Socket:          "33",
			RawLowat:        "0",
			Conns:           1,
			IsolatedP99MS:   40,
			ScenarioP99MS:   150,
			MaxNotsent:      65536,
			QdiscSamples:    500,
			ScenarioSamples: 120,
			BulkMBps:        11.8,
		},
	}
	if fail := evaluateE2EReport(report); fail != "" {
		t.Fatalf("evaluateE2EReport() fail = %q, want pass", fail)
	}

	report.Tuned128.ScenarioP99MS = 330
	if fail := evaluateE2EReport(report); fail == "" {
		t.Fatalf("evaluateE2EReport() fail = empty, want latency gate failure")
	}
}

func TestLoadExactTimeSeriesFiltersExactSocket(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "ebpf.raw")
	clientLog := filepath.Join(dir, "client.log")

	raw := `SAMPLE 100
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 10
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 20
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 30
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 40
tcpqueue_tcp_notsent_bytes{cgroup_id="333",socket_cookie="444",pid="1",comm="poc",variant="node"} 999
END_SAMPLE
SAMPLE 200
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 11
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 21
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 31
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 41
END_SAMPLE
SAMPLE 300
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 12
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 22
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 32
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 42
END_SAMPLE
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw file: %v", err)
	}
	client := `phase=scenario_window event=start variant=baseline unix_ns=150
phase=scenario_window event=end variant=baseline unix_ns=250
`
	if err := os.WriteFile(clientLog, []byte(client), 0o600); err != nil {
		t.Fatalf("write client log: %v", err)
	}

	start, end, err := parseScenarioWindow(clientLog, "baseline")
	if err != nil {
		t.Fatalf("parseScenarioWindow() error = %v", err)
	}
	if got, want := start, int64(150); got != want {
		t.Fatalf("scenario start = %d, want %d", got, want)
	}
	if got, want := end, int64(250); got != want {
		t.Fatalf("scenario end = %d, want %d", got, want)
	}

	exact, err := loadExactTimeSeries(rawPath, "111", "222", "17", "baseline", 150, 250)
	if err != nil {
		t.Fatalf("loadExactTimeSeries() error = %v", err)
	}
	if got, want := exact.RawSamples, 3; got != want {
		t.Fatalf("raw samples = %d, want %d", got, want)
	}
	if got, want := exact.ScenarioSamples, 1; got != want {
		t.Fatalf("scenario samples = %d, want %d", got, want)
	}
	if got, want := exact.SampledMaxNotsent, uint64(12); got != want {
		t.Fatalf("sampled max notsent = %d, want %d", got, want)
	}
	if len(exact.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(exact.Rows))
	}
	if exact.Rows[0].Phase != "pre_scenario" || exact.Rows[1].Phase != "scenario" || exact.Rows[2].Phase != "post_scenario" {
		t.Fatalf("unexpected phases: %+v", []string{exact.Rows[0].Phase, exact.Rows[1].Phase, exact.Rows[2].Phase})
	}
}

func TestLoadExactTimeSeriesMissingSocketFails(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "ebpf.raw")
	clientLog := filepath.Join(dir, "client.log")

	raw := `SAMPLE 100
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 10
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 20
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 30
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 40
END_SAMPLE
`
	client := `phase=scenario_window event=start variant=baseline unix_ns=100
phase=scenario_window event=end variant=baseline unix_ns=200
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw file: %v", err)
	}
	if err := os.WriteFile(clientLog, []byte(client), 0o600); err != nil {
		t.Fatalf("write client log: %v", err)
	}

	if _, err := loadExactTimeSeries(rawPath, "999", "888", "17", "baseline", 100, 200); err == nil {
		t.Fatalf("loadExactTimeSeries() missing socket error = nil, want failure")
	}
}

func TestLoadExactTimeSeriesRequiresOnlyNotsentForSamples(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "ebpf.raw")
	raw := `SAMPLE 100
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 10
END_SAMPLE
SAMPLE 200
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 20
END_SAMPLE
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw file: %v", err)
	}

	exact, err := loadExactTimeSeries(rawPath, "111", "222", "", "baseline", 50, 250)
	if err != nil {
		t.Fatalf("loadExactTimeSeries() error = %v", err)
	}
	if got, want := exact.RawSamples, 2; got != want {
		t.Fatalf("raw samples = %d, want %d", got, want)
	}
	if got, want := exact.ScenarioSamples, 2; got != want {
		t.Fatalf("scenario samples = %d, want %d", got, want)
	}
	if got, want := exact.SampledMaxNotsent, uint64(20); got != want {
		t.Fatalf("sampled max notsent = %d, want %d", got, want)
	}
}

func TestLoadExactTimeSeriesBackwardsTimestampsFail(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "ebpf.raw")
	clientLog := filepath.Join(dir, "client.log")

	raw := `SAMPLE 200
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 10
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 20
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 30
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 40
END_SAMPLE
SAMPLE 100
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 11
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 21
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 31
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 41
END_SAMPLE
`
	client := `phase=scenario_window event=start variant=baseline unix_ns=100
phase=scenario_window event=end variant=baseline unix_ns=200
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw file: %v", err)
	}
	if err := os.WriteFile(clientLog, []byte(client), 0o600); err != nil {
		t.Fatalf("write client log: %v", err)
	}

	if _, err := loadExactTimeSeries(rawPath, "111", "222", "17", "baseline", 100, 200); err == nil {
		t.Fatalf("loadExactTimeSeries() backwards timestamps error = nil, want failure")
	}
}

func TestLoadExactTimeSeriesIgnoresTrailingIncompleteSample(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "ebpf.raw")
	raw := `SAMPLE 100
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 10
tcpqueue_tcp_notsent_bytes_max{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 20
tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 30
tcpqueue_tcp_outstanding_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 40
END_SAMPLE
SAMPLE 200
tcpqueue_tcp_notsent_bytes{cgroup_id="111",socket_cookie="222",pid="1",comm="poc",variant="node"} 11
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write raw file: %v", err)
	}
	exact, err := loadExactTimeSeries(rawPath, "111", "222", "", "baseline", 50, 150)
	if err != nil {
		t.Fatalf("loadExactTimeSeries() error = %v", err)
	}
	if got, want := exact.RawSamples, 1; got != want {
		t.Fatalf("raw samples = %d, want %d", got, want)
	}
}

func TestScenarioSeriesFiltersScenarioRows(t *testing.T) {
	rows := []timeSeriesRow{
		{ElapsedMs: -100, Phase: "pre_scenario", TCPNotsentBytes: 1},
		{ElapsedMs: 100.4, Phase: "scenario", TCPNotsentBytes: 10},
		{ElapsedMs: 400, Phase: "post_scenario", TCPNotsentBytes: 99},
	}

	series := scenarioSeries(rows)
	if len(series) != 1 {
		t.Fatalf("series rows = %d, want 1", len(series))
	}
	if got, want := series[0].MS, int64(100); got != want {
		t.Fatalf("series ms = %d, want %d", got, want)
	}
	if got, want := series[0].Notsent, uint64(10); got != want {
		t.Fatalf("series notsent = %d, want %d", got, want)
	}
}

func TestParseMetricLineHandlesSpacesInLabels(t *testing.T) {
	line := `tcpqueue_tcp_inflight_or_unacked_bytes{cgroup_id="3272",comm="Socket Thread",pid="3873",socket_cookie="228880",variant="node"} 39`

	metricName, labels, value, err := parseMetricLine(line)
	if err != nil {
		t.Fatalf("parseMetricLine() error = %v", err)
	}
	if got, want := metricName, "tcpqueue_tcp_inflight_or_unacked_bytes"; got != want {
		t.Fatalf("metric name = %q, want %q", got, want)
	}
	if got, want := labels["comm"], "Socket Thread"; got != want {
		t.Fatalf("comm label = %q, want %q", got, want)
	}
	if got, want := labels["socket_cookie"], "228880"; got != want {
		t.Fatalf("socket_cookie label = %q, want %q", got, want)
	}
	if got, want := value, float64(39); got != want {
		t.Fatalf("metric value = %v, want %v", got, want)
	}
}

func TestFIFOQueuePreservesOrder(t *testing.T) {
	var q fifo[int]
	q.push(1)
	q.push(2)
	q.push(3)

	for i, want := range []int{1, 2, 3} {
		got, ok := q.pop()
		if !ok {
			t.Fatalf("pop %d: queue emptied early", i)
		}
		if got != want {
			t.Fatalf("pop %d: got %d, want %d", i, got, want)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatalf("expected queue to be empty")
	}
}

func TestFirstProgressingChoicePrefersControl(t *testing.T) {
	choice, ok := firstProgressingChoice([]frameChoice{
		{streamID: 11, dataSize: 4096, orderRank: 1},
		{streamID: 0, control: true, dataSize: 0, orderRank: 0},
	}, nil)
	if !ok {
		t.Fatalf("expected a choice")
	}
	if choice.streamID != 0 || !choice.control {
		t.Fatalf("expected control frame first, got %+v", choice)
	}
}

func TestFirstProgressingChoicePrefersSmallerData(t *testing.T) {
	choice, ok := firstProgressingChoice([]frameChoice{
		{streamID: 11, dataSize: 4096, orderRank: 1},
		{streamID: 13, dataSize: 16, orderRank: 0},
	}, nil)
	if !ok {
		t.Fatalf("expected a choice")
	}
	if choice.streamID != 13 {
		t.Fatalf("expected smaller DATA frame first, got stream %d", choice.streamID)
	}
}

func TestFirstProgressingChoiceUsesRoundRobinOnTies(t *testing.T) {
	first, ok := firstProgressingChoice([]frameChoice{
		{streamID: 11, dataSize: 32, orderRank: 0},
		{streamID: 13, dataSize: 32, orderRank: 1},
	}, nil)
	if !ok {
		t.Fatalf("expected a choice")
	}
	second, ok := firstProgressingChoice([]frameChoice{
		{streamID: 11, dataSize: 32, orderRank: 1},
		{streamID: 13, dataSize: 32, orderRank: 0},
	}, nil)
	if !ok {
		t.Fatalf("expected a second choice")
	}
	if first.streamID == second.streamID {
		t.Fatalf("expected tie-break rotation, got stream %d twice", first.streamID)
	}
}

func TestFirstProgressingChoiceSkipsBlockedCandidate(t *testing.T) {
	choice, ok := firstProgressingChoice([]frameChoice{
		{streamID: 11, dataSize: 16, orderRank: 0},
		{streamID: 13, dataSize: 32, orderRank: 1},
	}, map[uint32]bool{11: true})
	if !ok {
		t.Fatalf("expected an unblocked choice")
	}
	if choice.streamID != 13 {
		t.Fatalf("expected blocked stream to be skipped, got stream %d", choice.streamID)
	}
}

func TestRemoveStreamIDAdjustsTie(t *testing.T) {
	order, nextTie, ok := removeStreamID([]uint32{1, 2, 3, 4}, 2, 2)
	if !ok {
		t.Fatalf("expected removal to succeed")
	}
	if nextTie != 1 {
		t.Fatalf("expected nextTie=1 after removal, got %d", nextTie)
	}
	want := []uint32{1, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("got order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got order %v, want %v", order, want)
		}
	}
}
