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
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
)

// RenderRequest is what the user-data of a node depends on (overview section
// 8.2). It depends on the architecture (sysext URLs) and the region, not on the
// instance type, so one rendering serves every launch attempt with the same
// architecture and region.
type RenderRequest struct {
	NodeClass *v1alpha1.SCNodeClass
	NodeClaim *karpv1.NodeClaim
	Vendor    string
	Region    string
	Arch      string
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
}

// Renderer renders the user-data of NodeClaims from the SCNodeClass (with the
// artifacts the NodeClass controller resolved into its status) and the
// references it points at. It implements the UserDataRenderer of the
// CloudProvider.
type Renderer struct {
	opts RendererOptions
}

// NewRenderer returns a Renderer.
func NewRenderer(opts RendererOptions) *Renderer { return &Renderer{opts: opts} }

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
	p.NodeName, p.ProviderID, p.BootstrapToken = req.NodeName, req.ProviderID, req.Token
	p.Vendor, p.Region = req.Vendor, req.Region
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
// over the budget before the first launch. It uses the real cluster CA, which
// is the largest fixed part of the user-data (a CA bundle of 1 to 2 KiB, twice
// that after compression and encoding), and falls back to a CA-sized
// placeholder when it cannot be read: its availability is not the NodeClass's.
func (r *Renderer) Validate(ctx context.Context, nc *v1alpha1.SCNodeClass, arch string, in Inputs) error {
	ca, err := r.opts.Refs.ClusterCA(ctx)
	if err != nil {
		ca = placeholderCA()
	}
	p, err := r.params(nc, arch, ca, in)
	if err != nil {
		return err
	}
	p.NodeName, p.ProviderID = "validation-node", "scarpenter://validation/validation/validation-node"
	p.Vendor, p.Region, p.BootstrapToken = "validation", "validation", "000000.0000000000000000"
	p.Taints = []corev1.Taint{karpv1.UnregisteredNoExecuteTaint}
	_, err = Render(p)
	return err
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
		Kubelet: spec.Kubelet, ClusterDNS: r.opts.ClusterDNS, ClusterDomain: r.opts.ClusterDomain,
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
