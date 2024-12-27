package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"
)

type RecordingRule struct {
	name   string
	expr   string
	labels map[string]string
}

func getLabelMap(rawLabels string) (map[string]string, error) {
	toReturn := make(map[string]string)
	if rawLabels == "" {
		return toReturn, nil
	}
	
	mappings := strings.Split(rawLabels, ",")
	for _, mapping := range mappings {
		if mapping == "" {
			continue
		}
		keyValue := strings.Split(mapping, ":")
		if len(keyValue) != 2 {
			return nil, fmt.Errorf("invalid label format: %s", mapping)
		}
		toReturn[strings.TrimSpace(keyValue[0])] = strings.TrimSpace(keyValue[1])
	}
	return toReturn, nil
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

func validateRuleExpr(expr string) error {
	if expr == "" {
		return fmt.Errorf("expression cannot be empty")
	}
	return nil
}

func createRulesFromENV(rulesFile string) error {
	// Validate rules file path is writable
	dir := filepath.Dir(rulesFile)
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return fmt.Errorf("rules directory %s is not writable: %w", dir, err)
	}

	// Check if rules file exists and is writable
	if _, err := os.Stat(rulesFile); err == nil {
		if err := unix.Access(rulesFile, unix.W_OK); err != nil {
			return fmt.Errorf("rules file %s is not writable: %w", rulesFile, err)
		}
	} else {
		// Create rules file if it doesn't exist
		if _, err := os.OpenFile(rulesFile, os.O_CREATE, 0644); err != nil {
			return fmt.Errorf("failed to create rules file: %w", err)
		}
	}

	maxRules := 100
	if maxStr := os.Getenv("PROMETHEUS_MAX_RULES"); maxStr != "" {
		if max, err := strconv.Atoi(maxStr); err == nil && max > 0 {
			maxRules = max
		}
	}

	env := make(map[string]string)
	for _, e := range os.Environ() {
		pair := strings.Split(e, "=")
		env[pair[0]] = pair[1]
	}
	
	rules := make([]RecordingRule, 0)
	for i := 1; i <= maxRules; i++ {
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

		labels, err := getLabelMap(env[klabels])
		if err != nil {
			return fmt.Errorf("invalid labels for rule %s: %w", vname, err)
		}
		
		if err := validateRuleExpr(vexpr); err != nil {
			return fmt.Errorf("invalid expression for rule %s: %w", vname, err)
		}

		rules = append(rules, RecordingRule{
			name:   vname,
			expr:   vexpr,
			labels: labels,
		})
	}

	if len(rules) == 0 {
		logrus.Info("No prometheus rules found in environment variables")
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

	// Validate YAML before writing
	if err := validateYAML([]byte(rulesContents)); err != nil {
		return fmt.Errorf("invalid YAML rules: %w", err)
	}

	if err := writeConfigFile(rulesFile, []byte(rulesContents)); err != nil {
		return fmt.Errorf("failed to write rules file: %w", err)
	}

	return reloadPrometheus()
}

func validateYAML(data []byte) error {
	var out interface{}
	return yaml.Unmarshal(data, &out)
}
