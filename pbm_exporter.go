package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace   = "pbm"
	execTimeout = 5 * time.Second
)

type PBMBackup struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	RestoreTo  int64  `json:"restoreTo"`
	Size       int64  `json:"size"`
	PBMVersion string `json:"pbmVersion"`
	Type       string `json:"type"`
}

type Exporter struct {
	mutex  sync.Mutex
	logger log.Logger

	totalBackups *prometheus.Desc
	lastSize     *prometheus.Desc
	lastFinish   *prometheus.Desc
	lastRestore  *prometheus.Desc
	agentStatus  *prometheus.Desc
	backupInfo   *prometheus.Desc
	errorsTotal  prometheus.Counter
}

func NewExporter(logger log.Logger) *Exporter {
	e := &Exporter{
		logger: logger,
		totalBackups: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "backup", "total"),
			"Total number of backups.", nil, nil),
		lastSize: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "backup", "last_size_bytes"),
			"Size of the last backup.", nil, nil),
		lastFinish: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "backup", "last_timestamp_seconds"),
			"Unix timestamp of last finished backup.", nil, nil),
		lastRestore: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "backup", "last_restore_to_timestamp_seconds"),
			"RestoreTo timestamp of last backup.", nil, nil),
		agentStatus: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "agent", "status"),
			"PBM agent status (1 = running).", nil, nil),
		backupInfo: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "backup", "info"),
			"Metadata about last backup.", []string{"name", "status", "type", "version"}, nil),
		errorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_errors_total",
			Help:      "Total errors encountered by exporter.",
		}),
	}
	prometheus.MustRegister(e.errorsTotal)
	return e
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.totalBackups
	ch <- e.lastSize
	ch <- e.lastFinish
	ch <- e.lastRestore
	ch <- e.agentStatus
	ch <- e.backupInfo
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	backups, err := fetchBackups()
	if err != nil || len(backups) == 0 {
		e.errorsTotal.Inc()
		level.Error(e.logger).Log("msg", "cannot fetch backups", "err", err)
		ch <- prometheus.MustNewConstMetric(e.agentStatus, prometheus.GaugeValue, 0)
		return
	}
	last := backups[len(backups)-1]
	finish, size, err := describeBackup(last.Name)
	if err != nil {
		e.errorsTotal.Inc()
		level.Error(e.logger).Log("msg", "cannot describe backup", "err", err)
		ch <- prometheus.MustNewConstMetric(e.agentStatus, prometheus.GaugeValue, 0)
		return
	}

	ch <- prometheus.MustNewConstMetric(e.totalBackups, prometheus.CounterValue, float64(len(backups)))
	ch <- prometheus.MustNewConstMetric(e.lastSize, prometheus.GaugeValue, float64(size))
	ch <- prometheus.MustNewConstMetric(e.lastFinish, prometheus.GaugeValue, float64(finish))
	ch <- prometheus.MustNewConstMetric(e.lastRestore, prometheus.GaugeValue, float64(last.RestoreTo))
	ch <- prometheus.MustNewConstMetric(e.backupInfo, prometheus.GaugeValue, 1, last.Name, last.Status, last.Type, last.PBMVersion)

	status := 0.0
	if ok, _ := checkAgent(); ok {
		status = 1.0
	}
	ch <- prometheus.MustNewConstMetric(e.agentStatus, prometheus.GaugeValue, status)
}

func fetchBackups() ([]PBMBackup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pbm", "list", "--out=json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Snapshots []PBMBackup `json:"snapshots"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	return parsed.Snapshots, nil
}

func describeBackup(name string) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pbm", "describe-backup", name, "--out=json")
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	var parsed struct {
		FinishTime int64 `json:"last_transition_ts"`
		Size       int64 `json:"size"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, 0, err
	}
	return parsed.FinishTime, parsed.Size, nil
}

func checkAgent() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pbm", "status", "--out=json")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	var parsed struct {
		Cluster []struct {
			Nodes []struct {
				Ok bool `json:"ok"`
			} `json:"nodes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return false, err
	}
	for _, c := range parsed.Cluster {
		for _, n := range c.Nodes {
			if n.Ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func main() {
	webConfig := webflag.AddFlags(kingpin.CommandLine, ":9000")
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("pbm_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := promlog.New(promlogConfig)
	prometheus.MustRegister(NewExporter(logger))
	prometheus.MustRegister(version.NewCollector("pbm_exporter"))

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>PBM Exporter</title></head><body><h1>PBM Exporter</h1><p><a href='/metrics'>Metrics</a></p></body></html>`))
	})

	srv := &http.Server{}
	if err := web.ListenAndServe(srv, webConfig, logger); err != nil {
		level.Error(logger).Log("msg", "Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}
