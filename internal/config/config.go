package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

const (
	filename      = "config.json"
	configFolder  = ".config"
	configDirName = "s3duck-tui"
)

type Params struct {
	HomeDir  string
	FileName string
	Config   []*Config
	// LoadErr holds a non-fatal startup error (missing/corrupt config file).
	// The app starts with an empty profile list and surfaces this in the UI
	// instead of crashing.
	LoadErr error
}

type Config struct {
	Name      string  `json:"name"`
	BaseUrl   string  `json:"base_url"`
	Region    *string `json:"region"`
	AccessKey string  `json:"access_key"`
	SecretKey string  `json:"secret_key"`
	IgnoreSsl bool    `json:"ignore_ssl"`
	// DownloadDir is the destination for downloads. Empty -> ~/Downloads.
	// A leading "~" is expanded to the user's home directory.
	DownloadDir string `json:"download_dir,omitempty"`
}

func (p *Params) WriteConfig() error {
	file, err := json.Marshal(p.Config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(p.FileName, file, 0600); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", p.FileName, err)
	}
	return nil
}

func (p *Params) NewConfiguration(config *Config) error {
	if config.Name == "" {
		return errors.New("empty name not allowed")
	}

	p.Config = append(p.Config, config)
	return p.WriteConfig()
}

func LoadConfiguration(fileName string) ([]*Config, error) {
	var config []*Config
	configFile, err := os.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", fileName, err)
	}
	if err := json.Unmarshal(configFile, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", fileName, err)
	}
	return config, nil
}

func FileExist(fileName string) (bool, error) {
	_, err := os.Stat(fileName)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err == nil {
		return true, err
	}
	return false, err
}

func (p *Params) CopyConfig(conf Config) error {
	p.Config = append(p.Config, &conf)
	return p.WriteConfig()
}

func (p *Params) DeleteConfig(i int) error {
	p.Config = append(p.Config[:i], p.Config[i+1:]...)
	return p.WriteConfig()
}

func CreateEmptyConfig(configFile string) error {
	if err := os.MkdirAll(filepath.Dir(configFile), 0700); err != nil {
		return err
	}
	a, _ := json.Marshal(make([]Config, 0))
	return os.WriteFile(configFile, a, 0600)
}

func NewParams() *Params {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return &Params{LoadErr: fmt.Errorf("can't get user home dir: %w", err)}
	}

	configFile := path.Join(homeDir, configFolder, configDirName, filename)
	params := &Params{HomeDir: homeDir, FileName: configFile}

	exists, err := FileExist(configFile)
	if err != nil {
		params.LoadErr = err
		return params
	}
	if !exists {
		if err := CreateEmptyConfig(configFile); err != nil {
			params.LoadErr = fmt.Errorf("failed to create config: %w", err)
			return params
		}
	}

	config, err := LoadConfiguration(configFile)
	if err != nil {
		params.LoadErr = err
		return params
	}
	params.Config = config
	return params
}
