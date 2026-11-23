/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

// RenderRequest is what the user-data of a node depends on (overview section
// 8.2): the architecture (sysext URLs), the offering of the launch attempt, the
// kube-reserved of the instance type and the device hints of its volumes. A
// request without the last three (no instance type, no hints) renders what is
// the same for every attempt of the architecture and region.
type RenderRequest struct {
	NodeClass *v1alpha1.SCNodeClass
	NodeClaim *karpv1.NodeClaim
	Vendor    string
	Region    string
	Zone      string
	// InstanceType is the vendor's API reference.
	InstanceType string
	Arch         string
	// KubeReserved is the effective kubeReserved of the instance type
	// (resource name to quantity), what the catalog assumed.
	KubeReserved map[string]string
	// NodeMemoryMiB is the catalog's memory of the instance type.
	NodeMemoryMiB int64
	// Spot says that the launch is a spot or preemptible one: the kubelet then
	// shuts Pods down gracefully when the node is powered off.
	Spot    bool
	Volumes []VolumeHint
	Local   *LocalHint
	// VendorFragments are the fragments of the vendor's driver for the node.
	VendorFragments []Fragment
	// NodeIPCIDR is the IPv4 range of the private network of the launch, from
	// the driver's Prepared.NodeIPCIDR ("" for none).
	NodeIPCIDR string
	// NVIDIA is the driver of a supported NVIDIA GPU type (nil: none); the node
	// boots with it (the nvidia part).
	NVIDIA *nvidia.Selection
	// Token is the bootstrap token of the NodeClaim.
	Token string
	// NodeName and ProviderID are the node name (the NodeClaim name) and the
	// provider ID scarpenter://<vendor>/<region>/<nodeclaim>.
	NodeName   string
	ProviderID string
}

// RendererOptions configure a Renderer.
type RendererOptions struct {
	Refs References
	// APIServerEndpoint is --api-server-endpoint.
	APIServerEndpoint string
	// ClusterDNS and ClusterDomain are --cluster-dns and --cluster-domain.
	ClusterDNS    []string
	ClusterDomain string
	// ClusterDNSLookup finds the cluster DNS when ClusterDNS is empty (the
	// controller reads the DNS Service). Nil means no cluster DNS: the Pods of
	// the nodes use the node's resolver (--cluster-dns=none).
	ClusterDNSLookup func(ctx context.Context) ([]string, error)
	// KubeletClientCAFile is --kubelet-client-ca-file: the CA that signs the
	// API server's kubelet client certificates, when it is not the cluster CA.
	// It is read at every rendering.
	KubeletClientCAFile string
	// SysextBaseURL is the mirror of the sysext-bakery releases the nvidia-runtime
	// sysext is downloaded from (--sysext-base-url); empty: upstream.
	SysextBaseURL string
	// NVIDIADriverBaseURL is the mirror of the NVIDIA driver sysexts
	// (--nvidia-driver-base-url); empty: nodes download them from Flatcar's server.
	NVIDIADriverBaseURL string
}

// ClusterDNSError means the cluster DNS was neither given (--cluster-dns) nor
// found: rendering without it would silently give nodes no clusterDNS.
type ClusterDNSError struct{ Err error }

func (e *ClusterDNSError) Error() string { return e.Err.Error() }
func (e *ClusterDNSError) Unwrap() error { return e.Err }

// clusterDNS is --cluster-dns, else what the lookup finds.
func (r *Renderer) clusterDNS(ctx context.Context) ([]string, error) {
	if len(r.opts.ClusterDNS) > 0 || r.opts.ClusterDNSLookup == nil {
		return r.opts.ClusterDNS, nil
	}
	ips, err := r.opts.ClusterDNSLookup(ctx)
	if err != nil {
		return nil, &ClusterDNSError{Err: err}
	}
	return ips, nil
}

// Renderer renders the user-data of NodeClaims from the SCNodeClass (with the
// artifacts the NodeClass controller resolved into its status) and the
// references it points at. It implements the UserDataRenderer of the
// CloudProvider.
type Renderer struct {
	opts RendererOptions
	// urls builds the URL of the nvidia-runtime sysext.
	urls *artifacts.Resolver
}

// NewRenderer returns a Renderer.
func NewRenderer(opts RendererOptions) *Renderer {
	return &Renderer{opts: opts, urls: artifacts.New(artifacts.Config{SysextBase: opts.SysextBaseURL})}
}

// nvidiaParams are the inputs of the nvidia part for a driver selection.
func (r *Renderer) nvidiaParams(nc *v1alpha1.SCNodeClass, sel nvidia.Selection) (*NVIDIAParams, error) {
	url, err := r.urls.SysextURL("nvidia-runtime", sel.RuntimeVersion, artifacts.ArchAMD64)
	if err != nil {
		return nil, err
	}
	return &NVIDIAParams{
		Selection: sel, FlatcarVersion: nc.Spec.Flatcar.Version, RuntimeURL: url,
		DriverBaseURL: r.opts.NVIDIADriverBaseURL,
	}, nil
}

// Render renders the user-data for the request.
func (r *Renderer) Render(ctx context.Context, req RenderRequest) ([]byte, error) {
	ca, err := r.opts.Refs.ClusterCA(ctx)
	if err != nil {
		return nil, err
	}
	if req.NodeClass == nil {
		return nil, errors.New("no SCNodeClass")
	}
	in, err := LoadInputs(ctx, r.opts.Refs, req.NodeClass)
	if err != nil {
		return nil, err
	}
	p, err := r.params(req.NodeClass, req.Arch, ca, in)
	if err != nil {
		return nil, err
	}
	if p.ClusterDNS, err = r.clusterDNS(ctx); err != nil {
		return nil, err
	}
	if p.KubeletClientCA, err = r.kubeletClientCA(); err != nil {
		return nil, err
	}
	p.NodeName, p.ProviderID, p.BootstrapToken = req.NodeName, req.ProviderID, req.Token
	p.Vendor, p.Region, p.Zone, p.InstanceType = req.Vendor, req.Region, req.Zone, req.InstanceType
	p.KubeReserved, p.NodeMemoryMiB, p.Volumes, p.Local = req.KubeReserved, req.NodeMemoryMiB, req.Volumes, req.Local
	p.VendorFragments = req.VendorFragments
	p.Spot = req.Spot
	p.NodeIPCIDR = req.NodeIPCIDR
	if req.NVIDIA != nil {
		if p.NVIDIA, err = r.nvidiaParams(req.NodeClass, *req.NVIDIA); err != nil {
			return nil, err
		}
	}
	if req.NodeClaim != nil {
		p.Labels = req.NodeClaim.Labels
	}
	p.Taints = registrationTaints(req.NodeClaim)
	out, err := Render(p)
	if err != nil {
		return nil, err
	}
	return out.UserData, nil
}

// Validate renders the NodeClass for the architecture with placeholder node
// values and the loaded references, to find a broken template or a user-data
// over the budget before the first launch, also with every storage setup
// of each vendor (the volume unit is not part of the plain rendering). It uses the real cluster CA, which
// is the largest fixed part of the user-data (a CA bundle of 1 to 2 KiB, twice
// that after compression and encoding), and falls back to a CA-sized
// placeholder when it cannot be read: its availability is not the NodeClass's.
func (r *Renderer) Validate(ctx context.Context, nc *v1alpha1.SCNodeClass, arch string, in Inputs) error {
	return r.ValidateWith(ctx, nc, arch, in, nil)
}

// placeholderTailnetKey stands in for the per-node key in the trial renderings: 128
// characters, against about 60 of a Tailscale key and 90 of a Headscale one.
var placeholderTailnetKey = strings.Repeat("k", 128)

// validationNodeIPCIDR stands in for the private network range of a launch in the
// trial renderings: the longest text an IPv4 range has.
const validationNodeIPCIDR = "255.255.255.255/32"

// FragmentsFunc returns the fragments the core builds for the node of a NodeClass
// on a vendor for the architecture (the metadata restriction, unless the NodeClass
// says Open, and the interruption agent when spot is set), for the trial
// rendering; nil or an empty result means none.
type FragmentsFunc func(vendor, arch string, spot bool) ([]Fragment, error)

// ValidateWith is Validate that also renders, per vendor, the user-data with the
// fragments of the vendor's driver, so that a fragment that does not translate or
// pushes the user-data over the budget is found before the first launch.
func (r *Renderer) ValidateWith(
	ctx context.Context, nc *v1alpha1.SCNodeClass, arch string, in Inputs, fragments FragmentsFunc,
) error {
	ca, err := r.opts.Refs.ClusterCA(ctx)
	if err != nil {
		ca = placeholderCA()
	}
	p, err := r.params(nc, arch, ca, in)
	if err != nil {
		return err
	}
	if p.ClusterDNS, err = r.clusterDNS(ctx); err != nil {
		return err
	}
	// Like the cluster CA, a CA file that cannot be read now is not the
	// NodeClass's fault: size it with a placeholder.
	if r.opts.KubeletClientCAFile != "" {
		if p.KubeletClientCA, err = r.kubeletClientCA(); err != nil {
			p.KubeletClientCA = placeholderCA()
		}
	}
	p.NodeName, p.ProviderID = "validation-node", "scarpenter://validation/validation/validation-node"
	p.Vendor, p.Region, p.BootstrapToken = "validation", "validation", "000000.0000000000000000"
	p.Taints = []corev1.Taint{karpv1.UnregisteredNoExecuteTaint}
	// Only some vendors report a private network range (and the direct template
	// renders the node IP unit for it, about 1.9 KiB of user-data), the launch knows
	// which: size every rendering with one, so that a NodeClass that only fits
	// without it is rejected here and not when a node launches on such a vendor.
	p.NodeIPCIDR = validationNodeIPCIDR
	// A node of a supported GPU type adds the nvidia part (and the driver file,
	// when a mirror is set): size every rendering with the largest variant of the
	// NodeClass's Flatcar release, which only a launch of a GPU type renders.
	gpuNote := ""
	if sel, ok := largestNVIDIA(nc.Spec.Flatcar.Version); ok && arch == artifacts.ArchAMD64 {
		if p.NVIDIA, err = r.nvidiaParams(nc, sel); err != nil {
			return err
		}
		gpuNote = " and the NVIDIA driver"
	}
	if nc.Spec.Network.Tailscale.PerNode() {
		// The key of a node is minted at its launch: size the rendering with a
		// placeholder longer than any vendor's key.
		p.Network.Tailscale.AuthKey = placeholderTailnetKey
	}
	// A spot node adds the interruption agent (and the shutdown settings), and the
	// largest GPU part and the agent do not meet in the trial: a spot launch of
	// the largest GPU type with the largest storage setup would exceed the limit, and no
	// vendor offers such a type. The trial renders the GPU part without the agent
	// and the agent without the GPU part.
	variants := []Params{p}
	if p.NVIDIA == nil {
		variants[0].Spot = true
	} else {
		spot := p
		spot.NVIDIA, spot.Spot = nil, true
		variants = append(variants, spot)
	}
	worst, err := worstCaseVolumes(nc)
	if err != nil {
		return err
	}
	for _, v := range variants {
		note := gpuNote
		if v.Spot && v.NVIDIA == nil && p.NVIDIA != nil {
			note = " (a spot node without the NVIDIA part)"
		}
		if err := r.validateVariant(nc, arch, v, note, fragments, worst); err != nil {
			return err
		}
	}
	return nil
}

// validateVariant renders one variant of the trial: the plain node, with the
// boot fragments of each vendor, and with the largest storage setup of each
// vendor (the first-boot volume unit adds to the user-data of nodes that have
// volumes, about 2.5 KiB and 0.3 KiB per volume), so that no launch the NodeClass
// allows can exceed the budget.
func (r *Renderer) validateVariant(
	nc *v1alpha1.SCNodeClass, arch string, p Params, note string, fragments FragmentsFunc, worst []worstCase,
) error {
	if _, err := Render(p); err != nil {
		return fmt.Errorf("the trial rendering (with a node IP range as on a vendor with a private network%s): %w", note, err)
	}
	byVendor := map[string][]Fragment{}
	if fragments != nil {
		for _, vendor := range nc.Spec.Vendors.Names() {
			frags, err := fragments(vendor, arch, p.Spot)
			if err != nil {
				return fmt.Errorf("the boot fragments of %s: %w", vendor, err)
			}
			if len(frags) == 0 {
				continue
			}
			byVendor[vendor] = frags
			vp := p
			vp.Vendor, vp.VendorFragments = vendor, frags
			if _, err := Render(vp); err != nil {
				return fmt.Errorf("with the boot fragments of %s%s: %w", vendor, note, err)
			}
		}
	}
	for _, w := range worst {
		wp := p
		wp.Vendor, wp.Volumes, wp.Local, wp.VendorFragments = w.vendor, w.volumes, w.local, byVendor[w.vendor]
		if _, err := Render(wp); err != nil {
			return fmt.Errorf("with the storage setup of %s (%d volumes%s)%s: %w", w.vendor, len(w.volumes),
				map[bool]string{true: " and local storage", false: ""}[w.local != nil], note, err)
		}
	}
	return nil
}

// largestNVIDIA is the driver selection of the release whose nvidia part is the
// largest: the longest sysext name, and the most GPUs a type has (16, the
// NVSwitch systems with the most), with Fabric Manager when the release pins it.
func largestNVIDIA(flatcarVersion string) (nvidia.Selection, bool) {
	var best nvidia.Selection
	for _, sel := range nvidia.Selections(flatcarVersion) {
		if len(sel.Sysext) > len(best.Sysext) {
			best = sel
		}
	}
	best.Count = 16
	best.FabricManagerSHA256 = nvidia.FabricManagerPin(flatcarVersion, best.Version)
	best.NVSwitch = best.FabricManagerSHA256 != ""
	return best, best.Sysext != ""
}

// kubeletClientCA reads --kubelet-client-ca-file ("" without the flag).
func (r *Renderer) kubeletClientCA() (string, error) {
	if r.opts.KubeletClientCAFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(r.opts.KubeletClientCAFile)
	if err != nil {
		return "", fmt.Errorf("reading --kubelet-client-ca-file: %w", err)
	}
	return validCA(string(b), "--kubelet-client-ca-file "+r.opts.KubeletClientCAFile)
}

// placeholderCA is a PEM of the size of an RSA-2048 CA (about 1.2 KiB), random
// looking so that it does not compress better than a real one.
func placeholderCA() string {
	sum := sha256.Sum256([]byte("scarpenter placeholder CA"))
	raw := make([]byte, 0, 900)
	for len(raw) < 900 {
		raw = append(raw, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	enc := base64.StdEncoding.EncodeToString(raw[:900])
	var b strings.Builder
	b.WriteString("-----BEGIN CERTIFICATE-----\n")
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n-----END CERTIFICATE-----\n")
	return b.String()
}

// params gathers what does not depend on the node: the NodeClass, its
// resolved artifacts and its references.
func (r *Renderer) params(nc *v1alpha1.SCNodeClass, arch, ca string, in Inputs) (Params, error) {
	spec := &nc.Spec
	p := Params{
		Arch: arch, APIServerEndpoint: r.opts.APIServerEndpoint, ClusterCA: ca,
		Kubelet: spec.Kubelet, ClusterDomain: r.opts.ClusterDomain,
		ExtraSysexts: spec.Sysexts,
		Network:      Network{Template: spec.Network.Template},
		UserButane:   in.UserButane,
	}
	if spec.Butane != nil {
		ref := spec.Butane.ConfigMapRef
		p.UserSource = fmt.Sprintf("ConfigMap %s/%s key %s", ref.Namespace, ref.Name, ref.Key)
	}
	if spec.PoweroffAfter != nil {
		p.PoweroffAfter = spec.PoweroffAfter.Duration
	}
	resolved := nc.Status.Resolved
	if resolved == nil {
		return Params{}, errors.New("the artifacts of the SCNodeClass are not resolved yet")
	}
	k, ok := resolved.Kubernetes[arch]
	if !ok {
		return Params{}, fmt.Errorf("the kubernetes sysext is not resolved for %s", arch)
	}
	p.Kubernetes = artifacts.Artifact{URL: k.URL, SHA256: k.SHA256}

	switch spec.Network.Template {
	case v1alpha1.NetworkTemplateTailscale:
		ts := spec.Network.Tailscale
		if ts == nil {
			return Params{}, errors.New("network.tailscale is not set")
		}
		a, ok := resolved.Tailscale[arch]
		if !ok {
			return Params{}, fmt.Errorf("the tailscale sysext is not resolved for %s", arch)
		}
		p.Network.Tailscale = TailscaleParams{
			Version: ts.Version, LoginServer: ts.LoginServer, AuthKey: in.AuthKey,
			Tags: slices.Clone(ts.Tags), Sysext: artifacts.Artifact{URL: a.URL, SHA256: a.SHA256},
			AcceptRoutes: ts.AcceptRoutes,
		}
	case v1alpha1.NetworkTemplateCustom:
		ref := spec.Network.Custom.ConfigMapRef
		p.Network.Custom = in.CustomTemplate
		p.Network.CustomSource = fmt.Sprintf("ConfigMap %s/%s key %s", ref.Namespace, ref.Name, ref.Key)
	}
	return p, nil
}

// registrationTaints are the taints the kubelet registers the node with:
// Karpenter's unregistered taint (core removes it once it matched the Node to
// the NodeClaim), then the NodeClaim's taints and startup taints.
func registrationTaints(nodeClaim *karpv1.NodeClaim) []corev1.Taint {
	out := []corev1.Taint{karpv1.UnregisteredNoExecuteTaint}
	if nodeClaim == nil {
		return out
	}
	out = append(out, nodeClaim.Spec.Taints...)
	return append(out, nodeClaim.Spec.StartupTaints...)
}
