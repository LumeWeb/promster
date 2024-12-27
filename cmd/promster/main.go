package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	etcdregistry "go.lumeweb.com/etcd-registry"
	_ "embed"
)

//go:embed prometheus.yml.tmpl
var prometheusTemplate string

const (
	PROM_TEMPLATE_FILE = "prometheus.yml.tmpl"
	PROM_CONFIG_FILE   = "/prometheus.yml"
)

type BasicAuth struct {
	Password string `json:"password,omitempty"`
}

type SourceTarget struct {
	Targets   []string          `json:"targets"`
	Labels    map[string]string `json:"labels,omitempty"`
	BasicAuth *BasicAuth        `json:"basic_auth,omitempty"`
}

type RecordingRule struct {
	name   string
	expr   string
	labels map[string]string
}

func executeTemplate(data interface{}) (string, error) {
	tmpl, err := template.New(PROM_TEMPLATE_FILE).Parse(prometheusTemplate)
	if err != nil {
		return "", err
	}

	var result strings.Builder
	err = tmpl.Execute(&result, data)
	if err != nil {
		return "", err
	}

	return result.String(), nil
}

func writeConfigFile(filename string, data []byte) error {
	tempFile := filename + ".tmp"
	if err := os.WriteFile(tempFile, data, 0666); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tempFile, filename); err != nil {
		os.Remove(tempFile) // Clean up temp file
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func reloadPrometheus() error {
	adminUsername := os.Getenv("PROMETHEUS_ADMIN_USERNAME")
	adminPassword := os.Getenv("PROMETHEUS_ADMIN_PASSWORD")
	if adminUsername != "" && adminPassword != "" {
		req, err := http.NewRequest("POST", "http://localhost:9090/-/reload", nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.SetBasicAuth(adminUsername, adminPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to reload prometheus config: %w", err)
		}
		defer resp.Body.Close()
		return nil
	} else {
		return fmt.Errorf("PROMETHEUS_ADMIN_USERNAME and PROMETHEUS_ADMIN_PASSWORD must be set")
	}
}

func getScrapeTargets(registry *etcdregistry.EtcdRegistry, scrapeEtcdPaths []string) []SourceTarget {
	targets := make([]SourceTarget, 0)
	for _, path := range scrapeEtcdPaths {
		nodes, err := registry.GetServiceNodes(path)
		if err != nil {
			logrus.Warnf("Failed to get service nodes for path %s: %v", path, err)
			continue
		}

		for _, node := range nodes {
			target := SourceTarget{
				Labels:  map[string]string{"prsn": node.Name},
				Targets: []string{node.Info["address"]},
			}

			if password, ok := node.Info["password"]; ok {
				target.BasicAuth = &BasicAuth{
					Password: password,
				}
			}

			targets = append(targets, target)
		}
	}
	return targets
}

func areScrapeTargetsEqual(a, b []SourceTarget) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !areSourceTargetsEqual(a[i], b[i]) {
			return false
		}
	}

	return true
}

func areSourceTargetsEqual(a, b SourceTarget) bool {
	if len(a.Targets) != len(b.Targets) {
		return false
	}

	for i := range a.Targets {
		if a.Targets[i] != b.Targets[i] {
			return false
		}
	}

	if len(a.Labels) != len(b.Labels) {
		return false
	}

	for k, v := range a.Labels {
		if b.Labels[k] != v {
			return false
		}
	}

	if (a.BasicAuth == nil) != (b.BasicAuth == nil) {
		return false
	}
	if a.BasicAuth != nil && b.BasicAuth != nil {
		if a.BasicAuth.Password != b.BasicAuth.Password {
			return false
		}
	}

	return true
}

func updatePrometheusConfig(configFile string, config map[string]interface{}) error {
	contents, err := executeTemplate(config)
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	if err := writeConfigFile(configFile, []byte(contents)); err != nil {
		return err
	}

	return reloadPrometheus()
}

func updatePrometheusTargets(scrapeTargets []SourceTarget) error {
	contents, err := json.Marshal(scrapeTargets)
	if err != nil {
		return fmt.Errorf("failed to marshal targets: %w", err)
	}

	if err := writeConfigFile("/servers.json", contents); err != nil {
		return err
	}

	return reloadPrometheus()
}

func getLabelMap(rawLabels string) map[string]string {
	toReturn := make(map[string]string)
	mappings := strings.Split(rawLabels, ",")
	for _, mapping := range mappings {
		if mapping != "" {
			var keyValue = strings.Split(mapping, ":")
			toReturn[keyValue[0]] = keyValue[1]
		}
	}

	return toReturn
}

func getPrintableLabels(labels map[string]string) string {
	if len(labels) <= 0 {
		return ""
	}

	var toReturn = `
      labels:`
	for k, v := range labels {
		var format = `
        %s: %s`
		toReturn += fmt.Sprintf(format, k, v)
	}
	return toReturn
}

func createRulesFromENV(rulesFile string) error {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		pair := strings.Split(e, "=")
		env[pair[0]] = pair[1]
	}
	rules := make([]RecordingRule, 0)
	for i := 1; i < 100; i++ {
		kname := fmt.Sprintf("RECORD_RULE_%d_NAME", i)
		kexpr := fmt.Sprintf("RECORD_RULE_%d_EXPR", i)
		klabels := fmt.Sprintf("RECORD_RULE_%d_LABELS", i)

		vname, exists := env[kname]
		if !exists {
			break
		}
		vexpr, exists := env[kexpr]
		if !exists {
			break
		}

		rules = append(rules, RecordingRule{name: vname, expr: vexpr, labels: getLabelMap(env[klabels])})
	}

	if len(rules) == 0 {
		logrus.Infof("No prometheus rules found in environment variables")
		return nil
	}

	rulesContents := `groups:
  - name: env-rules
    rules:`

	for _, v := range rules {
		rc := `%s
    - record: %s
      expr: %s
%s
`
		rulesContents = fmt.Sprintf(rc, rulesContents, v.name, v.expr, getPrintableLabels(v.labels))
	}

	if err := writeConfigFile(rulesFile, []byte(rulesContents)); err != nil {
		return err
	}

	return reloadPrometheus()
}

func monitorTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry, scrapeEtcdPaths []string) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var (
		previousTargets []SourceTarget
		targetsMu       sync.RWMutex
	)

	for {
		select {
		case <-ticker.C:
			targets := getScrapeTargets(registry, scrapeEtcdPaths)

			targetsMu.Lock()
			if !areScrapeTargetsEqual(targets, previousTargets) {
				if err := updatePrometheusTargets(targets); err != nil {
					logrus.WithError(err).Error("Failed to update prometheus targets")
				} else {
					previousTargets = targets
				}
			}
			targetsMu.Unlock()

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	// Set log level
	switch cmd.String("loglevel") {
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
	case "warning":
		logrus.SetLevel(logrus.WarnLevel)
	case "error":
		logrus.SetLevel(logrus.ErrorLevel)
	default:
		logrus.SetLevel(logrus.InfoLevel)
	}

	// Get flag values
	etcdURLScrape := cmd.String("scrape-etcd-url")
	etcdBasePath := cmd.String("etcd-base-path")
	scrapeEtcdPaths := strings.Split(cmd.String("scrape-etcd-paths"), ",")
	scrapeInterval := cmd.String("scrape-interval")
	scrapeTimeout := cmd.String("scrape-timeout")
	evaluationInterval := cmd.String("evaluation-interval")
	scheme := cmd.String("scheme")
	tlsInsecure := cmd.String("tls-insecure")
	etcdUsername := cmd.String("etcd-username")
	etcdPassword := cmd.String("etcd-password")
	etcdTimeout := cmd.Duration("etcd-timeout")

	logrus.Info("====Starting Promster====")
	time.Sleep(5 * time.Second)

	registry, err := etcdregistry.NewEtcdRegistry(
		strings.Split(etcdURLScrape, ","),
		etcdBasePath,
		etcdUsername,
		etcdPassword,
		etcdTimeout,
	)
	if err != nil {
		return fmt.Errorf("failed to create etcd registry: %w", err)
	}

	// Create initial prometheus config without scrape paths
	config := map[string]interface{}{
		"scrapeInterval":     scrapeInterval,
		"scrapeTimeout":      scrapeTimeout,
		"evaluationInterval": evaluationInterval,
		"scheme":             scheme,
		"tlsInsecure":        tlsInsecure,
	}

	configFile := os.Getenv("PROMETHEUS_CONFIG_FILE")
	if configFile == "" {
		configFile = PROM_CONFIG_FILE
	}

	if err := updatePrometheusConfig(configFile, config); err != nil {
		return fmt.Errorf("failed to update initial prometheus config: %w", err)
	}

	if err := createRulesFromENV("/rules.yml"); err != nil {
		return fmt.Errorf("failed to create rules: %w", err)
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	return monitorTargets(ctx, registry, scrapeEtcdPaths)
}

func main() {
	cmd := &cli.Command{
		Name:  "promster",
		Usage: "Prometheus cluster manager with etcd integration",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "loglevel",
				Value:   "info",
				Usage:   "debug, info, warning, error",
				Sources: cli.EnvVars("PROMSTER_LOG_LEVEL"),
			},
			&cli.StringFlag{
				Name:     "scrape-etcd-url",
				Usage:    "ETCD URLs for scrape source server",
				Required: true,
				Sources:  cli.EnvVars("PROMSTER_SCRAPE_ETCD_URL"),
			},
			&cli.StringFlag{
				Name:     "etcd-base-path",
				Usage:    "Base ETCD path for the registry",
				Required: true,
				Sources:  cli.EnvVars("PROMSTER_ETCD_BASE_PATH"),
			},
			&cli.StringFlag{
				Name:     "scrape-etcd-paths",
				Usage:    "Comma-separated list of base ETCD paths for getting servers to be scrapped",
				Required: true,
				Sources:  cli.EnvVars("PROMSTER_SCRAPE_ETCD_PATHS"),
			},
			&cli.StringFlag{
				Name:    "scrape-interval",
				Value:   "30s",
				Usage:   "Prometheus scrape interval",
				Sources: cli.EnvVars("PROMSTER_SCRAPE_INTERVAL"),
			},
			&cli.StringFlag{
				Name:    "scrape-timeout",
				Value:   "30s",
				Usage:   "Prometheus scrape timeout",
				Sources: cli.EnvVars("PROMSTER_SCRAPE_TIMEOUT"),
			},
			&cli.StringFlag{
				Name:    "evaluation-interval",
				Value:   "30s",
				Usage:   "Prometheus evaluation interval",
				Sources: cli.EnvVars("PROMSTER_EVALUATION_INTERVAL"),
			},
			&cli.StringFlag{
				Name:    "scheme",
				Value:   "http",
				Usage:   "Scrape scheme, either http or https",
				Sources: cli.EnvVars("PROMSTER_SCHEME"),
			},
			&cli.StringFlag{
				Name:    "tls-insecure",
				Value:   "false",
				Usage:   "Disable validation of the server certificate. true or false",
				Sources: cli.EnvVars("PROMSTER_TLS_INSECURE"),
			},
			&cli.StringFlag{
				Name:    "etcd-username",
				Usage:   "ETCD username",
				Sources: cli.EnvVars("PROMSTER_ETCD_USERNAME"),
			},
			&cli.StringFlag{
				Name:    "etcd-password",
				Usage:   "ETCD password",
				Sources: cli.EnvVars("PROMSTER_ETCD_PASSWORD"),
			},
			&cli.DurationFlag{
				Name:    "etcd-timeout",
				Value:   30 * time.Second,
				Usage:   "ETCD timeout",
				Sources: cli.EnvVars("PROMSTER_ETCD_TIMEOUT"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		logrus.Fatal(err)
	}
}
