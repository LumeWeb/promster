package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"text/template"
	"time"

	_ "embed"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	etcdregistry "go.lumeweb.com/etcd-registry"
)

//go:embed prometheus.yml.tmpl
var prometheusTemplate string

const (
	PROM_TEMPLATE_FILE = "prometheus.yml.tmpl"
	PROM_CONFIG_FILE   = "/prometheus.yml"
)

type BasicAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type ServiceGroup struct {
	Name        string     `json:"name"`
	Targets     []string   `json:"targets"`
	BasicAuth   *BasicAuth `json:"basic_auth,omitempty"`
	MetricsPath string     `json:"metrics_path"`
	NodeID      string     `json:"node_id"`
}

type PrometheusConfig struct {
	ServiceGroups      []ServiceGroup `json:"service_groups"`
	ScrapeInterval     string         `json:"scrape_interval"`
	ScrapeTimeout      string         `json:"scrape_timeout"`
	EvaluationInterval string         `json:"evaluation_interval"`
	Scheme             string         `json:"scheme"`
	TlsInsecure        string         `json:"tls_insecure"`
	AdminUsername      string         `json:"admin_username,omitempty"`
	AdminPassword      string         `json:"admin_password,omitempty"`
}

func (sg *ServiceGroup) Validate() error {
	if sg.Name == "" {
		return fmt.Errorf("service group name cannot be empty")
	}
	if len(sg.Targets) == 0 {
		return fmt.Errorf("service group %s must have at least one target", sg.Name)
	}
	if sg.BasicAuth != nil {
		if sg.BasicAuth.Username == "" {
			return fmt.Errorf("service group %s basic auth username cannot be empty", sg.Name)
		}
		if sg.BasicAuth.Password == "" {
			return fmt.Errorf("service group %s basic auth password cannot be empty", sg.Name)
		}
	}
	return nil
}

func executeTemplate(data interface{}) (string, error) {
	// Validate service groups before template execution
	config, ok := data.(PrometheusConfig)
	if ok {
		for _, sg := range config.ServiceGroups {
			if err := sg.Validate(); err != nil {
				return "", fmt.Errorf("invalid service group configuration: %w", err)
			}
		}
	}
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


func parseAddress(address string) (host string, metricsPath string) {
	// Default metrics path
	metricsPath = "/metrics"
	
	// Remove scheme (http:// or https://)
	if strings.Contains(address, "://") {
		parts := strings.SplitN(address, "://", 2)
		address = parts[1]
	}
	
	// Extract path if exists
	if strings.Contains(address, "/") {
		parts := strings.SplitN(address, "/", 2)
		host = parts[0]
		if len(parts) > 1 {
			metricsPath = "/" + parts[1]
		}
	} else {
		host = address
	}
	
	return host, metricsPath
}

func getScrapeTargets(registry *etcdregistry.EtcdRegistry, scrapeEtcdPaths []string) []ServiceGroup {
	// Map to group targets by service name and auth
	groupMap := make(map[string]*ServiceGroup)

	for _, path := range scrapeEtcdPaths {
		nodes, err := registry.GetServiceNodes(path)
		if err != nil {
			logrus.Warnf("Failed to get service nodes for path %s: %v", path, err)
			continue
		}

		for _, node := range nodes {
			serviceName := path
			address, metricsPath := parseAddress(node.Info["address"])

			// Create auth key for grouping
			authKey := ""
			if password, ok := node.Info["password"]; ok {
				username := node.Info["username"]
				if username == "" {
					username = "prometheus" // default username
				}
				authKey = username + ":" + password
			}

			// Create group key
			groupKey := serviceName + "|" + authKey

			group, exists := groupMap[groupKey]
			if !exists {
				group = &ServiceGroup{
					Name:        serviceName,
					Targets:     make([]string, 0),
					MetricsPath: metricsPath,
					NodeID:      node.Name,
				}

				// Only set auth if credentials exist
				if authKey != "" {
					username := node.Info["username"]
					if username == "" {
						username = "prometheus"
					}
					group.BasicAuth = &BasicAuth{
						Username: username,
						Password: node.Info["password"],
					}
				}

				groupMap[groupKey] = group
			}

			group.Targets = append(group.Targets, address)
		}
	}

	// Convert map to slice
	groups := make([]ServiceGroup, 0, len(groupMap))
	for _, group := range groupMap {
		groups = append(groups, *group)
	}

	return groups
}

func createPrometheusConfig(serviceGroups []ServiceGroup) PrometheusConfig {
	return PrometheusConfig{
		ServiceGroups:      serviceGroups,
		ScrapeInterval:     scrapeInterval,
		ScrapeTimeout:      scrapeTimeout,
		EvaluationInterval: evaluationInterval,
		Scheme:             scheme,
		TlsInsecure:        tlsInsecure,
		AdminUsername:      os.Getenv("PROMETHEUS_ADMIN_USERNAME"),
		AdminPassword:      os.Getenv("PROMETHEUS_ADMIN_PASSWORD"),
	}
}

func updatePrometheusConfig(configFile string, serviceGroups []ServiceGroup) error {
	config := createPrometheusConfig(serviceGroups)
	contents, err := executeTemplate(config)
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	if err := writeConfigFile(configFile, []byte(contents)); err != nil {
		return err
	}

	return reloadPrometheus()
}

var (
	scrapeInterval     string
	scrapeTimeout      string
	evaluationInterval string
	scheme             string
	tlsInsecure        string
	configFile         string
)

func monitorTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry, scrapeEtcdPaths []string) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var (
		previousGroups []ServiceGroup
		targetsMu      sync.RWMutex
	)

	for {
		select {
		case <-ticker.C:
			serviceGroups := getScrapeTargets(registry, scrapeEtcdPaths)

			targetsMu.Lock()
			if !reflect.DeepEqual(serviceGroups, previousGroups) {
				logrus.WithFields(logrus.Fields{
					"num_groups": len(serviceGroups),
					"groups":     serviceGroups,
				}).Info("Service groups changed, updating configuration")

				if err := updatePrometheusConfig(configFile, serviceGroups); err != nil {
					logrus.WithError(err).Error("Failed to update prometheus config")
				} else {
					logrus.Info("Successfully updated prometheus configuration")
					previousGroups = serviceGroups
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
	scrapeInterval = cmd.String("scrape-interval")
	scrapeTimeout = cmd.String("scrape-timeout")
	evaluationInterval = cmd.String("evaluation-interval")
	scheme = cmd.String("scheme")
	tlsInsecure = cmd.String("tls-insecure")
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

	configFile = os.Getenv("PROMETHEUS_CONFIG_FILE")
	if configFile == "" {
		configFile = PROM_CONFIG_FILE
	}

	// Verify admin credentials are set
	if os.Getenv("PROMETHEUS_ADMIN_USERNAME") == "" || os.Getenv("PROMETHEUS_ADMIN_PASSWORD") == "" {
		return fmt.Errorf("PROMETHEUS_ADMIN_USERNAME and PROMETHEUS_ADMIN_PASSWORD must be set")
	}

	if err := updatePrometheusConfig(configFile, []ServiceGroup{}); err != nil {
		return fmt.Errorf("failed to update initial prometheus config: %w", err)
	}

	rulesFile := os.Getenv("PROMETHEUS_RULES_FILE")
	if rulesFile == "" {
		rulesFile = "/rules.yml"
	}
	
	if err := createRulesFromENV(rulesFile); err != nil {
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
