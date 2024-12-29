package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go.lumeweb.com/etcd-registry/types"
	"io"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	_ "embed"
	"errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	etcdregistry "go.lumeweb.com/etcd-registry"
	"go.lumeweb.com/promster/pkg/util"
	"gopkg.in/yaml.v2"
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
	return nil
}

var templateFuncs = template.FuncMap{
	"quote": func(v interface{}) string {
		return fmt.Sprintf("%q", v)
	},
	"toJson": func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
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
	tmpl, err := template.New(PROM_TEMPLATE_FILE).Funcs(templateFuncs).Parse(prometheusTemplate)
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
	if adminUsername == "" || adminPassword == "" {
		return fmt.Errorf("PROMETHEUS_ADMIN_USERNAME and PROMETHEUS_ADMIN_PASSWORD must be set")
	}

	_, err := util.RetryOperation(
		func() (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:9090/-/reload", nil)
			if err != nil {
				return false, fmt.Errorf("failed to create request: %w", err)
			}
			req.SetBasicAuth(adminUsername, adminPassword)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false, fmt.Errorf("failed to reload prometheus config: %w", err)
			}
			defer func(Body io.ReadCloser) {
				err := Body.Close()
				if err != nil {
					logrus.Errorf("Failed to close response body: %v", err)
				}
			}(resp.Body)

			return true, nil
		},
		util.ConfigRetry,
		3,
	)

	return err
}

func getScrapeTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry) ([]ServiceGroup, error) {
	groups, err := util.RetryOperation(
		func() ([]ServiceGroup, error) {
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
					// Use ingress_host from labels if available, fallback to node ID
					hostname := node.ID
					if ingressHost, ok := node.Labels["ingress_host"]; ok && ingressHost != "" {
						hostname = ingressHost
					}

					address := fmt.Sprintf("%s:%d", hostname, node.Port)
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

					// Use auth from group spec if both username and password are available
					if group.Spec.Password != "" {
						sg.BasicAuth = &BasicAuth{
							Username: group.Spec.Username,
							Password: group.Spec.Password,
						}
					}

					groups = append(groups, sg)
				}
			}

			return groups, nil
		},
		util.EtcdRetry,
		3,
	)

	return groups, err
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
	_, err := util.RetryOperation(
		func() (bool, error) {
			config := createPrometheusConfig(serviceGroups)
			contents, err := executeTemplate(config)
			if err != nil {
				return false, fmt.Errorf("failed to execute template: %w", err)
			}

			// Add YAML validation
			var testConfig map[string]interface{}
			if err := yaml.Unmarshal([]byte(contents), &testConfig); err != nil {
				logrus.WithError(err).WithField("config", contents).Error("Generated invalid YAML configuration")
				return false, fmt.Errorf("generated invalid YAML: %w", err)
			}

			// Log the generated configuration for debugging
			logrus.WithFields(logrus.Fields{
				"config_file": configFile,
				"yaml":        contents,
			}).Debug("Writing new configuration")

			if err := writeConfigFile(configFile, []byte(contents)); err != nil {
				return false, fmt.Errorf("failed to write config: %w", err)
			}

			if err := reloadPrometheus(); err != nil {
				return false, fmt.Errorf("failed to reload prometheus: %w", err)
			}

			return true, nil
		},
		util.ConfigRetry,
		3,
	)

	return err
}

var (
	scrapeInterval     string
	scrapeTimeout      string
	evaluationInterval string
	scheme             string
	tlsInsecure        string
	configFile         string
	monitoringInterval time.Duration
	serviceGroups      []ServiceGroup // Add serviceGroups to package vars

	// Timer management
	timerPool = sync.Pool{
		New: func() interface{} {
			return time.NewTimer(time.Hour) // Default duration, will be reset before use
		},
	}
)

// safeTimer wraps a timer with safe cleanup
type safeTimer struct {
	*time.Timer
	stopped bool
	mu      sync.Mutex
}

func newSafeTimer(d time.Duration) *safeTimer {
	t := timerPool.Get().(*time.Timer)
	t.Reset(d)
	return &safeTimer{Timer: t}
}

func (t *safeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		stopped := t.Timer.Stop()
		timerPool.Put(t.Timer)
		return stopped
	}
	return false
}

func monitorTargets(ctx context.Context, registry *etcdregistry.EtcdRegistry, errChan chan<- error) error {
	// Create ticker for stale check
	staleCheckTicker := time.NewTicker(5 * time.Minute)
	defer staleCheckTicker.Stop()

	// Start stale check loop in separate goroutine
	go func() {
		for {
			select {
			case <-staleCheckTicker.C:
				if err := checkStaleGroups(ctx, registry); err != nil {
					logrus.WithError(err).Error("Failed to check stale groups")
					errChan <- fmt.Errorf("%w: %v", ErrStaleGroup, err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main watch loop
	for {
		if err := watchWithRetry(ctx, registry, errChan); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logrus.WithError(err).Error("Watch failed, retrying...")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second): // Backoff before retry
				continue
			}
		}
	}
}

func watchWithRetry(ctx context.Context, registry *etcdregistry.EtcdRegistry, errChan chan<- error) error {
	// Start watching for changes with retry
	watchChan, err := util.RetryOperation(
		func() (<-chan types.WatchEvent, error) {
			return registry.WatchServices(ctx)
		},
		util.EtcdRetry,
		3,
	)
	if err != nil {
		return fmt.Errorf("failed to start watch after retries: %w", err)
	}

	// Use a safe timer for debouncing updates
	var currentTimer *safeTimer
	const debounceInterval = 5 * time.Second

	// Ensure timer cleanup on function exit
	defer func() {
		if currentTimer != nil {
			currentTimer.Stop()
		}
	}()

	for {
		select {
		case event, ok := <-watchChan:
			if !ok {
				return fmt.Errorf("watch channel closed")
			}

			logrus.WithFields(logrus.Fields{
				"type":  event.Type,
				"group": event.GroupName,
				"node":  event.Node,
			}).Debug("Received watch event")

			// Stop existing timer if any
			if currentTimer != nil {
				currentTimer.Stop()
			}

			// Create new timer for batched update
			currentTimer = newSafeTimer(debounceInterval)

			// Capture serviceGroups in closure
			currentGroups := serviceGroups

			go func() {
				<-currentTimer.C

				// Get fresh state after batched changes
				newGroups, err := getScrapeTargets(ctx, registry)
				if err != nil {
					errChan <- fmt.Errorf("failed to get updated state: %w", err)
					return
				}

				// Sort both slices for consistent comparison
				sort.Slice(currentGroups, func(i, j int) bool {
					if currentGroups[i].Name != currentGroups[j].Name {
						return currentGroups[i].Name < currentGroups[j].Name
					}
					return currentGroups[i].NodeID < currentGroups[j].NodeID
				})
				sort.Slice(newGroups, func(i, j int) bool {
					if newGroups[i].Name != newGroups[j].Name {
						return newGroups[i].Name < newGroups[j].Name
					}
					return newGroups[i].NodeID < newGroups[j].NodeID
				})

				// Compare sorted states to avoid unnecessary updates
				if !reflect.DeepEqual(currentGroups, newGroups) {
					// Update Prometheus config
					if err := updatePrometheusConfig(configFile, newGroups); err != nil {
						errChan <- fmt.Errorf("failed to update config: %w", err)
						return
					}

					serviceGroups = newGroups // Update outer variable
					logrus.WithField("batch_size", len(newGroups)).Info("Configuration updated after batched changes")
				} else {
					logrus.Debug("Skipping update - no changes detected in batch")
				}
			}()

		case <-ctx.Done():
			if currentTimer != nil {
				currentTimer.Stop()
			}
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

	logrus.Info("====Starting Promster====")
	logrus.WithFields(logrus.Fields{
		"endpoints": etcdURLScrape,
		"basePath":  etcdBasePath,
		"timeout":   etcdTimeout,
	}).Info("Connecting to etcd")

	var registry *etcdregistry.EtcdRegistry
	_, err := util.RetryOperation(
		func() (bool, error) {
			reg, err := etcdregistry.NewEtcdRegistry(
				strings.Split(etcdURLScrape, ","),
				etcdBasePath,
				etcdUsername,
				etcdPassword,
				etcdTimeout*2, // Double the timeout
				5,             // Increase max retries
			)
			if err != nil {
				return false, fmt.Errorf("failed to initialize connection: %w", err)
			}
			registry = reg
			return true, nil
		},
		util.EtcdRetry,
		5,
	)
	if err != nil {
		return fmt.Errorf("failed to create etcd registry after retries: %w", err)
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

	// Get initial state once connected
	serviceGroups, err := getScrapeTargets(ctx, registry)
	if err != nil {
		return fmt.Errorf("failed to get initial state: %w", err)
	}

	logrus.WithField("groups", len(serviceGroups)).Info("Retrieved initial service groups")

	// Create initial config with actual state
	if err := updatePrometheusConfig(configFile, serviceGroups); err != nil {
		return fmt.Errorf("failed to update initial prometheus config: %w", err)
	}

	// Perform initial stale group check
	if err := checkStaleGroups(ctx, registry); err != nil {
		logrus.WithError(err).Error("Failed to perform initial stale group check")
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
	ErrStaleGroup    = errors.New("stale group error")
)

// isProductionEnvironment is a helper to check for production environment labels
func isProductionEnvironment(labels map[string]string) bool {
	env := labels["environment"]
	return env == "production" || env == "prod"
}

func shouldCleanupEmptyGroup(group *types.ServiceGroup) bool {
	// Check protection labels in CommonLabels
	if group.Spec.CommonLabels["no-cleanup"] == "true" ||
		group.Spec.CommonLabels["retain-empty"] == "true" {
		return false
	}

	// Check environment - be more conservative in production
	if isProductionEnvironment(group.Spec.CommonLabels) {
		return false
	}

	// Default behavior - clean up empty groups in non-production
	return true
}

func shouldCleanupStaleGroup(group *types.ServiceGroup, nodes []types.Node) bool {
	// Check protection labels
	if group.Spec.CommonLabels["no-cleanup"] == "true" ||
		group.Spec.CommonLabels["retain-stale"] == "true" {
		return false
	}

	// Never clean up production by default
	if isProductionEnvironment(group.Spec.CommonLabels) {
		return false
	}

	// Check if all nodes have been stale for a significant time
	const staleThreshold = 30 * time.Minute
	for _, node := range nodes {
		if time.Since(node.LastSeen) < staleThreshold {
			return false // At least one node is recent
		}
	}

	// All nodes are stale - clean up in non-production
	return true
}

func checkStaleGroups(ctx context.Context, registry *etcdregistry.EtcdRegistry) error {
	// Disable stale group check for now
	return nil
	services, err := registry.GetServiceGroups(ctx)
	if err != nil {
		return fmt.Errorf("failed to get service groups: %w", err)
	}

	for _, serviceName := range services {
		group, err := registry.GetServiceGroup(ctx, serviceName)
		if err != nil {
			logrus.WithError(err).WithField("service", serviceName).
				Debug("Failed to get service group")
			continue
		}

		nodes, err := group.GetNodes(ctx)
		if err != nil {
			logrus.WithError(err).WithField("service", serviceName).
				Debug("Failed to get nodes")
			continue
		}

		// If group has no nodes, check if it should be cleaned up
		if len(nodes) == 0 {
			if shouldCleanupEmptyGroup(group) {
				if err := group.Delete(ctx); err != nil {
					logrus.WithError(err).WithField("service", serviceName).
						Error("Failed to cleanup empty group")
				} else {
					logrus.WithField("service", serviceName).
						Info("Cleaned up empty group")
				}
			}
			continue
		}

		// Check for stale groups
		if shouldCleanupStaleGroup(group, nodes) {
			if err := group.Delete(ctx); err != nil {
				logrus.WithError(err).WithField("service", serviceName).
					Error("Failed to cleanup stale group")
			} else {
				logrus.WithField("service", serviceName).
					Info("Cleaned up stale group")
			}
		}
	}
	return nil
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
