/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/coreos/butane/config"
	"github.com/coreos/butane/config/common"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
)

//go:embed templates/*.bu templates/*.sh
var templates embed.FS

// Size limits of the user-data. The limit is the smallest of the vendors, AWS's
// 16 KiB of raw user-data; the default templates are held well below it by a
// golden test, so that custom snippets have room.
const (
	MaxUserDataSize    = 16 * 1024
	DefaultBudgetBytes = 12 * 1024
)

// ignitionVersion is the version of the outer Ignition config. Flatcar 4757.2.3
// ships Ignition 2.24.0 (specs up to 3.5.0, overview section 15 Q3); Butane's
// flatcar variant 1.1.0 translates to 3.4.0, and a config merged into the outer
// one must not be newer.
const ignitionVersion = "3.4.0"

// Part is one of the translated configs that the user-data merges.
type Part struct {
	// Name is base, network or user.
	Name string
	// Ignition is the translated config (JSON), before compression.
	Ignition []byte
}

// Fragment is a Butane config a vendor's driver contributes (vendors.BootFragment):
// always-on fragments and the ones of the enabled hardenings.
type Fragment struct {
	Name   string
	Butane string
}

// Rendered is the user-data and the configs it merges.
type Rendered struct {
	UserData []byte
	Parts    []Part
}

// Sysext is a systemd system extension image installed on the node: a file
// below /opt/extensions pinned by its sha256 and linked into /etc/extensions,
// where systemd-sysext picks it up (the name must match the image's
// extension-release file).
type Sysext struct {
	Name   string
	URL    string
	SHA256 string
	// Path is where the image is stored, Link the symlink that activates it.
	Path string
	Link string
}

// TailscaleData are the variables of the tailscale template.
type TailscaleData struct {
	Version     string
	LoginServer string
	AuthKey     string
	// Tags are the ACL tags, comma separated.
	Tags   string
	Sysext Sysext
}

// TemplateData are the variables of the Butane templates (overview section
// 8.2). Templates are strict: a variable that does not exist is an error.
// Zone and InstanceType are the offering of the launch attempt: the user-data
// is rendered per attempt, because the kubelet's reservations (kube-reserved)
// and the device hints depend on the instance type (plan 06); they are empty
// in the trial rendering of the NodeClass controller.
type TemplateData struct {
	NodeName          string
	ProviderID        string
	APIServerEndpoint string
	// ClusterCA is the PEM bundle of the cluster CA.
	ClusterCA string
	// KubeletClientCA is the PEM CA of the API server's kubelet client
	// certificates when it differs from the cluster CA (empty: the same).
	KubeletClientCA string
	BootstrapToken  string
	Arch            string
	Vendor          string
	Region          string
	Zone            string
	InstanceType    string
	// Labels are the NodeClaim's labels, for templates that need them; they are
	// not passed to the kubelet.
	Labels map[string]string
	// KubeletConfig is the KubeletConfiguration YAML.
	KubeletConfig string
	// KubeletArgs are the flags that identify the node: --provider-id,
	// --hostname-override and --register-with-taints.
	KubeletArgs string
	// Sysexts are the Kubernetes sysext and the NodeClass's extra sysexts of the
	// node's architecture.
	Sysexts   []Sysext
	Tailscale TailscaleData
	// PoweroffAfter is poweroffAfter as a duration string and in seconds (0:
	// not set).
	PoweroffAfter        string
	PoweroffAfterSeconds int64
	// Volumes and Local are the first-boot volume unit (volumes template);
	// VolumeScript and LocalScript the scripts it installs.
	Volumes      []VolumeData
	Local        *LocalData
	VolumeScript string
	LocalScript  string
	// MountPaths are the mount paths of the volumes and local storage, space
	// separated, for the kubelet's RequiresMountsFor=.
	MountPaths string
}

// Network is the network part of the node's configuration.
type Network struct {
	Template v1alpha1.NetworkTemplateName
	// Tailscale holds the resolved tailscale settings (template tailscale).
	Tailscale TailscaleParams
	// Custom is the Butane template of the template custom.
	Custom string
	// CustomSource names where Custom came from, for error messages.
	CustomSource string
}

// TailscaleParams are the inputs of the tailscale template.
type TailscaleParams struct {
	Version     string
	LoginServer string
	AuthKey     string
	Tags        []string
	Sysext      artifacts.Artifact
}

// Params is everything the rendering of one node's user-data depends on. Render
// is a pure function of it.
type Params struct {
	NodeName     string
	ProviderID   string
	Vendor       string
	Region       string
	Zone         string
	InstanceType string
	Arch         string
	Labels       map[string]string
	// Taints are registered with the node by the kubelet.
	Taints []corev1.Taint

	APIServerEndpoint string
	ClusterCA         string
	KubeletClientCA   string
	BootstrapToken    string

	// Kubelet and the cluster DNS settings go into the KubeletConfiguration.
	Kubelet       v1alpha1.KubeletSpec
	ClusterDNS    []string
	ClusterDomain string

	// Kubernetes is the resolved Kubernetes sysext of the architecture.
	Kubernetes artifacts.Artifact
	// ExtraSysexts are spec.sysexts; the ones of other architectures are
	// ignored.
	ExtraSysexts []v1alpha1.Sysext

	Network Network
	// UserButane is spec.butane, empty when not set.
	UserButane string
	// UserSource names where UserButane came from, for error messages.
	UserSource string

	PoweroffAfter time.Duration

	// KubeReserved, when set, is the kubelet's kubeReserved for the instance
	// type: what the catalog assumed for scheduling, so that the node's
	// allocatable matches the scheduler's. It replaces Kubelet.KubeReserved.
	KubeReserved map[string]string
	// Volumes and Local are the device hints of the launch (00a section 11).
	Volumes []VolumeHint
	Local   *LocalHint
	// VendorFragments are the fragments the vendor's driver contributes (plan 14
	// A7). They merge after the base and volume configs and before the network
	// and the user's, so those override them; they are rendered with the same
	// variables as the user's Butane and count in the budget.
	VendorFragments []Fragment

	// Budget is the size above which Render fails; 0 means MaxUserDataSize.
	Budget int
}

// Render renders the user-data of a node: an Ignition config whose merge list
// holds the gzip and base64 encoded base, network and user configs.
func Render(p Params) (*Rendered, error) {
	data, err := p.templateData()
	if err != nil {
		return nil, err
	}

	var parts []Part
	base, err := renderBuiltin("base", "base.bu", data)
	if err != nil {
		return nil, err
	}
	parts = append(parts, Part{Name: "base", Ignition: base})

	if len(p.Volumes) > 0 || p.Local != nil {
		volumes, err := renderBuiltin("volumes", "volumes.bu", data)
		if err != nil {
			return nil, err
		}
		parts = append(parts, Part{Name: "volumes", Ignition: volumes})
	}

	for _, f := range p.VendorFragments {
		if strings.TrimSpace(f.Name) == "" {
			return nil, errors.New("a vendor fragment has no name")
		}
		frag, err := renderText("vendor fragment "+f.Name, f.Butane, data)
		if err != nil {
			return nil, err
		}
		parts = append(parts, Part{Name: "vendor-" + f.Name, Ignition: frag})
	}

	var network []byte
	switch p.Network.Template {
	case v1alpha1.NetworkTemplateTailscale:
		network, err = renderBuiltin("network", "tailscale.bu", data)
	case v1alpha1.NetworkTemplateDirect:
		network, err = renderBuiltin("network", "direct.bu", data)
	case v1alpha1.NetworkTemplateCustom:
		network, err = renderText("network ("+p.Network.CustomSource+")", p.Network.Custom, data)
	default:
		err = fmt.Errorf("unknown network template %q", p.Network.Template)
	}
	if err != nil {
		return nil, err
	}
	parts = append(parts, Part{Name: "network", Ignition: network})

	if strings.TrimSpace(p.UserButane) != "" {
		user, err := renderText("butane ("+p.UserSource+")", p.UserButane, data)
		if err != nil {
			return nil, err
		}
		parts = append(parts, Part{Name: "user", Ignition: user})
	}

	userData, err := merge(parts)
	if err != nil {
		return nil, err
	}
	budget := p.Budget
	if budget <= 0 {
		budget = MaxUserDataSize
	}
	if len(userData) > budget {
		return nil, fmt.Errorf("the user-data is %d bytes, over the budget of %d bytes (%s); "+
			"large payloads belong into sysexts referenced by URL and sha256",
			len(userData), budget, partSizes(parts))
	}
	return &Rendered{UserData: userData, Parts: parts}, nil
}

// argPattern is what may appear in a kubelet flag value: the flags go into a
// systemd unit, where whitespace, quotes, $ and % are special.
var argPattern = regexp.MustCompile(`^[A-Za-z0-9._:/=,@-]+$`)

func (p *Params) templateData() (*TemplateData, error) {
	for name, v := range map[string]string{
		"node name": p.NodeName, "provider ID": p.ProviderID, "API server endpoint": p.APIServerEndpoint,
		"cluster CA": p.ClusterCA, "bootstrap token": p.BootstrapToken, "architecture": p.Arch,
	} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("the %s is empty", name)
		}
	}
	if _, err := artifacts.SysextArch(p.Arch); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"node name": p.NodeName, "provider ID": p.ProviderID} {
		if !argPattern.MatchString(v) {
			return nil, fmt.Errorf("the %s %q has characters that cannot go on a kubelet command line", name, v)
		}
	}
	args := []string{"--provider-id=" + p.ProviderID, "--hostname-override=" + p.NodeName}
	if taints := formatTaints(p.Taints); taints != "" {
		if !argPattern.MatchString(taints) {
			return nil, fmt.Errorf("the taints %q have characters that cannot go on a kubelet command line", taints)
		}
		args = append(args, "--register-with-taints="+taints)
	}

	kubelet := p.Kubelet
	if len(p.KubeReserved) > 0 {
		kubelet.KubeReserved = p.KubeReserved
	}
	clientCAFile := ""
	if strings.TrimSpace(p.KubeletClientCA) != "" {
		clientCAFile = KubeletClientCAPath
	}
	kubeletConfig, err := KubeletConfiguration(kubelet, p.ClusterDNS, p.ClusterDomain, clientCAFile)
	if err != nil {
		return nil, err
	}
	sysexts, err := p.sysexts()
	if err != nil {
		return nil, err
	}

	d := &TemplateData{
		NodeName: p.NodeName, ProviderID: p.ProviderID, APIServerEndpoint: p.APIServerEndpoint,
		ClusterCA: strings.TrimSpace(p.ClusterCA) + "\n", BootstrapToken: p.BootstrapToken,
		KubeletClientCA: strings.TrimSpace(p.KubeletClientCA),
		Arch:            p.Arch, Vendor: p.Vendor, Region: p.Region, Zone: p.Zone, InstanceType: p.InstanceType,
		Labels: p.Labels, KubeletConfig: kubeletConfig, KubeletArgs: strings.Join(args, " "), Sysexts: sysexts,
	}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if len(p.Volumes) > 0 || p.Local != nil {
		if d.Volumes, d.Local, err = volumesData(p.Volumes, p.Local); err != nil {
			return nil, err
		}
		var paths []string
		for _, v := range d.Volumes {
			paths = append(paths, v.Where)
		}
		if d.Local != nil {
			for _, m := range d.Local.Mounts {
				paths = append(paths, m.Where)
			}
		}
		d.MountPaths = strings.Join(paths, " ")
		if len(d.Volumes) > 0 {
			script, err := templates.ReadFile("templates/scarpenter-volume.sh")
			if err != nil {
				return nil, fmt.Errorf("reading the volume script: %w", err)
			}
			d.VolumeScript = string(script)
		}
		if d.Local != nil {
			script, err := templates.ReadFile("templates/scarpenter-local-storage.sh")
			if err != nil {
				return nil, fmt.Errorf("reading the local storage script: %w", err)
			}
			d.LocalScript = string(script)
		}
	}
	if p.PoweroffAfter > 0 {
		d.PoweroffAfter = p.PoweroffAfter.String()
		d.PoweroffAfterSeconds = max(int64(p.PoweroffAfter/time.Second), 1)
	}
	if p.Network.Template == v1alpha1.NetworkTemplateTailscale {
		if d.Tailscale, err = p.tailscaleData(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (p *Params) tailscaleData() (TailscaleData, error) {
	ts := p.Network.Tailscale
	if strings.TrimSpace(ts.AuthKey) == "" || strings.ContainsAny(ts.AuthKey, " \t\r\n") {
		return TailscaleData{}, errors.New("the tailscale auth key is empty or contains whitespace")
	}
	if ts.Sysext.URL == "" || ts.Sysext.SHA256 == "" {
		return TailscaleData{}, errors.New("the tailscale sysext is not resolved")
	}
	for _, t := range ts.Tags {
		if !argPattern.MatchString(t) {
			return TailscaleData{}, fmt.Errorf("the tailscale tag %q is invalid", t)
		}
	}
	if ts.LoginServer != "" && !argPattern.MatchString(ts.LoginServer) {
		return TailscaleData{}, fmt.Errorf("the tailscale login server %q is invalid", ts.LoginServer)
	}
	return TailscaleData{
		Version: ts.Version, LoginServer: ts.LoginServer, AuthKey: ts.AuthKey, Tags: strings.Join(ts.Tags, ","),
		Sysext: builtinSysext("tailscale", ts.Sysext),
	}, nil
}

// builtinSysext places an image of the bakery: the file keeps its release file
// name.
func builtinSysext(name string, a artifacts.Artifact) Sysext {
	file := path.Base(a.URL)
	if !sysextFilePattern.MatchString(file) {
		file = name + ".raw"
	}
	return Sysext{Name: name, URL: a.URL, SHA256: a.SHA256,
		Path: "/opt/extensions/" + name + "/" + file, Link: "/etc/extensions/" + name + ".raw"}
}

var sysextFilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.raw$`)

func (p *Params) sysexts() ([]Sysext, error) {
	if p.Kubernetes.URL == "" || p.Kubernetes.SHA256 == "" {
		return nil, fmt.Errorf("the kubernetes sysext is not resolved for %s", p.Arch)
	}
	out := []Sysext{builtinSysext("kubernetes", p.Kubernetes)}
	taken := map[string]bool{"kubernetes": true}
	if p.Network.Template == v1alpha1.NetworkTemplateTailscale {
		taken["tailscale"] = true
	}
	for _, s := range p.ExtraSysexts {
		if s.Arch != p.Arch {
			continue
		}
		if taken[s.Name] {
			return nil, fmt.Errorf("the extra sysext %q collides with a sysext of the bootstrap", s.Name)
		}
		taken[s.Name] = true
		out = append(out, Sysext{Name: s.Name, URL: s.URL, SHA256: s.SHA256,
			Path: "/opt/extensions/" + s.Name + "/" + s.Name + ".raw", Link: "/etc/extensions/" + s.Name + ".raw"})
	}
	return out, nil
}

// formatTaints formats taints the way --register-with-taints takes them:
// key[=value]:effect, comma separated.
func formatTaints(taints []corev1.Taint) string {
	out := make([]string, 0, len(taints))
	for _, t := range taints {
		s := t.Key
		if t.Value != "" {
			s += "=" + t.Value
		}
		out = append(out, s+":"+string(t.Effect))
	}
	return strings.Join(out, ",")
}

var funcs = template.FuncMap{
	// q quotes a string as a YAML (JSON) double-quoted scalar.
	"q": func(s string) (string, error) {
		b, err := json.Marshal(s)
		return string(b), err
	},
	// indent pads every non-empty line with n spaces, for block scalars.
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = pad + l
			}
		}
		return strings.Join(lines, "\n")
	},
	// b64 base64 encodes a string.
	"b64": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
}

func renderBuiltin(part, file string, data *TemplateData) ([]byte, error) {
	src, err := templates.ReadFile("templates/" + file)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded template %s: %w", file, err)
	}
	return renderText(part+" (built-in "+file+")", string(src), data)
}

// renderText executes a Butane template and translates the result to Ignition.
func renderText(what, src string, data *TemplateData) ([]byte, error) {
	tmpl, err := template.New(what).Funcs(funcs).Option("missingkey=error").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parsing the %s template: %w", what, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing the %s template: %w", what, err)
	}
	return translate(what, buf.Bytes())
}

// supportedButane lists the Butane versions of the flatcar variant that
// translate to an Ignition version Flatcar's Ignition merges into the outer
// config (not the experimental one).
var supportedButane = map[string]bool{"1.0.0": true, "1.1.0": true}

// translate converts a Butane config to Ignition JSON, in strict mode: any
// warning is an error.
func translate(what string, src []byte) ([]byte, error) {
	var head struct{ Variant, Version string }
	if err := yaml.Unmarshal(src, &head); err != nil {
		return nil, fmt.Errorf("the %s config is not valid YAML: %w", what, err)
	}
	if head.Variant != "flatcar" || !supportedButane[head.Version] {
		return nil, fmt.Errorf(
			"the %s config must be 'variant: flatcar' with 'version: 1.0.0' or '1.1.0', got variant %q version %q",
			what, head.Variant, head.Version)
	}
	out, rep, err := config.TranslateBytes(src, common.TranslateBytesOptions{
		// Inline contents are not compressed by Butane: the whole config is
		// compressed once, which compresses better and keeps the output stable.
		TranslateOptions: common.TranslateOptions{NoResourceAutoCompression: true},
	})
	if err != nil {
		return nil, fmt.Errorf("translating the %s config: %w", what, withReport(err, rep.String()))
	}
	if len(rep.Entries) > 0 {
		return nil, fmt.Errorf("translating the %s config (strict mode): %s", what, rep.String())
	}
	return out, nil
}

func withReport(err error, rep string) error {
	if rep == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(rep))
}

type mergeResource struct {
	Compression string `json:"compression"`
	Source      string `json:"source"`
}

type ignition struct {
	Ignition struct {
		Version string `json:"version"`
		Config  struct {
			Merge []mergeResource `json:"merge"`
		} `json:"config"`
	} `json:"ignition"`
}

// merge builds the outer config. The order is the merge order: later parts
// override earlier ones, so the user's configuration has the last word.
func merge(parts []Part) ([]byte, error) {
	var out ignition
	out.Ignition.Version = ignitionVersion
	for _, p := range parts {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, fmt.Errorf("compressing the %s config: %w", p.Name, err)
		}
		if _, err := zw.Write(p.Ignition); err != nil {
			return nil, fmt.Errorf("compressing the %s config: %w", p.Name, err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("compressing the %s config: %w", p.Name, err)
		}
		out.Ignition.Config.Merge = append(out.Ignition.Config.Merge, mergeResource{
			Compression: "gzip",
			Source:      "data:;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding the user-data: %w", err)
	}
	return b, nil
}

func partSizes(parts []Part) string {
	s := make([]string, 0, len(parts))
	for _, p := range parts {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(p.Ignition)
		_ = zw.Close()
		s = append(s, fmt.Sprintf("%s %d bytes compressed", p.Name, buf.Len()))
	}
	return strings.Join(s, ", ")
}
