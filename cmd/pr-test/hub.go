package main

import (
	"io/ioutil"
	"path/filepath"

	"github.com/pkg/errors"

	"gopkg.in/yaml.v2"
)

type hubHost struct {
	Host        string `toml:"host"`
	User        string `toml:"user"`
	AccessToken string `toml:"access_token"`
	Protocol    string `toml:"protocol"`
	UnixSocket  string `toml:"unix_socket,omitempty"`
}

type hubConfig struct {
	Hosts []*hubHost `toml:"hosts"`
}

func decodeHubConfig(d []byte, c *hubConfig) error {
	yc := yaml.MapSlice{}
	err := yaml.Unmarshal(d, &yc)

	if err != nil {
		return err
	}

	for _, hostEntry := range yc {
		v := hostEntry.Value.([]interface{})
		if len(v) < 1 {
			continue
		}
		host := &hubHost{Host: hostEntry.Key.(string)}
		for _, prop := range v[0].(yaml.MapSlice) {
			switch prop.Key.(string) {
			case "user":
				host.User = prop.Value.(string)
			case "oauth_token":
				host.AccessToken = prop.Value.(string)
			case "protocol":
				host.Protocol = prop.Value.(string)
			case "unix_socket":
				host.UnixSocket = prop.Value.(string)
			}
		}
		c.Hosts = append(c.Hosts, host)
	}

	return nil
}

func (hc *hubConfig) getGithubHost() *hubHost {
	for _, host := range hc.Hosts {
		if host.Host == "github.com" {
			return host
		}
	}
	return nil
}

func getGithubAccessToken() (string, error) {
	home, err := getHome()
	if err != nil {
		return "", err
	}
	hubCfgFile := filepath.Join(home, ".config/hub")
	content, err := ioutil.ReadFile(hubCfgFile)
	if err != nil {
		return "", err
	}

	var cfg hubConfig
	err = decodeHubConfig(content, &cfg)
	if err != nil {
		return "", err
	}

	ghHost := cfg.getGithubHost()
	if ghHost == nil {
		return "", errors.New("not found host github.com in hub config")
	}

	return ghHost.AccessToken, nil
}
