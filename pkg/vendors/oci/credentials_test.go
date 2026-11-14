/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests build providers from files and variables made here: no key is real
// and nothing reaches a network.

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func checkIdentity(t *testing.T, c CredentialConfig) {
	t.Helper()
	p, err := NewConfigurationProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.TenancyOCID(); got != "ocid1.tenancy.oc1..aaaa" {
		t.Errorf("tenancy %q", got)
	}
	if got, _ := p.UserOCID(); got != "ocid1.user.oc1..aaaa" {
		t.Errorf("user %q", got)
	}
	if got, _ := p.Region(); got != "eu-frankfurt-1" {
		t.Errorf("region %q", got)
	}
	if _, err := p.PrivateRSAKey(); err != nil {
		t.Errorf("key: %v", err)
	}
}

func TestAPIKeyFromASecretDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, SecretTenancy, "ocid1.tenancy.oc1..aaaa\n")
	write(t, dir, SecretUser, "ocid1.user.oc1..aaaa\n")
	write(t, dir, SecretFingerprint, "aa:bb\n")
	write(t, dir, SecretRegion, "eu-frankfurt-1\n")
	write(t, dir, SecretKey, testKeyPEM(t))
	checkIdentity(t, CredentialConfig{SecretDir: dir})
	checkIdentity(t, CredentialConfig{Mode: AuthAPIKey, SecretDir: dir})

	if err := os.Remove(filepath.Join(dir, SecretUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigurationProvider(CredentialConfig{SecretDir: dir}); err == nil || !strings.Contains(err.Error(), `"user"`) {
		t.Errorf("a missing key is %v, want one naming it", err)
	}
	write(t, dir, SecretUser, "ocid1.user.oc1..aaaa")
	write(t, dir, SecretKey, "not a key")
	if _, err := NewConfigurationProvider(CredentialConfig{SecretDir: dir}); err == nil {
		t.Error("a key that does not parse was accepted")
	}
}

func TestAPIKeyFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	keyPEM := testKeyPEM(t)
	keyFile := write(t, dir, "key.pem", keyPEM)
	base := map[string]string{
		"OCI_CLI_TENANCY": "ocid1.tenancy.oc1..aaaa", "OCI_CLI_USER": "ocid1.user.oc1..aaaa",
		"OCI_CLI_FINGERPRINT": "aa:bb", "OCI_CLI_REGION": "eu-frankfurt-1",
	}
	with := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	checkIdentity(t, CredentialConfig{Getenv: with(map[string]string{"OCI_CLI_KEY_FILE": keyFile})})
	checkIdentity(t, CredentialConfig{Getenv: with(map[string]string{"OCI_CLI_KEY_CONTENT": keyPEM})})

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no key":           {nil, "OCI_CLI_KEY_FILE or OCI_CLI_KEY_CONTENT"},
		"no tenancy":       {map[string]string{"OCI_CLI_TENANCY": "", "OCI_CLI_KEY_CONTENT": keyPEM}, "OCI_CLI_TENANCY"},
		"key file missing": {map[string]string{"OCI_CLI_KEY_FILE": filepath.Join(dir, "nope")}, "OCI_CLI_KEY_FILE"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewConfigurationProvider(CredentialConfig{Getenv: with(tc.env)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestAPIKeyFromAConfigFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := write(t, dir, "key.pem", testKeyPEM(t))
	cfg := write(t, dir, "config", "[DEFAULT]\nuser=ocid1.user.oc1..aaaa\nfingerprint=aa:bb\ntenancy=ocid1.tenancy.oc1..aaaa\n"+
		"region=eu-frankfurt-1\nkey_file="+keyFile+"\n")
	checkIdentity(t, CredentialConfig{ConfigFile: cfg})
	checkIdentity(t, CredentialConfig{ConfigFile: cfg, Profile: "DEFAULT"})
	if _, err := NewConfigurationProvider(CredentialConfig{ConfigFile: cfg, Profile: "other"}); err == nil {
		t.Error("a profile the file does not have was accepted")
	}
	if _, err := NewConfigurationProvider(CredentialConfig{ConfigFile: filepath.Join(dir, "nope")}); err == nil {
		t.Error("a missing config file was accepted")
	}
}

func TestSessionTokenProfile(t *testing.T) {
	dir := t.TempDir()
	keyFile := write(t, dir, "key.pem", testKeyPEM(t))
	tokenFile := write(t, dir, "token", "header.payload.signature")
	cfg := write(t, dir, "config", "[DEFAULT]\nfingerprint=aa:bb\ntenancy=ocid1.tenancy.oc1..aaaa\nregion=eu-frankfurt-1\n"+
		"key_file="+keyFile+"\n[SESSION]\nfingerprint=aa:bb\ntenancy=ocid1.tenancy.oc1..aaaa\nregion=eu-frankfurt-1\n"+
		"key_file="+keyFile+"\nsecurity_token_file="+tokenFile+"\n")
	if hasSessionToken(cfg, "DEFAULT") || !hasSessionToken(cfg, "SESSION") || hasSessionToken(cfg, "none") ||
		hasSessionToken(filepath.Join(dir, "nope"), "SESSION") {
		t.Error("hasSessionToken does not find the session profile and only it")
	}
	// The session provider reads the token file as a JWT only when it signs: building it succeeds.
	if _, err := NewConfigurationProvider(CredentialConfig{ConfigFile: cfg, Profile: "SESSION"}); err != nil {
		t.Errorf("session-token profile: %v", err)
	}
}

func TestWorkloadIdentityFederation(t *testing.T) {
	dir := t.TempDir()
	tokenFile := write(t, dir, "token", "a.b.c\n")
	secretFile := write(t, dir, "secret", "s3cret\n")
	good := CredentialConfig{
		Mode: AuthWorkloadIdentityFederation, TokenFile: tokenFile, IdentityDomain: "https://idcs-example.identity.oraclecloud.com/",
		ClientID: "client", ClientSecretFile: secretFile, Region: "eu-frankfurt-1",
	}
	p, err := NewConfigurationProvider(good)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := p.Region(); err != nil || r != "eu-frankfurt-1" {
		t.Errorf("region %q, %v", r, err)
	}
	// The token file is read at every exchange.
	iss := fileIssuer{tokenFile}
	write(t, dir, "token", "d.e.f")
	if tok, err := iss.GetToken(); err != nil || tok != "d.e.f" {
		t.Errorf("issuer gave %q, %v", tok, err)
	}

	for name, mutate := range map[string]func(c *CredentialConfig){
		"no token file":     func(c *CredentialConfig) { c.TokenFile = "" },
		"no domain":         func(c *CredentialConfig) { c.IdentityDomain = "" },
		"no client":         func(c *CredentialConfig) { c.ClientID = "" },
		"no secret file":    func(c *CredentialConfig) { c.ClientSecretFile = "" },
		"no region":         func(c *CredentialConfig) { c.Region = "" },
		"secret file gone":  func(c *CredentialConfig) { c.ClientSecretFile = filepath.Join(dir, "nope") },
		"token file gone":   func(c *CredentialConfig) { c.TokenFile = filepath.Join(dir, "nope") },
		"empty secret file": func(c *CredentialConfig) { c.ClientSecretFile = write(t, dir, "empty", "\n") },
		"token path in url": func(c *CredentialConfig) { c.IdentityDomain += "oauth2/v1/token" },
		"plain http url":    func(c *CredentialConfig) { c.IdentityDomain = "http://idcs-example.identity.oraclecloud.com" },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			if _, err := NewConfigurationProvider(c); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// TestWorkloadIdentityFederationExchange runs one exchange against a local identity domain
// (a TLS server on the loopback interface) and checks the request the SDK sends for the
// provider: the token endpoint, the client's basic credentials and the form of a JWT
// exchanged for a user principal session token.
func TestWorkloadIdentityFederationExchange(t *testing.T) {
	var got struct {
		path, auth string
		form       map[string][]string
	}
	seg := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	upst := seg(map[string]any{"alg": "none"}) + "." + seg(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(), "tenant": "ocid1.tenancy.oc1..aaaa", "sub": "ocid1.user.oc1..bbbb",
	}) + ".sig"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseForm(); err == nil {
			got.form = r.PostForm
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":%q}`, upst)
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := write(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	t.Setenv("OCI_DEFAULT_CERTS_PATH", ca)

	p, err := NewConfigurationProvider(CredentialConfig{
		Mode: AuthWorkloadIdentityFederation, TokenFile: write(t, dir, "token", "a.b.c\n"), IdentityDomain: srv.URL,
		ClientID: "client", ClientSecretFile: write(t, dir, "secret", "s3cret\n"), Region: "eu-frankfurt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenancy, err := p.TenancyOCID()
	if err != nil || tenancy != "ocid1.tenancy.oc1..aaaa" {
		t.Fatalf("tenancy %q, %v", tenancy, err)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("client:s3cret"))
	if got.path != "/oauth2/v1/token" || got.auth != wantAuth {
		t.Errorf("request to %q with %q", got.path, got.auth)
	}
	for k, want := range map[string]string{
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "requested_token_type": upstTokenType,
		"subject_token_type": "jwt", "subject_token": "a.b.c",
	} {
		if v := got.form[k]; len(v) != 1 || v[0] != want {
			t.Errorf("form %s = %q, want %q", k, v, want)
		}
	}
	if len(got.form["public_key"]) != 1 || got.form["public_key"][0] == "" {
		t.Error("the exchange sent no public key")
	}
}

func TestUnsupportedModesNameTheSupportedOnes(t *testing.T) {
	for _, mode := range []string{"instancePrincipal", "okeWorkloadIdentity", "nonsense"} {
		_, err := NewConfigurationProvider(CredentialConfig{Mode: mode})
		if err == nil || !strings.Contains(err.Error(), "apiKey, workloadIdentityFederation") || !strings.Contains(err.Error(), mode) {
			t.Errorf("mode %s: %v", mode, err)
		}
	}
}
