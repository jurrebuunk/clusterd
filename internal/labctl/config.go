package labctl

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Built-in context used until the config file says otherwise.
const (
	defaultSite = "hgd1"
	defaultAPI  = "https://api.lan.buunk.org"
)

// Site is one context: an API and where its token lives.
type Site struct {
	API       string `toml:"api"`
	TokenFile string `toml:"token_file,omitempty"`
}

// Config is ~/.config/labctl/config.toml.
type Config struct {
	Current string          `toml:"current"`
	Sites   map[string]Site `toml:"sites"`

	path string
}

func configDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "labctl"), nil
}

func loadConfig() (*Config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	c := &Config{path: filepath.Join(dir, "config.toml")}
	if _, err := toml.DecodeFile(c.path, c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", c.path, err)
	}
	if c.Sites == nil {
		c.Sites = map[string]Site{}
	}
	if len(c.Sites) == 0 {
		c.Sites[defaultSite] = Site{API: defaultAPI}
	}
	if c.Current == "" {
		c.Current = defaultSite
	}
	return c, nil
}

func (c *Config) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# labctl contexts; see `labctl use` and `labctl login`.\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	return writeFileAtomic(c.path, buf.Bytes(), 0o600)
}

func (c *Config) siteNames() []string {
	names := make([]string, 0, len(c.Sites))
	for n := range c.Sites {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// tokenFile is where a site's token is kept.
func (c *Config) tokenFile(site string) (string, error) {
	if s, ok := c.Sites[site]; ok && s.TokenFile != "" {
		return expandHome(s.TokenFile), nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tokens", site), nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
