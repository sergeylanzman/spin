// Copyright (c) 2018, Google, Inc.
// Copyright (c) 2019, Noel Cower.
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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mitchellh/go-homedir"
	"github.com/spinnaker/spin/cmd/output"
	"github.com/spinnaker/spin/config"
	iap "github.com/spinnaker/spin/config/auth/iap"
	"github.com/spinnaker/spin/version"
	"sigs.k8s.io/yaml"

	gate "github.com/spinnaker/spin/gateapi"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// defaultConfigFileMode is the default file mode used for config files. This corresponds to
	// the Unix file permissions u=rw,g=,o= so that config files with cached tokens, at least by
	// default, are only readable by the user that owns the config file.
	defaultConfigFileMode os.FileMode = 0600 // u=rw,g=,o=
)

// GatewayClient is the wrapper with authentication
type GatewayClient struct {
	// The exported fields below should be set by anyone using a command
	// with an GatewayClient field. These are expected to be set externally
	// (not from within the command itself).

	// Generate Gate Api client.
	*gate.APIClient

	// Spin CLI configuration.
	Config config.Config

	// Context for OAuth2 access token.
	Context context.Context

	// This is the set of flags global to the command parser.
	gateEndpoint string

	ignoreCertErrors bool

	// Location of the spin config.
	configLocation string

	// Raw Http Client to do OAuth2 login.
	httpClient *http.Client

	ui output.Ui
}

func (m *GatewayClient) GateEndpoint() string {
	if m.Config.Gate.Endpoint == "" && m.gateEndpoint == "" {
		return "http://localhost:8084"
	}
	if m.gateEndpoint != "" {
		return m.gateEndpoint
	}
	return m.Config.Gate.Endpoint
}

// Create new spinnaker gateway client with flag
func NewGateClient(ui output.Ui, gateEndpoint, defaultHeaders, configLocation string, ignoreCertErrors bool) (*GatewayClient, error) {
	gateClient := &GatewayClient{
		gateEndpoint:     gateEndpoint,
		ignoreCertErrors: ignoreCertErrors,
		ui:               ui,
	}

	err := userConfig(gateClient, configLocation)
	if err != nil {
		return nil, err
	}

	// Api client initialization.
	httpClient, err := gateClient.initializeClient()
	if err != nil {
		ui.Error("Could not initialize http client, failing.")
		return nil, err
	}

	gateClient.httpClient = httpClient

	err = gateClient.authenticateOAuth2()
	if err != nil {
		ui.Error("OAuth2 Authentication failed.")
		return nil, err
	}

	err = gateClient.authenticateGoogleServiceAccount()
	if err != nil {
		ui.Error(fmt.Sprintf("Google service account authentication failed: %v", err))
		return nil, err
	}

	if err = gateClient.authenticateLdap(); err != nil {
		ui.Error("LDAP Authentication Failed")
		return nil, err
	}

	m := make(map[string]string)

	if defaultHeaders != "" {
		headers := strings.Split(defaultHeaders, ",")
		for _, element := range headers {
			header := strings.SplitN(element, "=", 2)
			if len(header) != 2 {
				return nil, fmt.Errorf("Bad default-header value, use key=value form: %s", element)
			}
			m[strings.TrimSpace(header[0])] = strings.TrimSpace(header[1])
		}
	}

	cfg := &gate.Configuration{
		BasePath:      gateClient.GateEndpoint(),
		DefaultHeader: m,
		UserAgent:     fmt.Sprintf("%s/%s", version.UserAgent, version.String()),
		HTTPClient:    httpClient,
	}
	gateClient.APIClient = gate.NewAPIClient(cfg)

	// TODO: Verify version compatibility between Spin CLI and Gate.
	_, _, err = gateClient.VersionControllerApi.GetVersionUsingGET(gateClient.Context)
	if err != nil {
		ui.Error("Could not reach Gate, please ensure it is running. Failing.")
		return nil, err
	}

	return gateClient, nil
}

func userConfig(gateClient *GatewayClient, configLocation string) error {
	if configLocation != "" {
		gateClient.configLocation = configLocation
	} else {
		userHome := ""
		usr, err := user.Current()
		if err != nil {
			// Fallback by trying to read $HOME
			userHome = os.Getenv("HOME")
			if userHome != "" {
				err = nil
			} else {
				gateClient.ui.Error("Could not read current user from environment, failing.")
				return err
			}
		} else {
			userHome = usr.HomeDir
		}
		gateClient.configLocation = filepath.Join(userHome, ".spin", "config")
	}

	yamlFile, err := ioutil.ReadFile(gateClient.configLocation)
	if err != nil {
		return err
	}
	if yamlFile != nil {
		err = yaml.UnmarshalStrict([]byte(os.ExpandEnv(string(yamlFile))), &gateClient.Config)
		if err != nil {
			gateClient.ui.Error(fmt.Sprintf("Could not deserialize config file with contents: %s, failing.", yamlFile))
			return err
		}
	} else {
		gateClient.Config = config.Config{}
	}
	return nil
}

func (m *GatewayClient) initializeClient() (*http.Client, error) {
	auth := m.Config.Auth
	cookieJar, _ := cookiejar.New(nil)
	client := http.Client{
		Jar: cookieJar,
	}

	if m.ignoreCertErrors {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	switch {
	case auth != nil && auth.Enabled && auth.X509 != nil:
		X509 := auth.X509
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{},
		}

		if !X509.IsValid() {
			// Misconfigured.
			return nil, errors.New("Incorrect x509 auth configuration.\nMust specify certPath/keyPath or cert/key pair.")
		}
		switch {
		case X509.CertPath != "" && X509.KeyPath != "":
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
		case X509.Cert != "" && X509.Key != "":
			certBytes := []byte(X509.Cert)
			keyBytes := []byte(X509.Key)
			cert, err := tls.X509KeyPair(certBytes, keyBytes)
			if err != nil {
				return nil, err
			}

			return m.initializeX509Config(client, certBytes, cert), nil
		default:
			// Misconfigured.
			return nil, errors.New("Incorrect x509 auth configuration.\nMust specify certPath/keyPath or cert/key pair.")
		}
	case auth != nil && auth.Enabled && auth.Iap != nil:
		accessToken, err := m.authenticateIAP()
		m.Context = context.WithValue(context.Background(), gate.ContextAccessToken, accessToken)
		return &client, err
	case auth != nil && auth.Enabled && auth.Basic != nil:
		if !auth.Basic.IsValid() {
			return nil, errors.New("Incorrect Basic auth configuration. Must include username and password.")
		}
		m.Context = context.WithValue(context.Background(), gate.ContextBasicAuth, gate.BasicAuth{
			UserName: auth.Basic.Username,
			Password: auth.Basic.Password,
		})
		return &client, nil
	}
	return &client, nil
}

func (m *GatewayClient) initializeX509Config(client http.Client, clientCA []byte, cert tls.Certificate) *http.Client {
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

func (m *GatewayClient) authenticateOAuth2() error {
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
			tokenSource := config.TokenSource(context.Background(), token)
			newToken, err = tokenSource.Token()
			if err != nil {
				m.ui.Error(fmt.Sprintf("Could not refresh token from source: %v", tokenSource))
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

			verifier, verifierCode, err := m.generateCodeVerifier()
			if err != nil {
				return err
			}

			codeVerifier := oauth2.SetAuthURLParam("code_verifier", verifier)
			codeChallenge := oauth2.SetAuthURLParam("code_challenge", verifierCode)
			challengeMethod := oauth2.SetAuthURLParam("code_challenge_method", "S256")

			authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce, challengeMethod, codeChallenge)
			m.ui.Output(fmt.Sprintf("Navigate to %s and authenticate", authURL))
			code := m.prompt("Paste authorization code:")

			newToken, err = config.Exchange(context.Background(), code, codeVerifier)
			if err != nil {
				return err
			}
		}

		m.ui.Info("Caching oauth2 token.")
		OAuth2.CachedToken = newToken
		_ = m.writeYAMLConfig()

		m.login(newToken.AccessToken)
		m.Context = context.Background()
	}
	return nil
}

func (m *GatewayClient) authenticateIAP() (string, error) {
	auth := m.Config.Auth
	iapConfig := auth.Iap
	token, err := iap.GetIapToken(*iapConfig)
	return token, err
}

func (m *GatewayClient) authenticateGoogleServiceAccount() (err error) {
	auth := m.Config.Auth
	if auth == nil {
		return nil
	}

	gsa := auth.GoogleServiceAccount
	if !gsa.IsEnabled() {
		return nil
	}

	if gsa.CachedToken != nil && gsa.CachedToken.Valid() {
		return m.login(gsa.CachedToken.AccessToken)
	}
	gsa.CachedToken = nil

	var source oauth2.TokenSource
	if gsa.File == "" {
		source, err = google.DefaultTokenSource(context.Background(), "profile", "email")
	} else {
		serviceAccountJSON, ferr := ioutil.ReadFile(gsa.File)
		if ferr != nil {
			return ferr
		}
		source, err = google.JWTAccessTokenSourceFromJSON(serviceAccountJSON, "https://accounts.google.com/o/oauth2/v2/auth")
	}
	if err != nil {
		return err
	}

	token, err := source.Token()
	if err != nil {
		return err
	}

	if err := m.login(token.AccessToken); err != nil {
		return err
	}

	gsa.CachedToken = token
	m.Context = context.Background()

	// Cache token if login succeeded
	gsa.CachedToken = token
	_ = m.writeYAMLConfig()

	return nil
}

func (m *GatewayClient) login(accessToken string) error {
	loginReq, err := http.NewRequest("GET", m.GateEndpoint()+"/login", nil)
	if err != nil {
		return err
	}
	loginReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	m.httpClient.Do(loginReq) // Login to establish session.
	return nil
}

func (m *GatewayClient) authenticateLdap() error {
	auth := m.Config.Auth
	if auth != nil && auth.Enabled && auth.Ldap != nil {
		if auth.Ldap.Username == "" {
			auth.Ldap.Username = m.prompt("Username:")
		}

		if auth.Ldap.Password == "" {
			auth.Ldap.Password = m.securePrompt("Password:")
		}

		if !auth.Ldap.IsValid() {
			return errors.New("Incorrect LDAP auth configuration. Must include username and password.")
		}

		form := url.Values{}
		form.Add("username", auth.Ldap.Username)
		form.Add("password", auth.Ldap.Password)

		loginReq, err := http.NewRequest("POST", m.GateEndpoint()+"/login", strings.NewReader(form.Encode()))
		loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err != nil {
			return err
		}

		_, err = m.httpClient.Do(loginReq) // Login to establish session.

		if err != nil {
			return errors.New("ldap authentication failed")
		}

		m.Context = context.Background()
	}

	return nil
}

// writeYAMLConfig writes an updated YAML configuration file to the reciever's config file location.
// It returns an error, but the error may be ignored.
func (m *GatewayClient) writeYAMLConfig() error {
	// Write updated config file with u=rw,g=,o= permissions by default.
	// The default permissions should only be used if the file no longer exists.
	err := writeYAML(&m.Config, m.configLocation, defaultConfigFileMode)
	if err != nil {
		m.ui.Warn(fmt.Sprintf("Error caching oauth2 token: %v", err))
	}
	return err
}

func writeYAML(v interface{}, dest string, defaultMode os.FileMode) error {
	// Write config with cached token
	buf, err := yaml.Marshal(v)
	if err != nil {
		return err
	}

	info, err := os.Stat(dest)
	if err != nil && !os.IsNotExist(err) {
		return nil
	}
	mode := info.Mode()

	return ioutil.WriteFile(dest, buf, mode)
}

// generateCodeVerifier generates an OAuth2 code verifier
// in accordance to https://www.oauth.com/oauth2-servers/pkce/authorization-request and
// https://tools.ietf.org/html/rfc7636#section-4.1.
func (m *GatewayClient) generateCodeVerifier() (verifier string, code string, err error) {
	randomBytes := make([]byte, 64)
	if _, err := rand.Read(randomBytes); err != nil {
		m.ui.Error("Could not generate random string for code_verifier")
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(randomBytes)
	verifierHash := sha256.Sum256([]byte(verifier))
	code = base64.RawURLEncoding.EncodeToString(verifierHash[:]) // Slice for type conversion
	return verifier, code, nil
}

func (m *GatewayClient) prompt(inputMsg string) string {
	reader := bufio.NewReader(os.Stdin)
	m.ui.Output(inputMsg)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func (m *GatewayClient) securePrompt(inputMsg string) string {
	m.ui.Output(inputMsg)
	byteSecret, _ := terminal.ReadPassword(syscall.Stdin)
	secret := string(byteSecret)
	return strings.TrimSpace(secret)
}
