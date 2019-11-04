package config

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
)

const (
	annotationNamespaceDefault = "injector.tumblr.com"
	defaultVersion             = "latest"
)

var (
	// ErrMissingName ..
	ErrMissingName = fmt.Errorf(`name field is required for an injection config`)
	// ErrNoConfigurationLoaded ..
	ErrNoConfigurationLoaded = fmt.Errorf(`at least one config must be present in the --config-directory`)
	// ErrCannotMergeNilInjectionConfig indicates an error trying to merge `nil` into an InjectionConfig
	ErrCannotMergeNilInjectionConfig = fmt.Errorf("cannot merge nil InjectionConfig")
)

// InjectionConfig is a specific instance of a injected config, for a given annotation
type InjectionConfig struct {
	Name           string               `json:"name"`
	Inherits       string               `json:"inherits"`
	Containers     []corev1.Container   `json:"containers"`
	Volumes        []corev1.Volume      `json:"volumes"`
	Environment    []corev1.EnvVar      `json:"env"`
	VolumeMounts   []corev1.VolumeMount `json:"volumeMounts"`
	HostAliases    []corev1.HostAlias   `json:"hostAliases"`
	InitContainers []corev1.Container   `json:"initContainers"`

	version string
}

// Config is a struct indicating how a given injection should be configured
type Config struct {
	sync.RWMutex
	AnnotationNamespace string                      `yaml:"annotationnamespace"`
	Injections          map[string]*InjectionConfig `yaml:"injections"`
}

// String returns a string representation of the config
func (c *InjectionConfig) String() string {
	inheritsString := ""
	if c.Inherits != "" {
		inheritsString = fmt.Sprintf(" (inherits %s)", c.Inherits)
	}

	return fmt.Sprintf("%s%s: %d containers, %d init containers, %d volumes, %d environment vars, %d volume mounts, %d host aliases",
		c.FullName(),
		inheritsString,
		len(c.Containers),
		len(c.InitContainers),
		len(c.Volumes),
		len(c.Environment),
		len(c.VolumeMounts),
		len(c.HostAliases))
}

// Version returns the parsed version of this injection config. If no version is specified,
// "latest" is returned. The version is extracted from the request annotation, i.e.
// injector.tumblr.com/request: my-sidecar:1.2, where "1.2" is the version.
func (c *InjectionConfig) Version() string {
	if c.version == "" {
		return defaultVersion
	}

	return c.version
}

// FullName returns the full identifier of this sidecar - both the Name, and the Version(), formatted like
// "${.Name}:${.Version}"
func (c *InjectionConfig) FullName() string {
	return canonicalizeConfigName(c.Name, c.Version())
}

// ReplaceInjectionConfigs will take a list of new InjectionConfigs, and replace the current configuration with them.
// this blocks waiting on being able to update the configs in place.
func (c *Config) ReplaceInjectionConfigs(replacementConfigs []*InjectionConfig) {
	c.Lock()
	defer c.Unlock()
	c.Injections = map[string]*InjectionConfig{}

	for _, r := range replacementConfigs {
		c.Injections[r.FullName()] = r
	}
}

// HasInjectionConfig returns bool for whether the config contains a config
// given some key identifier
func (c *Config) HasInjectionConfig(key string) bool {
	c.RLock()
	defer c.RUnlock()

	name, version := configNameFields(key)
	fullKey := canonicalizeConfigName(name, version)

	_, ok := c.Injections[fullKey]

	return ok
}

// GetInjectionConfig returns the InjectionConfig given a requested key
func (c *Config) GetInjectionConfig(key string) (*InjectionConfig, error) {
	c.RLock()
	defer c.RUnlock()

	name, version := configNameFields(key)
	fullKey := canonicalizeConfigName(name, version)

	i, ok := c.Injections[fullKey]
	if !ok {
		return nil, fmt.Errorf("no injection config found for annotation %s", fullKey)
	}

	return i, nil
}

// LoadConfigDirectory loads all configs in a directory and returns the Config
func LoadConfigDirectory(path string) (*Config, error) {
	cfg := Config{
		Injections: map[string]*InjectionConfig{},
	}
	glob := filepath.Join(path, "*.yaml")
	matches, err := filepath.Glob(glob)

	if err != nil {
		return nil, err
	}

	for _, p := range matches {
		c, err := LoadInjectionConfigFromFilePath(p)
		if err != nil {
			glog.Errorf("Error reading injection config from %s: %v", p, err)
			return nil, err
		}

		cfg.Injections[c.FullName()] = c
	}

	if len(cfg.Injections) == 0 {
		return nil, ErrNoConfigurationLoaded
	}

	if cfg.AnnotationNamespace == "" {
		cfg.AnnotationNamespace = annotationNamespaceDefault
	}

	glog.V(2).Infof("Loaded %d injection configs from %s", len(cfg.Injections), glob)

	return &cfg, nil
}

func (base *InjectionConfig) Merge(child *InjectionConfig) error {
	if child == nil {
		return ErrCannotMergeNilInjectionConfig
	}
	// for all fields, merge child into base, eventually returning base
	base.Name = child.Name
	base.version = child.version
	base.Inherits = child.Inherits

	// merge containers
	for _, cctr := range child.Containers {
		contains := false
		for bi, bctr := range base.Containers {
			if bctr.Name == cctr.Name {
				contains = true
				base.Containers[bi] = cctr
			}
		}
		if !contains {
			base.Containers = append(base.Containers, cctr)
		}
	}

	// merge volumes
	for _, cv := range child.Volumes {
		contains := false
		for bi, bv := range base.Volumes {
			if bv.Name == cv.Name {
				contains = true
				base.Volumes[bi] = cv
			}
		}
		if !contains {
			base.Volumes = append(base.Volumes, cv)
		}
	}

	// merge environment
	for _, cv := range child.Environment {
		contains := false
		for bi, bv := range base.Environment {
			if bv.Name == cv.Name {
				contains = true
				base.Environment[bi] = cv
			}
		}
		if !contains {
			base.Environment = append(base.Environment, cv)
		}
	}

	// merge volume mounts
	for _, cv := range child.VolumeMounts {
		contains := false
		for bi, bv := range base.VolumeMounts {
			if bv.Name == cv.Name {
				contains = true
				base.VolumeMounts[bi] = cv
			}
		}
		if !contains {
			base.VolumeMounts = append(base.VolumeMounts, cv)
		}
	}

	// merge host aliases
	// note: we do not need to merge things, as entries are not keyed
	for _, cv := range child.HostAliases {
		base.HostAliases = append(base.HostAliases, cv)
	}

	// merge init containers
	for _, cv := range child.InitContainers {
		contains := false
		for bi, bv := range base.InitContainers {
			if bv.Name == cv.Name {
				contains = true
				base.InitContainers[bi] = cv
			}
		}
		if !contains {
			base.InitContainers = append(base.InitContainers, cv)
		}
	}

	return nil
}

// LoadInjectionConfigFromFilePath returns a InjectionConfig given a yaml file on disk
// NOTE: if the InjectionConfig loaded has an Inherits field, we recursively load from Inherits
// and merge the InjectionConfigs to create an inheritance pattern. Inherits is not supported for
// configs loaded via `LoadInjectionConfig`
func LoadInjectionConfigFromFilePath(configFile string) (*InjectionConfig, error) {
	f, err := os.Open(configFile)
	if err != nil {
		return nil, fmt.Errorf("error loading injection config from file %s: %s", configFile, err.Error())
	}

	defer f.Close()
	glog.V(3).Infof("Loading injection config from file %s", configFile)

	ic, err := LoadInjectionConfig(f)
	if err != nil {
		return nil, err
	}

	// Support inheritance from an InjectionConfig loaded from a file on disk
	if ic.Inherits != "" {
		base, err := LoadInjectionConfigFromFilePath(ic.Inherits)
		if err != nil {
			return nil, err
		}
		return base.Merge(ic)
	}
	return ic, nil
}

// LoadInjectionConfig takes an io.Reader and parses out an injectionconfig
func LoadInjectionConfig(reader io.Reader) (*InjectionConfig, error) {
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var cfg InjectionConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Name == "" {
		return nil, ErrMissingName
	}

	// we need to split the Name field apart into a Name and Version component
	cfg.Name, cfg.version = configNameFields(cfg.Name)

	glog.V(3).Infof("Loaded injection config %s version=%s sha256sum=%x", cfg.Name, cfg.Version(), sha256.Sum256(data))

	return &cfg, nil
}

// given a name of a config, extract the name and version. Format is "name[:version]" where :version
// is optional, and is assumed to be "latest" if omitted.
func configNameFields(shortName string) (name, version string) {
	substrings := strings.Split(shortName, ":")

	if len(substrings) <= 1 {
		// no :<version> specified, so assume default version
		return shortName, defaultVersion
	}

	versionField := len(substrings) - 1

	if substrings[versionField] == "" {
		return strings.Join(substrings[:versionField], ":"), defaultVersion
	}

	return strings.Join(substrings[:versionField], ":"), substrings[versionField]
}

func canonicalizeConfigName(name, version string) string {
	return strings.ToLower(fmt.Sprintf("%s:%s", name, version))
}
