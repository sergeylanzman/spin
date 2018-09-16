// Copyright (c) 2018, Google, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package gateclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	_ "net/http/pprof"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/spinnaker/spin/util"

	"github.com/mitchellh/go-homedir"
	"github.com/spinnaker/spin/config"
	gate "github.com/spinnaker/spin/gateapi"
	"github.com/spinnaker/spin/version"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"
)

// ApiMeta is the state & utility shared by our commands.
type ApiMeta struct {
	// The exported fields below should be set by anyone using a command
	// with an ApiMeta field. These are expected to be set externally
	// (not from within the command itself).

	// Gate Api client.
	GateClient *gate.APIClient

	// Spin CLI configuration.
	Config config.Config

	// Context for OAuth2 access token.
	Context context.Context

	// This is the set of flags global to the command parser.
	gateEndpoint string

	ignoreCertErrors bool

	// Location of the spin config.
	configLocation string
}

func (m *ApiMeta) GateEndpoint() string {
	if m.Config.Gate.Endpoint == "" && m.gateEndpoint == "" {
		return "http://localhost:8084"
	}
	if m.gateEndpoint != "" {
		return m.gateEndpoint
	}
	return m.Config.Gate.Endpoint
}

// Process will process the meta-parameters out of the arguments. This
// potentially modifies the args in-place. It will return the resulting slice.
// NOTE: This expects the flag set to be parsed prior to invoking it.
func NewGateClient(flags *pflag.FlagSet) (*gate.APIClient, error) {
	gateEndpoint, err := flags.GetString("gate-endpoint")
	if err != nil {
		util.UI.Error(fmt.Sprintf("%s\n", err))
		return nil, err
	}
	configLocationFlag, err := flags.GetString("config")
	if err != nil {
		util.UI.Error(fmt.Sprintf("%s\n", err))
		return nil, err
	}
	ignoreCertErrors, err := flags.GetBool("insecure")
	if err != nil {
		util.UI.Error(fmt.Sprintf("%s\n", err))
		return nil, err
	}
	m := ApiMeta{
		gateEndpoint:     gateEndpoint,
		ignoreCertErrors: ignoreCertErrors,
	}

	// CLI configuration.
	if configLocationFlag != "" {
		m.configLocation = configLocationFlag
	} else {
		userHome := ""
		usr, err := user.Current()
		if err != nil {
			// Fallback by trying to read $HOME
			userHome = os.Getenv("HOME")
			if userHome != "" {
				err = nil
			} else {
				util.UI.Error(fmt.Sprintf("Could not read current user from environment, failing."))
				return nil, err
			}
		} else {
			userHome = usr.HomeDir
		}
		m.configLocation = filepath.Join(userHome, ".spin", "config")
	}
	yamlFile, err := ioutil.ReadFile(m.configLocation)
	if err != nil {
		util.UI.Warn(fmt.Sprintf("Could not read configuration file from %s.", m.configLocation))
	}

	if yamlFile != nil {
		err = yaml.UnmarshalStrict(yamlFile, &m.Config)
		if err != nil {
			util.UI.Error(fmt.Sprintf("Could not deserialize config file with contents: %d, failing.", yamlFile))
			return nil, err
		}
	} else {
		m.Config = config.Config{}
	}

	// Api client initialization.
	err = m.Authenticate()
	if err != nil {
		util.UI.Error(fmt.Sprintf("OAuth2 Authentication failed."))
		return nil, err
	}

	client, err := m.InitializeClient()
	if err != nil {
		util.UI.Error(fmt.Sprintf("Could not initialize http client, failing."))
		return nil, err
	}

	cfg := &gate.Configuration{
		BasePath:      m.GateEndpoint(),
		DefaultHeader: make(map[string]string),
		UserAgent:     fmt.Sprintf("%s/%s", version.UserAgent, version.String()),
		HTTPClient:    client,
	}
	gateClient := gate.NewAPIClient(cfg)
	return gateClient, nil
}

func (m *ApiMeta) InitializeClient() (*http.Client, error) {
	auth := m.Config.Auth
	cookieJar, _ := cookiejar.New(nil)
	client := http.Client{
		Jar: cookieJar,
	}

	if m.ignoreCertErrors {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	if auth != nil && auth.Enabled && auth.X509 != nil {
		X509 := auth.X509
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{},
		}

		if !X509.IsValid() {
			// Misconfigured.
			return nil, errors.New("Incorrect x509 auth configuration.\nMust specify certPath/keyPath or cert/key pair.")
		}

		if X509.CertPath != "" && X509.KeyPath != "" {
			certPath, err := homedir.Expand(X509.CertPath)
			if err != nil {
				return nil, err
			}
			keyPath, err := homedir.Expand(X509.KeyPath)
			if err != nil {
				return nil, err
			}

			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return nil, err
			}

			clientCA, err := ioutil.ReadFile(certPath)
			if err != nil {
				return nil, err
			}

			return m.initializeX509Config(client, clientCA, cert), nil
		} else if X509.Cert != "" && X509.Key != "" {
			certBytes := []byte(X509.Cert)
			keyBytes := []byte(X509.Key)
			cert, err := tls.X509KeyPair(certBytes, keyBytes)
			if err != nil {
				return nil, err
			}

			return m.initializeX509Config(client, certBytes, cert), nil
		} else {
			// Misconfigured.
			return nil, errors.New("Incorrect x509 auth configuration.\nMust specify certPath/keyPath or cert/key pair.")
		}
	} else if auth != nil && auth.Enabled && auth.Basic != nil {
		if !auth.Basic.IsValid() {
			return nil, errors.New("incorrect Basic auth configuration. Must include username and password")
		}
		m.Context = context.WithValue(context.Background(), gate.ContextBasicAuth, gate.BasicAuth{
			UserName: auth.Basic.Username,
			Password: auth.Basic.Password,
		})
		return &client, nil
	} else {
		return &client, nil
	}
}

func (m *ApiMeta) initializeX509Config(client http.Client, clientCA []byte, cert tls.Certificate) *http.Client {
	clientCertPool := x509.NewCertPool()
	clientCertPool.AppendCertsFromPEM(clientCA)

	client.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS12
	client.Transport.(*http.Transport).TLSClientConfig.PreferServerCipherSuites = true
	client.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{cert}
	if m.ignoreCertErrors {
		client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	}
	return &client
}

func (m *ApiMeta) Authenticate() error {
	auth := m.Config.Auth
	if auth != nil && auth.Enabled && auth.OAuth2 != nil {
		OAuth2 := auth.OAuth2
		if !OAuth2.IsValid() {
			// TODO(jacobkiefer): Improve this error message.
			return errors.New("incorrect OAuth2 auth configuration")
		}

		config := &oauth2.Config{
			ClientID:     OAuth2.ClientId,
			ClientSecret: OAuth2.ClientSecret,
			RedirectURL:  "http://localhost:8085",
			Scopes:       OAuth2.Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  OAuth2.AuthUrl,
				TokenURL: OAuth2.TokenUrl,
			},
		}
		var newToken *oauth2.Token
		var err error

		if auth.OAuth2.CachedToken != nil {
			// Look up cached credentials to save oauth2 roundtrip.
			token := auth.OAuth2.CachedToken
			tokenSource := config.TokenSource(context.TODO(), token)
			newToken, err = tokenSource.Token()
			if err != nil {
				util.UI.Error(fmt.Sprintf("Could not refresh token from source: %v", tokenSource))
				return err
			}
		} else {
			// Do roundtrip.
			http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				code := r.FormValue("code")
				fmt.Fprintln(w, code)
			}))
			go http.ListenAndServe(":8085", nil)
			// Note: leaving server connection open for scope of request, will be reaped on exit.

			verifierBytes := make([]byte, 5)
			if _, err := rand.Read(verifierBytes); err != nil {
				util.UI.Error("Could not generate random string for code_verifier")
				return err
			}
			verifier := string(verifierBytes)
			codeChallenge := oauth2.SetAuthURLParam("code_challenge", verifier)
			codeVerifier := oauth2.SetAuthURLParam("code_verifier", verifier)

			authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce, codeChallenge)
			util.UI.Output(fmt.Sprintf("Navigate to %s and authenticate", authURL))
			code := m.Prompt()

			newToken, err = config.Exchange(context.TODO(), code, codeVerifier)
			if err != nil {
				return err
			}
		}

		util.UI.Info("Caching oauth2 token.")
		OAuth2.CachedToken = newToken
		buf, _ := yaml.Marshal(&m.Config)
		info, _ := os.Stat(m.configLocation)
		ioutil.WriteFile(m.configLocation, buf, info.Mode())
		m.Context = context.WithValue(context.Background(), gate.ContextAccessToken, newToken.AccessToken)
	}
	return nil
}

func (m *ApiMeta) Prompt() string {
	reader := bufio.NewReader(os.Stdin)
	util.UI.Output(fmt.Sprintf("Paste authorization code:"))
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}
