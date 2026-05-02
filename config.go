package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Telmate/proxmox-api-go/proxmox"
	"github.com/actions/scaleset"
)

type Config struct {
	RegistrationURL string        `yaml:"registrationUrl"`
	MaxRunners      int           `yaml:"maxRunners"`
	MinRunners      int           `yaml:"minRunners"`
	ScaleSetName    string        `yaml:"scaleSetName"`
	Labels          []string      `yaml:"labels"`
	RunnerGroup     string        `yaml:"runnerGroup"`
	GitHubApp       GitHubAppYAML `yaml:"gitHubApp"`
	Token           string        `yaml:"token"`
	RunnerImage     string        `yaml:"runnerImage"`
	LogLevel        string        `yaml:"logLevel"`
	LogFormat       string        `yaml:"logFormat"`

	// Proxmox connection settings
	ProxmoxURL            string                 `yaml:"proxmoxUrl"`
	ProxmoxTokenID        string                 `yaml:"proxmoxTokenId"`
	ProxmoxTokenSecret    proxmox.ApiTokenSecret `yaml:"proxmoxTokenSecret"`
	ProxmoxInsecure       bool                   `yaml:"proxmoxInsecure"`
	ProxmoxNode           string                 `yaml:"proxmoxNode"`
	ProxmoxStorage        string                 `yaml:"proxmoxStorage"`
	ProxmoxOSTemplate     string                 `yaml:"proxmoxOsTemplate"`
	ProxmoxOSTemplateName string                 `yaml:"proxmoxOsTemplateName"`
}

// GitHubAppYAML mirrors the fields needed for GitHub App auth and includes
// YAML tags in camelCase so the config file can specify these values.
type GitHubAppYAML struct {
	ClientID       string `yaml:"clientId"`
	InstallationID int64  `yaml:"installationId"`
	PrivateKey     string `yaml:"privateKey"`
}

func (g GitHubAppYAML) toScalesetAuth() scaleset.GitHubAppAuth {
	return scaleset.GitHubAppAuth{
		ClientID:       g.ClientID,
		InstallationID: g.InstallationID,
		PrivateKey:     g.PrivateKey,
	}
}

func (c *Config) defaults() {
	if c.RunnerGroup == "" {
		c.RunnerGroup = scaleset.DefaultRunnerGroup
	}
	if c.RunnerImage == "" {
		c.RunnerImage = "ghcr.io/actions/actions-runner:latest"
	}
}

func (c *Config) Validate() error {
	c.defaults()
	c.Print()
	if _, err := url.ParseRequestURI(c.RegistrationURL); err != nil {
		return fmt.Errorf("invalid registration URL: %w, it should be the full URL of where you want to register your scale set, e.g. 'https://github.com/org/repo'", err)
	}

	gha := c.GitHubApp.toScalesetAuth()
	appError := (&gha).Validate()
	if c.Token == "" && appError != nil {
		return fmt.Errorf("no credentials provided: either GitHub App (client id, installation id and private key) (recommended) or a Personal Access Token are required")
	}

	if c.ScaleSetName == "" {
		return fmt.Errorf("scale set name is required")
	}
	for i, label := range c.Labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("label at index %d is empty", i)
		}
	}
	if c.MaxRunners < c.MinRunners {
		return fmt.Errorf("max runners cannot be less than min-runners")
	}
	if c.RunnerGroup == "" {
		return fmt.Errorf("runner group is required")
	}
	if c.RunnerImage == "" {
		return fmt.Errorf("runner image is required")
	}

	// Proxmox validation: require basic fields for LXC creation
	if c.ProxmoxURL == "" {
		return fmt.Errorf("proxmox URL is required (proxmoxUrl)")
	}
	if c.ProxmoxTokenSecret == "" {
		return fmt.Errorf("proxmox token secret is required (proxmoxTokenSecret)")
	}
	if c.ProxmoxNode == "" {
		return fmt.Errorf("proxmox node is required (proxmoxNode)")
	}
	if c.ProxmoxStorage == "" {
		return fmt.Errorf("proxmox storage is required (proxmoxStorage)")
	}
	if c.ProxmoxOSTemplate == "" {
		if c.ProxmoxOSTemplateName == "" {
			return fmt.Errorf("proxmox OS template is required (proxmoxOsTemplate or proxmoxOsTemplateName)")
		}
	}
	return nil
}

// ProxmoxClient initializes and returns a proxmox client authenticated with the
// provided credentials. Uses the Telmate proxmox-api-go library.
func (c *Config) ProxmoxClient() (*proxmox.Client, error) {
	// build TLS config
	var tlsCfg *tls.Config
	if c.ProxmoxInsecure {
		tlsCfg = &tls.Config{InsecureSkipVerify: true}
	}

	client, err := proxmox.NewClient(c.ProxmoxURL, nil, "", tlsCfg, "", 300, false)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxmox client: %w", err)
	}
	var apiTokenID proxmox.ApiTokenID
	apiTokenID.Parse(c.ProxmoxTokenID)
	client.SetAPIToken(apiTokenID, c.ProxmoxTokenSecret)
	return client, nil
}

// systemInfo serves as a base system info
func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "dockerscaleset",
		Subsystem:  "dockerscaleset",
		CommitSHA:  "NA",    // You can leverage build flags to set commit SHA
		Version:    "0.1.0", // You can leverage build flags to set version
		ScaleSetID: scaleSetID,
	}
}

func (c *Config) ScalesetClient() (*scaleset.Client, error) {
	gha := c.GitHubApp.toScalesetAuth()
	if err := (&gha).Validate(); err == nil {
		return scaleset.NewClientWithGitHubApp(
			scaleset.ClientWithGitHubAppConfig{
				GitHubConfigURL: c.RegistrationURL,
				GitHubAppAuth:   gha,
				SystemInfo:      systemInfo(0),
			},
		)
	}

	return scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     c.RegistrationURL,
			PersonalAccessToken: c.Token,
			SystemInfo:          systemInfo(0),
		},
	)
}

func (c *Config) Logger() *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	switch c.LogFormat {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     lvl,
		}))
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     lvl,
		}))
	default:
		return slog.New(slog.DiscardHandler)
	}
}

// BuildLabels returns the labels to use for the runner scale set.
// If custom labels are provided, those are used; otherwise, the scale set name is used as the label.
func (c *Config) BuildLabels() []scaleset.Label {
	if len(c.Labels) > 0 {
		labels := make([]scaleset.Label, len(c.Labels))
		for i, name := range c.Labels {
			labels[i] = scaleset.Label{Name: strings.TrimSpace(name)}
		}
		return labels
	}
	return []scaleset.Label{{Name: c.ScaleSetName}}
}

// LoadConfigFromFile reads a YAML file at path and unmarshals it into Config.
// It applies default values via defaults() before returning.
// LoadFromFile reads a YAML file at path and unmarshals it into the receiver.
// It applies default values via defaults() before returning.
func (c *Config) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	var fileCfg Config
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		return fmt.Errorf("parsing config file: %w", err)
	}

	// Merge: only set fields that are not already set on the receiver.
	if c.RegistrationURL == "" {
		c.RegistrationURL = fileCfg.RegistrationURL
	}
	if c.MaxRunners == 0 && fileCfg.MaxRunners != 0 {
		c.MaxRunners = fileCfg.MaxRunners
	}
	if c.MinRunners == 0 && fileCfg.MinRunners != 0 {
		c.MinRunners = fileCfg.MinRunners
	}
	if c.ScaleSetName == "" {
		c.ScaleSetName = fileCfg.ScaleSetName
	}
	if len(c.Labels) == 0 && len(fileCfg.Labels) > 0 {
		c.Labels = fileCfg.Labels
	}
	if c.RunnerGroup == "" {
		c.RunnerGroup = fileCfg.RunnerGroup
	}
	// GitHub App: prefer existing valid app, otherwise take from file if valid.
	gha := c.GitHubApp.toScalesetAuth()
	fgha := fileCfg.GitHubApp.toScalesetAuth()
	if (&gha).Validate() != nil && (&fgha).Validate() == nil {
		c.GitHubApp = fileCfg.GitHubApp
	}
	if c.Token == "" {
		c.Token = fileCfg.Token
	}
	if c.RunnerImage == "" {
		c.RunnerImage = fileCfg.RunnerImage
	}
	if c.LogLevel == "" {
		c.LogLevel = fileCfg.LogLevel
	}
	if c.LogFormat == "" {
		c.LogFormat = fileCfg.LogFormat
	}

	// Proxmox fields: merge values from file when not set on receiver
	if c.ProxmoxURL == "" {
		c.ProxmoxURL = fileCfg.ProxmoxURL
	}
	if c.ProxmoxTokenID == "" {
		c.ProxmoxTokenID = fileCfg.ProxmoxTokenID
	}
	if c.ProxmoxTokenSecret == "" {
		c.ProxmoxTokenSecret = fileCfg.ProxmoxTokenSecret
	}
	// Insecure: if not set true, inherit from file
	if !c.ProxmoxInsecure && fileCfg.ProxmoxInsecure {
		c.ProxmoxInsecure = true
	}
	if c.ProxmoxNode == "" {
		c.ProxmoxNode = fileCfg.ProxmoxNode
	}
	if c.ProxmoxStorage == "" {
		c.ProxmoxStorage = fileCfg.ProxmoxStorage
	}
	if c.ProxmoxOSTemplate == "" {
		c.ProxmoxOSTemplate = fileCfg.ProxmoxOSTemplate
	}
	if c.ProxmoxOSTemplateName == "" {
		c.ProxmoxOSTemplateName = fileCfg.ProxmoxOSTemplateName
	}

	c.defaults()
	return nil
}

// Print marshals the config to YAML and writes it to stdout.
func (c *Config) Print() {
	data, err := yaml.Marshal(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal config: %v\n", err)
		return
	}
	fmt.Print(string(data))
}
