package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
}

type Config struct {
	Name      string  `json:"name"`
	BaseUrl   string  `json:"base_url"`
	Region    *string `json:"region"`
	AccessKey string  `json:"access_key"`
	SecretKey string  `json:"secret_key"`
}

func GetHomeDir() string {
	homeDir, err := os.UserHomeDir()

	if err != nil {
		panic("can't get user homedir")
	}
	return homeDir
}

//func (p *Params) ConfigExists(name string) (bool, *Config, int) {
//	for i, cf := range p.Config {
//		if cf.Name == name {
//			return true, cf, i
//		}
//	}
//	return false, nil, -1
//}

func (p *Params) WriteConfig() {

	file, _ := json.Marshal(p.Config)
	err := os.WriteFile(p.FileName, file, 0700)

	if err != nil {
		log.Fatalf("failed to write config file: %s", filename)
	}
}

func (p *Params) NewConfiguration(config *Config) error {
	//exists, _, _ := p.ConfigExists(config.Name)
	if config.Name == "" {
		return errors.New(fmt.Sprintf("empty name not allowed"))
	}
	//
	//if exists {
	//	return errors.New(fmt.Sprintf("profile with name %s exists", config.Name))
	//}
	//
	p.Config = append(p.Config, config)
	p.WriteConfig()
	return nil
}

func LoadConfiguration(fileName string) []*Config {
	var config []*Config
	configFile, err := os.ReadFile(fileName)

	if err != nil {
		log.Fatalf("failed to open config file: %s", fileName)
	}
	err = json.Unmarshal(configFile, &config)
	if err != nil {
		log.Fatalf("failed to parse config file: %s", fileName)
	}
	return config
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

func (p *Params) CopyConfig(conf Config) {
	p.Config = append(p.Config, &conf)
	p.WriteConfig()
}

func (p *Params) DeleteConfig(i int) {
	p.Config = append(p.Config[:i], p.Config[i+1:]...)
	p.WriteConfig()
}

func CreateEmptyConfig(configFile string) {
	err := os.MkdirAll(filepath.Dir(configFile), 0700)
	f, err := os.Create(configFile)
	ap := make([]Config, 0)
	a, _ := json.Marshal(ap)
	err = os.WriteFile(configFile, a, 0700)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
}

func NewParams() *Params {

	homeDir := GetHomeDir()
	configFile := path.Join(homeDir, configFolder, configDirName, filename)
	exists, err := FileExist(configFile)
	if err != nil {
		log.Fatalf(err.Error())
	}
	if !exists {
		CreateEmptyConfig(configFile)
	}
	config := LoadConfiguration(configFile)
	params := Params{
		homeDir,
		configFile,
		config,
	}
	return &params
}
