// Package kube is the small slice of the Kubernetes API clusterd needs,
// over plain HTTPS with a ServiceAccount token from a kubeconfig.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jurrebuunk/clusterd/internal/api"
)

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// Client talks to one Kubernetes API server.
type Client struct {
	server string
	token  string
	http   *http.Client
}

// FromKubeconfig reads a kubeconfig with a bearer token and an inline CA.
func FromKubeconfig(path string) (*Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ctxName := kc.CurrentContext
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == ctxName || (ctxName == "" && len(kc.Contexts) == 1) {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	c := &Client{}
	var caData string
	for _, cl := range kc.Clusters {
		if cl.Name == clusterName {
			c.server, caData = strings.TrimRight(cl.Cluster.Server, "/"), cl.Cluster.CAData
		}
	}
	for _, u := range kc.Users {
		if u.Name == userName {
			c.token = u.User.Token
		}
	}
	if c.server == "" || c.token == "" || caData == "" {
		return nil, fmt.Errorf("%s: need a current context with server, certificate-authority-data and token", path)
	}
	pem, err := base64.StdEncoding.DecodeString(caData)
	if err != nil {
		return nil, fmt.Errorf("%s: certificate-authority-data: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no CA certificate found", path)
	}
	c.http = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 5 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return c, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kubernetes %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type nodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			NodeInfo struct {
				KubeletVersion string `json:"kubeletVersion"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

// Nodes lists the cluster's nodes.
func (c *Client) Nodes(ctx context.Context) ([]api.K3sNode, error) {
	if c == nil {
		return nil, errors.New("no kubeconfig configured")
	}
	var list nodeList
	if err := c.get(ctx, "/api/v1/nodes", &list); err != nil {
		return nil, err
	}
	return convertNodes(list), nil
}

func convertNodes(list nodeList) []api.K3sNode {
	out := make([]api.K3sNode, 0, len(list.Items))
	for _, n := range list.Items {
		node := api.K3sNode{
			Name:        n.Metadata.Name,
			Schedulable: !n.Spec.Unschedulable,
			Version:     n.Status.NodeInfo.KubeletVersion,
			Zone:        n.Metadata.Labels["topology.kubernetes.io/zone"],
			Roles:       []string{},
		}
		for k := range n.Metadata.Labels {
			if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
				node.Roles = append(node.Roles, role)
			}
		}
		sort.Strings(node.Roles)
		for _, cond := range n.Status.Conditions {
			if cond.Type == "Ready" {
				node.Ready = cond.Status == "True"
			}
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
