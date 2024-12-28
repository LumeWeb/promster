package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"text/template"
	"time"

	_ "embed"
	"errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v2"
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
	Name        string            `json:"name"`
	Targets     []string          `json:"targets"`
	BasicAuth   *BasicAuth        `json:"basic_auth,omitempty"`
	MetricsPath string            `json:"metrics_path"`
	NodeID      string            `json:"node_id"`
	Labels      map[string]string `json:"labels,omitempty"`
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
		err := os.Remove(tempFile)
		if err != nil {
			return fmt.Errorf("failed to remove temp file: %w", err)
		} // Clean up temp file
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func reloadPrometheus() error {
	adminUsername := os.Getenv("PROMETHEUS_ADMIN_USERNAME")
	adminPassword := os.Getenv("PROMETHEUS_ADMIN_PASSWORD")
	if adminUsername != "" && adminPassword != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:9090/-/reload", nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.SetBasicAuth(adminUsername, adminPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to reload prometheus config: %w", err)
		}
		defer func(Body io.ReadCloser) {
			err := Body.Close()
			if err != nil {
				logrus.Errorf("Failed to close response body: %v", err)
			}
		}(resp.Body)
		return nil
	} else {
		return fmt.Errorf("PROMETHEUS_ADMIN_USERNAME and PROMETHEUS_ADMIN_PASSWORD must be set")
	}
}

func getScrapeTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry) ([]ServiceGroup, error) {
	// Get all service groups
	services, err := registry.GetServiceGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get service groups: %w", err)
	}

	var groups []ServiceGroup

	// For each service, get its group and nodes
	for _, serviceName := range services {
		group, err := registry.GetServiceGroup(ctx, serviceName)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to get service group %s", serviceName)
			continue
		}

		nodes, err := group.GetNodes(ctx)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to get nodes for service %s", serviceName)
			continue
		}

		// Create a ServiceGroup for each node
		for _, node := range nodes {
			address := fmt.Sprintf("%s:%d", node.ID, node.Port)
			if address == "" {
				logrus.Warnf("Invalid address for node %s in service %s", node.ID, serviceName)
				continue
			}

			metricsPath := node.MetricsPath
			if metricsPath == "" {
				metricsPath = "/metrics"
			}

			// Merge group common labels with node labels
			labels := make(map[string]string)
			for k, v := range group.Spec.CommonLabels {
				labels[k] = v
			}
			for k, v := range node.Labels {
				labels[k] = v
			}

			sg := ServiceGroup{
				Name:        serviceName,
				Targets:     []string{address},
				MetricsPath: metricsPath,
				NodeID:      node.ID,
				Labels:      labels,
			}

			// Use auth from group spec if available
			if group.Spec.Username != "" && group.Spec.Password != "" {
				sg.BasicAuth = &BasicAuth{
					Username: group.Spec.Username,
					Password: group.Spec.Password,
				}
			}

			groups = append(groups, sg)
		}
	}

	return groups, nil
}

func createPrometheusConfig(serviceGroups []ServiceGroup) PrometheusConfig {
	config := PrometheusConfig{
		ServiceGroups:      serviceGroups,
		ScrapeInterval:     scrapeInterval,
		ScrapeTimeout:      scrapeTimeout,
		EvaluationInterval: evaluationInterval,
		Scheme:             scheme,
		TlsInsecure:        tlsInsecure,
		AdminUsername:      os.Getenv("PROMETHEUS_ADMIN_USERNAME"),
		AdminPassword:      os.Getenv("PROMETHEUS_ADMIN_PASSWORD"),
	}

	// Validate all service groups
	for _, sg := range config.ServiceGroups {
		if err := sg.Validate(); err != nil {
			logrus.WithError(err).Error("Invalid service group configuration")
			return PrometheusConfig{} // Return empty config on validation failure
		}
	}

	return config
}

func updatePrometheusConfig(configFile string, serviceGroups []ServiceGroup) error {
	config := createPrometheusConfig(serviceGroups)
	contents, err := executeTemplate(config)
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Add YAML validation
	var testConfig map[string]interface{}
	if err := yaml.Unmarshal([]byte(contents), &testConfig); err != nil {
		logrus.WithError(err).WithField("config", contents).Error("Generated invalid YAML configuration")
		return fmt.Errorf("generated invalid YAML: %w", err)
	}

	// Log the generated configuration for debugging
	logrus.WithFields(logrus.Fields{
		"config_file": configFile,
		"yaml":        contents,
	}).Debug("Writing new configuration")

	if err := writeConfigFile(configFile, []byte(contents)); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
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
	monitoringInterval time.Duration
)

func monitorTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry, errChan chan<- error) error {
	ticker := time.NewTicker(monitoringInterval)
	defer ticker.Stop()

	var (
		previousGroups []ServiceGroup
		targetsMu      sync.RWMutex
	)

	for {
		select {
		case <-ticker.C:
			serviceGroups, err := getScrapeTargets(ctx, registry)
			if err != nil {
				logrus.WithError(err).Error("Failed to get scrape targets")
				errChan <- fmt.Errorf("%w: failed to get scrape targets: %v", ErrMonitoring, err)
				continue
			}

			targetsMu.Lock()
			groupsChanged := !reflect.DeepEqual(serviceGroups, previousGroups)
			targetsMu.Unlock()

			if groupsChanged {
				logrus.WithFields(logrus.Fields{
					"num_groups": len(serviceGroups),
					"groups":     serviceGroups,
				}).Info("Service groups changed, updating configuration")

				if err := updatePrometheusConfig(configFile, serviceGroups); err != nil {
					errChan <- fmt.Errorf("failed to update prometheus config: %w", err)
				} else {
					logrus.Info("Successfully updated prometheus configuration")
					targetsMu.Lock()
					previousGroups = serviceGroups
					targetsMu.Unlock()
				}
			}

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
	scrapeInterval = cmd.String("scrape-interval")
	scrapeTimeout = cmd.String("scrape-timeout")
	evaluationInterval = cmd.String("evaluation-interval")
	scheme = cmd.String("scheme")
	tlsInsecure = cmd.String("tls-insecure")
	etcdUsername := cmd.String("etcd-username")
	etcdPassword := cmd.String("etcd-password")
	etcdTimeout := cmd.Duration("etcd-timeout")
	monitoringInterval = cmd.Duration("monitoring-interval")

	logrus.Info("====Starting Promster====")
	logrus.WithFields(logrus.Fields{
		"endpoints": etcdURLScrape,
		"basePath":  etcdBasePath,
		"timeout":   etcdTimeout,
	}).Info("Connecting to etcd")

	registry, err := etcdregistry.NewEtcdRegistry(
		strings.Split(etcdURLScrape, ","),
		etcdBasePath,
		etcdUsername,
		etcdPassword,
		etcdTimeout,
		3,
	)
	if err != nil {
		return fmt.Errorf("failed to create etcd registry: %w", err)
	}

	logrus.Info("Successfully connected to etcd")

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
	// Create error channels
	errChan := make(chan error, 100)
	defer close(errChan)

	// Error handling function
	handleError := func(err error, category string) {
		if err != nil {
			var baseErr error
			switch category {
			case "monitoring":
				baseErr = ErrMonitoring
			case "configuration":
				baseErr = ErrConfiguration
			default:
				baseErr = errors.New("unknown error")
			}
			errChan <- fmt.Errorf("%w: %v", baseErr, err)
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	// Start error handling goroutine
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case err := <-errChan:
				if err != nil {
					logrus.WithError(err).Error("Registry error occurred")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start monitoring in background
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		if err := monitorTargets(monitorCtx, registry, errChan); err != nil {
			if err == context.Canceled {
				logrus.Info("Service monitoring stopped due to shutdown")
			} else {
				handleError(err, "monitoring")
				logrus.WithError(err).Error("Service monitoring stopped unexpectedly")
			}
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	logrus.Info("Shutting down gracefully...")

	// Stop monitoring first
	stopMonitor()
	select {
	case <-monitorDone:
		logrus.Info("Monitoring stopped cleanly")
	case <-time.After(3 * time.Second):
		logrus.Warn("Monitoring stop timed out")
	}

	// Wait for error handling goroutine to finish
	wg.Wait()

	// Close registry connection
	if err := registry.Close(); err != nil {
		logrus.WithError(err).Error("Error during registry shutdown")
	} else {
		logrus.Info("Registry connection closed")
	}

	return nil
}

// Error types
var (
	ErrMonitoring    = errors.New("monitoring error")
	ErrConfiguration = errors.New("configuration error")
)

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
			&cli.DurationFlag{
				Name:    "monitoring-interval",
				Value:   5 * time.Second,
				Usage:   "How often to check for service changes",
				Sources: cli.EnvVars("PROMSTER_MONITORING_INTERVAL"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		logrus.Fatal(err)
	}
}
