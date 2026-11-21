// Package kubeconfig reads what the first-party provider plugins need from
// the kubeconfig files that provider APIs generate: the API server of the
// current context, its CA, and the user's client certificate or token.
package kubeconfig

import (
	"encoding/base64"
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

// Access is how a kubeconfig's current context reaches its cluster.
type Access struct {
	Server        string
	CAData        []byte
	TLSServerName string
	// CertData and KeyData are the user's client certificate and key, in
	// PEM.
	CertData, KeyData []byte
	// Token is the user's bearer token.
	Token string
}

// file is the part of a kubeconfig that Parse reads.
type file struct {
	CurrentContext string `json:"current-context"`
	Contexts       []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster string `json:"cluster"`
			User    string `json:"user"`
		} `json:"context"`
	} `json:"contexts"`
	Clusters []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server        string `json:"server"`
			CAData        string `json:"certificate-authority-data"`
			TLSServerName string `json:"tls-server-name"`
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			CertData string `json:"client-certificate-data"`
			KeyData  string `json:"client-key-data"`
			Token    string `json:"token"`
		} `json:"user"`
	} `json:"users"`
}

// Parse reads a kubeconfig in YAML or JSON. It follows the current context,
// or the first one when none is current, and without contexts takes the
// first cluster and user.
func Parse(data []byte) (*Access, error) {
	var kc file
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return nil, errors.New("the kubeconfig has no clusters")
	}
	clusterName := kc.Clusters[0].Name
	userName := ""
	if len(kc.Users) > 0 {
		userName = kc.Users[0].Name
	}
	for i, c := range kc.Contexts {
		if c.Name == kc.CurrentContext || (i == 0 && kc.CurrentContext == "") {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	acc := &Access{}
	found := false
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		found = true
		acc.Server, acc.TLSServerName = c.Cluster.Server, c.Cluster.TLSServerName
		var err error
		if acc.CAData, err = decode(c.Cluster.CAData); err != nil {
			return nil, fmt.Errorf("reading the CA of cluster %s: %w", c.Name, err)
		}
		break
	}
	if !found {
		return nil, fmt.Errorf("the kubeconfig has no cluster named %q", clusterName)
	}
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		var err error
		if acc.CertData, err = decode(u.User.CertData); err != nil {
			return nil, fmt.Errorf("reading the client certificate of user %s: %w", u.Name, err)
		}
		if acc.KeyData, err = decode(u.User.KeyData); err != nil {
			return nil, fmt.Errorf("reading the client key of user %s: %w", u.Name, err)
		}
		acc.Token = u.User.Token
		break
	}
	return acc, nil
}

// decode decodes a kubeconfig *-data field, which is base64.
func decode(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
