/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package common

// Isolation is who may reach a node privately.
// +kubebuilder:validation:Enum=none;nodeclass;run
type Isolation string

// Isolation levels.
const (
	IsolationNone      Isolation = "none"
	IsolationNodeClass Isolation = "nodeclass"
	IsolationRun       Isolation = "run"
)

// Networking is the generic network setting of nodes: identical in meaning on
// every vendor (00a section 6.2). Everything vendor-specific lives in the
// vendor blocks and the network profiles.
type Networking struct {
	// publicIPv4 gives nodes a public IPv4 address.
	// +kubebuilder:default=true
	// +optional
	PublicIPv4 *bool `json:"publicIPv4,omitempty"`

	// publicIPv6 gives nodes a public IPv6 address.
	// +kubebuilder:default=false
	// +optional
	PublicIPv6 *bool `json:"publicIPv6,omitempty"`

	// inbound lists the allowed inbound traffic. Without rules everything
	// inbound is denied (a deny firewall is attached where the vendor default
	// is open).
	// +optional
	Inbound *Inbound `json:"inbound,omitempty"`

	// isolation decides who may reach a node privately: none (no other node:
	// the strictest, only inbound rules apply; it does not mean "not isolated"),
	// the nodes of the SCNodeClass, or only the nodes of the same run (placement
	// group run key; without a run key the nodes of the SCNodeClass are one run).
	// +kubebuilder:default=run
	// +optional
	Isolation Isolation `json:"isolation,omitempty"`
}

// NetworkingOverride overrides Networking per vendor. Unset fields keep the
// SCNodeClass-level value, so none of them has a default.
type NetworkingOverride struct {
	// publicIPv4 overrides spec.networking.publicIPv4.
	// +optional
	PublicIPv4 *bool `json:"publicIPv4,omitempty"`

	// publicIPv6 overrides spec.networking.publicIPv6.
	// +optional
	PublicIPv6 *bool `json:"publicIPv6,omitempty"`

	// inbound replaces spec.networking.inbound.
	// +optional
	Inbound *Inbound `json:"inbound,omitempty"`

	// isolation overrides spec.networking.isolation.
	// +optional
	Isolation Isolation `json:"isolation,omitempty"`
}

// Inbound holds the inbound rules.
type Inbound struct {
	// rules allow traffic from CIDRs; everything else is denied.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Rules []InboundRule `json:"rules,omitempty"`
}

// Protocol of an inbound rule.
// +kubebuilder:validation:Enum=tcp;udp;icmp;any
type Protocol string

// InboundRule allows inbound traffic from CIDRs.
// +kubebuilder:validation:XValidation:rule="!has(self.fromPort) || self.protocol == 'tcp' || self.protocol == 'udp'",message="ports are only valid for tcp and udp"
// +kubebuilder:validation:XValidation:rule="!has(self.toPort) || has(self.fromPort)",message="toPort needs fromPort"
// +kubebuilder:validation:XValidation:rule="!has(self.toPort) || self.toPort >= self.fromPort",message="toPort must not be smaller than fromPort"
// +kubebuilder:validation:XValidation:rule="self.cidrs.all(c, isCIDR(c))",message="cidrs must be valid CIDR notation"
type InboundRule struct {
	// protocol of the rule.
	// +required
	Protocol Protocol `json:"protocol"`

	// fromPort is the first port of the range (tcp and udp); without ports
	// the rule covers every port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	FromPort *int32 `json:"fromPort,omitempty"`

	// toPort is the last port of the range; defaults to fromPort.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ToPort *int32 `json:"toPort,omitempty"`

	// cidrs are the allowed source networks.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=43
	// +listType=set
	// +required
	CIDRs []string `json:"cidrs"`
}
