/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
)

// The credential modes (plan 25, section 4: decided 2026-11-14). The instance
// principal and OKE workload identity modes are deferred.
const (
	AuthAPIKey                     = "apiKey"
	AuthWorkloadIdentityFederation = "workloadIdentityFederation"
)

// AuthModes are the supported modes, in the order the messages name them.
var AuthModes = []string{AuthAPIKey, AuthWorkloadIdentityFederation}

// Keys of the files of an API-key Secret (the chart mounts it as a directory),
// and the environment variables of the environment source.
const (
	SecretTenancy     = "tenancy"
	SecretUser        = "user"
	SecretFingerprint = "fingerprint"
	SecretRegion      = "region"
	SecretKey         = "key"
	// SecretPassphrase is optional: the passphrase of an encrypted key.
	SecretPassphrase = "passphrase"
)

// CredentialConfig is what the controller's options say about the identity.
type CredentialConfig struct {
	// Mode is AuthAPIKey (the default when empty) or AuthWorkloadIdentityFederation.
	Mode string

	// API key: the first of these that is set wins. SecretDir is a directory with
	// one file per key (tenancy, user, fingerprint, region, key); ConfigFile is an
	// OCI config file (with Profile, default DEFAULT; a profile with a
	// security_token_file is a session-token profile, the development path);
	// otherwise the environment (OCI_CLI_TENANCY, OCI_CLI_USER,
	// OCI_CLI_FINGERPRINT, OCI_CLI_REGION, OCI_CLI_KEY_FILE or
	// OCI_CLI_KEY_CONTENT, optional OCI_CLI_PASSPHRASE).
	SecretDir  string
	ConfigFile string
	Profile    string

	// Workload identity federation: the token file (the projected ServiceAccount
	// token of a cluster, or the OIDC token of a CI job; read at every exchange),
	// exchanged at the identity domain's token endpoint for a short-lived user
	// principal session token with the confidential application's client ID and
	// secret (read from ClientSecretFile). Region is the home region.
	TokenFile        string
	IdentityDomain   string
	ClientID         string
	ClientSecretFile string
	Region           string

	// Getenv reads the environment; nil is os.Getenv.
	Getenv func(string) string
}

// NewConfigurationProvider builds the SDK's configuration provider for the
// options. It reads the files it needs once, to fail at start-up with a message
// naming the missing piece, but makes no network call.
func NewConfigurationProvider(c CredentialConfig) (common.ConfigurationProvider, error) {
	switch c.Mode {
	case "", AuthAPIKey:
		return c.apiKey()
	case AuthWorkloadIdentityFederation:
		return c.federation()
	case "instancePrincipal", "okeWorkloadIdentity":
		return nil, fmt.Errorf("the OCI credentials mode %q is not supported yet; use one of %s",
			c.Mode, strings.Join(AuthModes, ", "))
	}
	return nil, fmt.Errorf("unknown OCI credentials mode %q; use one of %s", c.Mode, strings.Join(AuthModes, ", "))
}

func (c CredentialConfig) env(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c CredentialConfig) apiKey() (common.ConfigurationProvider, error) {
	switch {
	case c.SecretDir != "":
		return c.fromSecretDir()
	case c.ConfigFile != "":
		return c.fromConfigFile()
	}
	return c.fromEnvironment()
}

func readTrim(dir, name string, required bool) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if !required && os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading the key %q of the OCI credentials Secret: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c CredentialConfig) fromSecretDir() (common.ConfigurationProvider, error) {
	v := map[string]string{}
	for _, k := range []string{SecretTenancy, SecretUser, SecretFingerprint, SecretRegion} {
		s, err := readTrim(c.SecretDir, k, true)
		if err != nil {
			return nil, err
		}
		v[k] = s
	}
	// The key keeps its newlines: PEM.
	key, err := os.ReadFile(filepath.Join(c.SecretDir, SecretKey))
	if err != nil {
		return nil, fmt.Errorf("reading the key %q of the OCI credentials Secret: %w", SecretKey, err)
	}
	pass, err := readTrim(c.SecretDir, SecretPassphrase, false)
	if err != nil {
		return nil, err
	}
	return rawProvider(v[SecretTenancy], v[SecretUser], v[SecretRegion], v[SecretFingerprint], string(key), pass)
}

func (c CredentialConfig) fromEnvironment() (common.ConfigurationProvider, error) {
	var missing []string
	get := func(k string) string {
		v := c.env(k)
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	tenancy, user := get("OCI_CLI_TENANCY"), get("OCI_CLI_USER")
	fingerprint, region := get("OCI_CLI_FINGERPRINT"), get("OCI_CLI_REGION")
	key := c.env("OCI_CLI_KEY_CONTENT")
	if key == "" {
		if f := c.env("OCI_CLI_KEY_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("reading OCI_CLI_KEY_FILE: %w", err)
			}
			key = string(b)
		} else {
			missing = append(missing, "OCI_CLI_KEY_FILE or OCI_CLI_KEY_CONTENT")
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the OCI API key is not configured: set %s, or a credentials directory "+
			"(--oci-credentials-dir) or a config file (--oci-config-file)", strings.Join(missing, ", "))
	}
	return rawProvider(tenancy, user, region, fingerprint, key, c.env("OCI_CLI_PASSPHRASE"))
}

func rawProvider(tenancy, user, region, fingerprint, key, passphrase string) (common.ConfigurationProvider, error) {
	var pass *string
	if passphrase != "" {
		pass = &passphrase
	}
	p := common.NewRawConfigurationProvider(tenancy, user, region, fingerprint, key, pass)
	// Fail now, not at the first call, on a key that does not parse.
	if _, err := p.PrivateRSAKey(); err != nil {
		return nil, fmt.Errorf("the OCI private key: %w", err)
	}
	return p, nil
}

func (c CredentialConfig) fromConfigFile() (common.ConfigurationProvider, error) {
	profile := c.Profile
	if profile == "" {
		profile = "DEFAULT"
	}
	pass := c.env("OCI_CLI_PASSPHRASE")
	var (
		p   common.ConfigurationProvider
		err error
	)
	if hasSessionToken(c.ConfigFile, profile) {
		p, err = common.ConfigurationProviderForSessionTokenWithProfile(c.ConfigFile, profile, pass)
	} else {
		p, err = common.ConfigurationProviderFromFileWithProfile(c.ConfigFile, profile, pass)
	}
	if err != nil {
		return nil, fmt.Errorf("the OCI config file %s, profile %s: %w", c.ConfigFile, profile, err)
	}
	// The SDK reads the file at the first call: fail now on a missing profile or key.
	if _, err := p.TenancyOCID(); err != nil {
		return nil, fmt.Errorf("the OCI config file %s, profile %s: %w", c.ConfigFile, profile, err)
	}
	if _, err := p.Region(); err != nil {
		return nil, fmt.Errorf("the OCI config file %s, profile %s: %w", c.ConfigFile, profile, err)
	}
	if _, err := p.PrivateRSAKey(); err != nil {
		return nil, fmt.Errorf("the OCI config file %s, profile %s: %w", c.ConfigFile, profile, err)
	}
	return p, nil
}

// hasSessionToken reports whether the profile of the config file names a
// security_token_file: a session-token profile.
func hasSessionToken(path, profile string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			in = strings.TrimSpace(line[1:len(line)-1]) == profile
		case in && strings.HasPrefix(line, "security_token_file"):
			k, _, _ := strings.Cut(line, "=")
			if strings.TrimSpace(k) == "security_token_file" {
				return true
			}
		}
	}
	return false
}

// fileIssuer is the token issuer of the federation: it reads the token file at
// every exchange, so a projected token that the kubelet rotates is always the
// current one.
type fileIssuer struct{ path string }

func (f fileIssuer) GetToken() (string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("reading the OCI federation token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the OCI federation token file %s is empty", f.path)
	}
	return t, nil
}

// upstTokenType is the token the exchange asks for: a user principal session
// token (plan 25, section 4; UNVERIFIED with the real service, spike S11, as are
// the audience the identity domain expects of the subject token and the lifetime
// of the session token: the SDK refreshes it).
const upstTokenType = "urn:oci:token-type:oci-upst"

// subjectTokenType says that the token the exchange sends is a JWT: the identity
// domain's token exchange requires it (the SDK's own examples set it; without it
// the parameter is not sent).
const subjectTokenType = "jwt"

// domainEndpoint checks the identity domain's URL: https and a host only. The
// SDK appends the token endpoint's path itself and refuses a URL that has one, at
// the first call; this fails at start-up instead.
func domainEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		u.User != nil {
		return "", fmt.Errorf("the OCI identity domain URL %q must be https://<host> only "+
			"(https://idcs-<id>.identity.oraclecloud.com, without /oauth2/v1/token)", raw)
	}
	return u.String(), nil
}

func (c CredentialConfig) federation() (common.ConfigurationProvider, error) {
	var missing []string
	for name, v := range map[string]string{
		"--oci-token-file": c.TokenFile, "--oci-identity-domain-url": c.IdentityDomain,
		"--oci-client-id": c.ClientID, "--oci-client-secret-file": c.ClientSecretFile, "--oci-region": c.Region,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("the OCI mode %s needs %s", AuthWorkloadIdentityFederation, strings.Join(missing, ", "))
	}
	domain, err := domainEndpoint(c.IdentityDomain)
	if err != nil {
		return nil, err
	}
	secret, err := os.ReadFile(c.ClientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("reading the OCI client secret file: %w", err)
	}
	if strings.TrimSpace(string(secret)) == "" {
		return nil, fmt.Errorf("the OCI client secret file %s is empty", c.ClientSecretFile)
	}
	issuer := fileIssuer{c.TokenFile}
	if _, err := issuer.GetToken(); err != nil {
		return nil, err
	}
	return auth.TokenExchangeConfigurationProviderFromIssuer(issuer, auth.TokenExchangeBuilder{
		DomainUrl: domain, ClientId: c.ClientID, ClientSecret: strings.TrimSpace(string(secret)), Region: c.Region,
		RequestedTokenType: upstTokenType, SubjectTokenType: subjectTokenType,
	})
}
