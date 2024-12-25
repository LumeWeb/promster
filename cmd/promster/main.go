package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	etcdregistry "go.lumeweb.com/etcd-registry"
)

const PROM_TEMPLATE_FILE = "prometheus.yml.tmpl"

// SourceTarget defines the structure of a prometheus source target
type SourceTarget struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels,omitempty"`
}

func executeTemplate(baseDir string, templateFile string, data interface{}) (string, error) {
	tmpl, err := template.ParseFiles(baseDir + templateFile)
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
				Usage:    "ETCD URLs for scrape source server. If empty, will be the same as --etcd-url. ex: http://etcd0:2379",
				Required: true,
				Sources:  cli.EnvVars("PROMSTER_SCRAPE_ETCD_URL"),
			},
			&cli.StringFlag{
				Name:     "scrape-etcd-path",
				Usage:    "Base ETCD path for getting servers to be scrapped",
				Required: true,
				Sources:  cli.EnvVars("PROMSTER_SCRAPE_ETCD_PATH"),
			},
			&cli.StringFlag{
				Name:    "register-etcd-path",
				Usage:   "Base ETCD path for registering this service",
				Value:   "/promster",
				Sources: cli.EnvVars("PROMSTER_REGISTER_ETCD_PATH"),
			},
			&cli.StringFlag{
				Name:    "scrape-paths",
				Value:   "/metrics",
				Usage:   "URI for scrape of each target. May contain a list separated by ','.",
				Sources: cli.EnvVars("PROMSTER_SCRAPE_PATHS"),
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
				Name:    "scrape-match",
				Usage:   "Metrics regex filter applied on scraped targets. Commonly used in conjunction with /federate metrics endpoint",
				Sources: cli.EnvVars("PROMSTER_SCRAPE_MATCH"),
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
				Name:    "register-ttl",
				Value:   60 * time.Second,
				Usage:   "ETCD TTL for service registration",
				Sources: cli.EnvVars("PROMSTER_REGISTER_TTL"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		logrus.Fatal(err)
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
	scrapeEtcdPath := cmd.String("scrape-etcd-path")
	registerEtcdPath := cmd.String("register-etcd-path")
	scrapeInterval := cmd.String("scrape-interval")
	scrapeTimeout := cmd.String("scrape-timeout")
	scrapeMatch := cmd.String("scrape-match")
	evaluationInterval := cmd.String("evaluation-interval")
	scrapePaths := strings.Split(cmd.String("scrape-paths"), ",")
	scheme := cmd.String("scheme")
	tlsInsecure := cmd.String("tls-insecure")
	etcdUsername := cmd.String("etcd-username")
	etcdPassword := cmd.String("etcd-password")
	etcdTimeout := cmd.Duration("etcd-timeout")

	logrus.Infof("====Starting Promster====")
	logrus.Debugf("Updating prometheus file...")
	time.Sleep(5 * time.Second)

	if err := updatePrometheusConfig("/prometheus.yml", scrapeInterval, scrapeTimeout, evaluationInterval, scrapePaths, scrapeMatch, scheme, tlsInsecure); err != nil {
		return fmt.Errorf("failed to update prometheus config: %w", err)
	}

	if err := createRulesFromENV("/rules.yml"); err != nil {
		return fmt.Errorf("failed to create rules: %w", err)
	}

	endpointsScrape := strings.Split(etcdURLScrape, ",")

	registry, err := etcdregistry.NewEtcdRegistry(endpointsScrape, scrapeEtcdPath, etcdUsername, etcdPassword, etcdTimeout)
	if err != nil {
		return fmt.Errorf("failed to create etcd registry: %w", err)
	}

	// Create a context that is cancelled when the program is interrupted
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	scrapeTargets := make([]SourceTarget, 0)
	go func() {
		for {
			nodes, err := registry.GetServiceNodes(registerEtcdPath)
			if err != nil {
				logrus.Warnf("Failed to get service nodes: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			scrapeTargets = make([]SourceTarget, 0)
			for _, node := range nodes {
				scrapeTargets = append(scrapeTargets, SourceTarget{
					Labels:  map[string]string{"prsn": node.Name},
					Targets: []string{node.Info["address"]},
				})
			}

			if err := updatePrometheusTargets(scrapeTargets); err != nil {
				logrus.Warnf("Couldn't update Prometheus scrape targets: %v", err)
			}
			logrus.Debugf("Scrape targets found: %s", scrapeTargets)
			time.Sleep(5 * time.Second)
		}
	}()

	<-ctx.Done()

	return nil
}

func updatePrometheusConfig(prometheusFile string, scrapeInterval string, scrapeTimeout string, evaluationInterval string, scrapePaths []string, scrapeMatch string, scheme string, tlsInsecure string) error {
	logrus.Infof("updatePrometheusConfig. scrapeInterval=%s,scrapeTimeout=%s,evaluationInterval=%s,scrapePaths=%s,scrapeMatch=%s,scheme=%s,tlsInsecure=%s", scrapeInterval, scrapeTimeout, evaluationInterval, scrapePaths, scrapeMatch, scheme, tlsInsecure)
	input := make(map[string]interface{})
	input["scrapeInterval"] = scrapeInterval
	input["scrapeTimeout"] = scrapeTimeout
	input["evaluationInterval"] = evaluationInterval
	input["scrapePaths"] = scrapePaths
	input["scrapeMatch"] = scrapeMatch
	input["scheme"] = scheme
	input["tlsInsecure"] = tlsInsecure
	contents, err := executeTemplate("/", PROM_TEMPLATE_FILE, input)
	if err != nil {
		return err
	}

	logrus.Debugf("%s: '%s'", prometheusFile, contents)
	err = os.WriteFile(prometheusFile, []byte(contents), 0666)
	if err != nil {
		return err
	}

	resp, err := http.Post("http://localhost:9090/-/reload", "", nil)
	if err != nil {
		logrus.Warnf("Couldn't reload Prometheus config. Maybe it wasn't initialized at this time and will get the config as soon as getting started. Ignoring.")
	}
	if resp != nil {
		err = resp.Body.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// RecordingRule defines a structure to simplify the handling of Prometheus recording rules
type RecordingRule struct {
	name   string
	expr   string
	labels map[string]string
}

// getLabelMap builds a label map from a raw configuration string
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

// getPrintableLabels builds the labels in a printable format
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

	logrus.Debugf("Found %d rules: %s", len(rules), rules)

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

	logrus.Debugf("%s: '%v'", rulesFile, rulesContents)

	err := os.WriteFile(rulesFile, []byte(rulesContents), 0666)
	if err != nil {
		return err
	}

	resp, err := http.Post("http://localhost:9090/-/reload", "", nil)
	if err != nil {
		logrus.Warnf("Couldn't reload Prometheus config. Maybe it wasn't initialized at this time and will get the config as soon as getting started. Ignoring.")
	}
	if resp != nil {
		err = resp.Body.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

func updatePrometheusTargets(scrapeTargets []SourceTarget) error {
	logrus.Debugf("updatePrometheusTargets. scrapeTargets=%s", scrapeTargets)

	// Since we're not scaling, we'll use all targets
	selfScrapeTargets := scrapeTargets

	//generate json file
	contents, err := json.Marshal(selfScrapeTargets)
	if err != nil {
		return err
	}
	logrus.Debugf("Writing /servers.json: '%s'", string(contents))
	err = os.WriteFile("/servers.json", contents, 0666)
	if err != nil {
		return err
	}

	//force Prometheus to update its configuration live
	resp, err := http.Post("http://localhost:9090/-/reload", "", nil)
	if err != nil {
		return err
	}
	if resp != nil {
		err = resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
