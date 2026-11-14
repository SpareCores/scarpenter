/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package options holds the flags of the SCarpenter controller. They are
// injected next to Karpenter core's own options, so one command line carries
// both: every flag has a SCARPENTER_* environment fallback (overview section
// 12), set by the Helm chart.
//
// The pattern (an Injectable appended to core's Injectables in init) follows the
// AWS provider's pkg/operator/options (Apache-2.0,
// https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/operator/options/options.go).
package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

// Defaults of the flags that have one.
const (
	DefaultVendors                    = "aws"
	DefaultKeeperURL                  = "https://keeper.sparecores.net"
	DefaultEmbeddedCatalogVendors     = "oci"
	DefaultCatalogRefreshInterval     = time.Hour
	DefaultCatalogMaxAge              = 24 * time.Hour
	DefaultBootstrapTokenTTL          = time.Hour
	DefaultClusterDomain              = "cluster.local"
	DefaultMaxLaunchAttempts          = 3
	DefaultQuotaRefreshInterval       = 2 * time.Minute
	DefaultQuotaLimitsRefreshInterval = 15 * time.Minute
	DefaultQuotaMaxStaleness          = 15 * time.Minute
	DefaultQuotaLaunchableInterval    = time.Hour
	DefaultGCInterval                 = 2 * time.Minute
	DefaultGCGracePeriod              = 5 * time.Minute
	DefaultTerminatingGrace           = 10 * time.Minute // vendors.DefaultTerminatingGrace; options imports no scarpenter package
	DefaultResourceDefaultTTL         = 24 * time.Hour
	DefaultResourcePerUseGrace        = 2 * time.Minute
	DefaultResourceCheckInterval      = 30 * time.Second
	DefaultResourceFullSweepEvery     = 10
	DefaultAWSHomeRegion              = "us-east-1"
)

// bootstrapTokenMinTTL is the shortest token lifetime (bootstrap.MinTokenTTL;
// not imported, to keep the options free of the bootstrap package's
// dependencies).
const bootstrapTokenMinTTL = 20 * time.Minute

type optionsKey struct{}

var (
	// The cluster name is a vendor tag value and part of resource names.
	clusterNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
	vendorPattern      = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
)

// Options are the flags of the controller. Flags that later plans consume
// (quota, garbage collection, bootstrap) are parsed and validated here and
// otherwise unused until then.
type Options struct {
	// ClusterName tags vendor resources and scopes IAM.
	ClusterName string
	// APIServerEndpoint is what nodes use to reach the API server.
	APIServerEndpoint string
	// ClusterCAFile is the cluster CA bundle given to nodes; empty means the
	// kube-root-ca.crt ConfigMap of the controller's namespace.
	ClusterCAFile string
	// KubeletClientCAFile is the CA bundle that signs the API server's client
	// certificates for kubelets, when it is not the cluster CA (k3s); empty
	// means the cluster CA.
	KubeletClientCAFile string
	// Vendors are the enabled drivers.
	Vendors []string
	// KeeperURL is the base URL of the Keeper API.
	KeeperURL string
	// KeeperAPIKeyFile holds the Keeper API key, which the public Keeper needs
	// (optional for a self-hosted one). It is read at every catalog refresh.
	KeeperAPIKeyFile string
	// CatalogRefreshInterval is how often the catalog is refreshed (with jitter).
	CatalogRefreshInterval time.Duration
	// CatalogMaxAge is the snapshot age after which the catalog counts as stale.
	CatalogMaxAge time.Duration
	// SysextBaseURL and FlatcarBaseURL are mirrors of the upstream artifact
	// hosts; empty means upstream.
	SysextBaseURL  string
	FlatcarBaseURL string
	// BootstrapTokenTTL is the lifetime of a node's bootstrap token.
	BootstrapTokenTTL time.Duration
	// ClusterDNS are the cluster DNS service IPs and ClusterDomain the cluster
	// domain the kubelets of the nodes are configured with; with no ClusterDNS
	// the Pods of the nodes use the node's resolver.
	ClusterDNS []string
	// ClusterDNSNone is --cluster-dns=none: nodes get no clusterDNS on purpose.
	// With neither IPs nor none the controller finds the DNS Service itself.
	ClusterDNSNone bool
	// TailnetPrefixes are the address ranges of the tailnet; empty means
	// Tailscale's defaults.
	TailnetPrefixes []string
	ClusterDomain   string
	// MaxLaunchAttempts is the number of vendor calls one Create may make.
	MaxLaunchAttempts int
	// QuotaRefreshInterval and QuotaLimitsRefreshInterval are the refresh
	// periods of quota usage and limits; QuotaMaxStaleness is the age after
	// which a region's quota counts as unknown.
	QuotaRefreshInterval       time.Duration
	QuotaLimitsRefreshInterval time.Duration
	QuotaMaxStaleness          time.Duration
	// QuotaLaunchableInterval is the period of the refresh of which instance
	// types the account can launch in which zones.
	QuotaLaunchableInterval time.Duration
	// GCInterval is the period of the garbage collector; GCGracePeriod the age
	// an orphan instance must have before it is deleted (the time between the
	// vendor call and the NodeClaim status update); GCDryRun only logs.
	GCInterval    time.Duration
	GCGracePeriod time.Duration
	GCDryRun      bool
	// TerminatingGrace is how long an instance may be seen terminating (stopped,
	// powered off, suspended: every driver reports those as terminating) before
	// the garbage collector and Delete delete it again.
	TerminatingGrace time.Duration
	// The lifetimes of the unavailable-offerings cache entries per error class
	// (overview section 5.6); zero uses the default of the class.
	UnavailableInsufficientCapacityTTL time.Duration
	UnavailableUnsupportedTTL          time.Duration
	UnavailableQuotaExceededTTL        time.Duration
	UnavailableZoneUnusableTTL         time.Duration
	// KeeperCacheDir keeps Keeper's snapshot files of the pinned data version on
	// disk (empty: no copy, every new version is fetched in full).
	KeeperCacheDir string
	// EmbeddedCatalogVendors are the vendors whose catalog is the one compiled
	// into the binary, never read from Keeper (--embedded-catalog-vendors; the
	// value "none" is the empty list). Remove a vendor to read it from Keeper.
	EmbeddedCatalogVendors []string
	// ResourceDefaultTTL is the idleTTL of managed shared resources when a
	// SCNodeClass does not set one.
	ResourceDefaultTTL time.Duration
	// ResourcePerUseGrace is how long an unused perUse resource is kept for
	// launches in flight; ResourceCheckInterval how often the vendor resources
	// of each SCNodeClass are checked (status and preCreate).
	ResourcePerUseGrace   time.Duration
	ResourceCheckInterval time.Duration
	// ResourceFullSweepEvery is how many garbage collection sweeps pass between
	// two sweeps of every region of a driver (managed resources of regions no
	// NodeClass or NodeClaim uses any more).
	ResourceFullSweepEvery int
	// AWSHomeRegion is the region of the AWS global endpoints.
	AWSHomeRegion string
	// GCPProject is the Google Cloud project of the GCP driver; empty means the
	// project of the credentials.
	GCPProject string
	// HCloudServerLimit is the server limit of the Hetzner Cloud project, which
	// the API does not report (0 unknown: no headroom is known before a launch).
	HCloudServerLimit int
	// BillingRetention runs the billing retention controller (ADR-024); the
	// NodeClass's spec.billing is the real switch, this turns the whole
	// controller off.
	BillingRetention bool

	vendorsRaw    string
	embeddedRaw   string
	clusterDNSRaw string
	tailnetRaw    string
}

// AddFlags registers the flags. Their defaults come from the SCARPENTER_*
// environment variables, then from the constants above.
func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ClusterName, "cluster-name", env.WithDefaultString("SCARPENTER_CLUSTER_NAME", ""), "[REQUIRED] The cluster name: a tag value on vendor resources and part of their names.")
	fs.StringVar(&o.APIServerEndpoint, "api-server-endpoint", env.WithDefaultString("SCARPENTER_API_SERVER_ENDPOINT", ""), "[REQUIRED] The API server URL nodes use to reach the control plane, for example https://10.0.0.1:6443.")
	fs.StringVar(&o.ClusterCAFile, "cluster-ca-file", env.WithDefaultString("SCARPENTER_CLUSTER_CA_FILE", ""), "File with the cluster CA bundle given to nodes. If empty, the kube-root-ca.crt ConfigMap of the controller's namespace is used.")
	fs.StringVar(&o.KubeletClientCAFile, "kubelet-client-ca-file", env.WithDefaultString("SCARPENTER_KUBELET_CLIENT_CA_FILE", ""), "File with the CA bundle that signs the API server's kubelet client certificates, for control planes where it is not the cluster CA (k3s: server/tls/client-ca.crt). Written to the nodes as the kubelet's client CA, so kubectl logs and exec work. Empty means the cluster CA.")
	fs.StringVar(&o.vendorsRaw, "vendors", env.WithDefaultString("SCARPENTER_VENDORS", DefaultVendors), "Comma-separated list of enabled vendor drivers.")
	fs.StringVar(&o.KeeperURL, "keeper-url", env.WithDefaultString("SCARPENTER_KEEPER_URL", DefaultKeeperURL), "Base URL of the Keeper API: the public Spare Cores one by default, or a self-hosted Keeper (open source: github.com/SpareCores/sc-keeper).")
	fs.StringVar(&o.KeeperAPIKeyFile, "keeper-api-key-file", env.WithDefaultString("SCARPENTER_KEEPER_API_KEY_FILE", ""), "File with the Keeper API key, which the public Keeper requires (request one: https://docs.sparecores.com/api/#authentication; docs section \"Keeper API key\"). Read at every catalog refresh. Optional for a self-hosted Keeper.")
	fs.StringVar(&o.embeddedRaw, "embedded-catalog-vendors", env.WithDefaultString("SCARPENTER_EMBEDDED_CATALOG_VENDORS", DefaultEmbeddedCatalogVendors), "Comma-separated vendors served from the catalog embedded in the binary (static prices and sizes) instead of Keeper, for vendors Keeper does not serve yet; \"none\" lists none. A vendor listed here is never requested from Keeper.")
	fs.DurationVar(&o.CatalogRefreshInterval, "catalog-refresh-interval", env.WithDefaultDuration("SCARPENTER_CATALOG_REFRESH_INTERVAL", DefaultCatalogRefreshInterval), "How often the catalog is refreshed from Keeper (with 10% jitter).")
	fs.DurationVar(&o.CatalogMaxAge, "catalog-max-age", env.WithDefaultDuration("SCARPENTER_CATALOG_MAX_AGE", DefaultCatalogMaxAge), "Age of the last good catalog snapshot after which SCNodeClasses report CatalogReady=False.")
	fs.StringVar(&o.SysextBaseURL, "sysext-base-url", env.WithDefaultString("SCARPENTER_SYSEXT_BASE_URL", ""), "Mirror of the Flatcar sysext-bakery releases. Empty means upstream.")
	fs.StringVar(&o.FlatcarBaseURL, "flatcar-base-url", env.WithDefaultString("SCARPENTER_FLATCAR_BASE_URL", ""), "Mirror of the Flatcar release files. Empty means upstream.")
	fs.DurationVar(&o.BootstrapTokenTTL, "bootstrap-token-ttl", env.WithDefaultDuration("SCARPENTER_BOOTSTRAP_TOKEN_TTL", DefaultBootstrapTokenTTL), "Lifetime of a node's bootstrap token; at least 20m, because core gives a NodeClaim 15 minutes to register.")
	fs.StringVar(&o.tailnetRaw, "tailnet-prefixes", env.WithDefaultString("SCARPENTER_TAILNET_PREFIXES", ""), "Comma-separated CIDRs of the tailnet (default 100.64.0.0/10 and fd7a:115c:a1e0::/48): with the tailscale template a serving certificate may name a node address only inside them (set it for a Headscale with other prefixes).")
	fs.StringVar(&o.clusterDNSRaw, "cluster-dns", env.WithDefaultString("SCARPENTER_CLUSTER_DNS", ""), "Comma-separated IPs of the cluster DNS service, for the kubelet of the nodes (clusterDNS). Empty: the controller reads the ClusterIP of the DNS Service of kube-system (label k8s-app=kube-dns, else the Service kube-dns), and a NodeClass is not valid while it finds none. \"none\" leaves clusterDNS unset: Pods then use the node's resolver.")
	fs.StringVar(&o.ClusterDomain, "cluster-domain", env.WithDefaultString("SCARPENTER_CLUSTER_DOMAIN", DefaultClusterDomain), "Cluster domain of the kubelet of the nodes (clusterDomain).")
	fs.IntVar(&o.MaxLaunchAttempts, "max-launch-attempts", env.WithDefaultInt("SCARPENTER_MAX_LAUNCH_ATTEMPTS", DefaultMaxLaunchAttempts), "Vendor calls one NodeClaim creation may make; candidates skipped for lack of quota headroom do not count.")
	fs.DurationVar(&o.QuotaRefreshInterval, "quota-refresh-interval", env.WithDefaultDuration("SCARPENTER_QUOTA_REFRESH_INTERVAL", DefaultQuotaRefreshInterval), "How often quota usage is refreshed per vendor and region in scope.")
	fs.DurationVar(&o.QuotaLimitsRefreshInterval, "quota-limits-refresh-interval", env.WithDefaultDuration("SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL", DefaultQuotaLimitsRefreshInterval), "How often quota limits are refreshed per vendor and region in scope.")
	fs.DurationVar(&o.QuotaMaxStaleness, "quota-max-staleness", env.WithDefaultDuration("SCARPENTER_QUOTA_MAX_STALENESS", DefaultQuotaMaxStaleness), "Age after which a region whose quota refreshes fail counts as unknown quota.")
	fs.DurationVar(&o.QuotaLaunchableInterval, "quota-launchable-refresh-interval", env.WithDefaultDuration("SCARPENTER_QUOTA_LAUNCHABLE_REFRESH_INTERVAL", DefaultQuotaLaunchableInterval), "How often the instance types the account can launch per zone are refreshed.")
	fs.DurationVar(&o.GCInterval, "gc-interval", env.WithDefaultDuration("SCARPENTER_GC_INTERVAL", DefaultGCInterval), "How often the garbage collector looks for orphans.")
	fs.DurationVar(&o.GCGracePeriod, "gc-grace-period", env.WithDefaultDuration("SCARPENTER_GC_GRACE_PERIOD", DefaultGCGracePeriod), "Age an instance without a NodeClaim must have before the garbage collector deletes it.")
	fs.DurationVar(&o.TerminatingGrace, "terminating-grace", env.WithDefaultDuration("SCARPENTER_TERMINATING_GRACE", DefaultTerminatingGrace), "How long an instance may be seen terminating (a stopped instance never leaves that state by itself) before it is deleted again.")
	fs.BoolVar(&o.GCDryRun, "gc-dry-run", env.WithDefaultBool("SCARPENTER_GC_DRY_RUN", false), "Log what the garbage collector would delete without deleting it.")
	fs.DurationVar(&o.UnavailableInsufficientCapacityTTL, "unavailable-insufficient-capacity-ttl", env.WithDefaultDuration("SCARPENTER_UNAVAILABLE_INSUFFICIENT_CAPACITY_TTL", 0), "How long an offering stays unavailable after an InsufficientCapacity error (0 = 3m).")
	fs.DurationVar(&o.UnavailableUnsupportedTTL, "unavailable-unsupported-ttl", env.WithDefaultDuration("SCARPENTER_UNAVAILABLE_UNSUPPORTED_TTL", 0), "How long a type stays unavailable in a zone after an Unsupported error (0 = 24h).")
	fs.DurationVar(&o.UnavailableQuotaExceededTTL, "unavailable-quota-exceeded-ttl", env.WithDefaultDuration("SCARPENTER_UNAVAILABLE_QUOTA_EXCEEDED_TTL", 0), "How long a region and capacity type stay unavailable after a QuotaExceeded error of a vendor without a quota API (0 = 30m).")
	fs.DurationVar(&o.UnavailableZoneUnusableTTL, "unavailable-zone-unusable-ttl", env.WithDefaultDuration("SCARPENTER_UNAVAILABLE_ZONE_UNUSABLE_TTL", 0), "How long a zone stays unavailable after a ZoneUnusable error (0 = 10m).")
	fs.StringVar(&o.KeeperCacheDir, "keeper-cache-dir", env.WithDefaultString("SCARPENTER_KEEPER_CACHE_DIR", ""), "Directory that keeps Keeper's snapshot files of the pinned data version, so that a restart and an unchanged file need no download (empty: no copy).")
	fs.DurationVar(&o.ResourcePerUseGrace, "resource-per-use-grace", env.WithDefaultDuration("SCARPENTER_RESOURCE_PER_USE_GRACE", DefaultResourcePerUseGrace), "How long an unused perUse managed vendor resource is kept for launches in flight.")
	fs.DurationVar(&o.ResourceCheckInterval, "resource-check-interval", env.WithDefaultDuration("SCARPENTER_RESOURCE_CHECK_INTERVAL", DefaultResourceCheckInterval), "How often the vendor resources of each SCNodeClass are checked (status, preCreate).")
	fs.IntVar(&o.ResourceFullSweepEvery, "resource-full-sweep-every", env.WithDefaultInt("SCARPENTER_RESOURCE_FULL_SWEEP_EVERY", DefaultResourceFullSweepEvery), "Every this many garbage collection sweeps (and after a NodeClass was deleted) the managed vendor resources of every region of the driver are swept, not only of the regions in use.")
	fs.DurationVar(&o.ResourceDefaultTTL, "resource-default-ttl", env.WithDefaultDuration("SCARPENTER_RESOURCE_DEFAULT_TTL", DefaultResourceDefaultTTL), "idleTTL of managed shared vendor resources when an SCNodeClass does not set one.")
	fs.BoolVar(&o.BillingRetention, "billing-retention", env.WithDefaultBool("SCARPENTER_BILLING_RETENTION", true), "Run the billing retention controller (it holds nodes until the end of their paid billing period for the SCNodeClasses that set spec.billing.retainUntilPeriodEnd). The NodeClass field is the real switch; false turns the controller off as a whole.")
	fs.StringVar(&o.AWSHomeRegion, "aws-home-region", env.WithDefaultString("SCARPENTER_AWS_HOME_REGION", DefaultAWSHomeRegion), "AWS region of the global endpoints (pricing and quota APIs).")
	fs.StringVar(&o.GCPProject, "gcp-project", env.WithDefaultString("SCARPENTER_GCP_PROJECT", ""), "Google Cloud project of the GCP driver's nodes; empty means the project of the credentials (a service account key or the metadata server; workload identity federation has none, so set it there).")
	fs.IntVar(&o.HCloudServerLimit, "hcloud-server-limit", env.WithDefaultInt("SCARPENTER_HCLOUD_SERVER_LIMIT", 0), "Server limit of the Hetzner Cloud project (shown in its Console; the API does not report it). 0 means unknown: only the API's own refusal ends a launch.")
}

// Parse parses the arguments and validates the options.
func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags: %w", err)
	}
	o.Vendors = splitList(o.vendorsRaw)
	if !strings.EqualFold(strings.TrimSpace(o.embeddedRaw), "none") {
		o.EmbeddedCatalogVendors = splitList(o.embeddedRaw)
	}
	o.ClusterDNS = splitList(o.clusterDNSRaw)
	if len(o.ClusterDNS) == 1 && strings.EqualFold(o.ClusterDNS[0], "none") {
		o.ClusterDNS, o.ClusterDNSNone = nil, true
	}
	o.TailnetPrefixes = splitList(o.tailnetRaw)
	if err := o.Validate(); err != nil {
		return fmt.Errorf("validating options: %w", err)
	}
	return nil
}

// Validate checks the options after parsing.
func (o *Options) Validate() error {
	var errs []error
	if o.ClusterName == "" {
		errs = append(errs, errors.New("--cluster-name (SCARPENTER_CLUSTER_NAME) is required"))
	} else if !clusterNamePattern.MatchString(o.ClusterName) {
		errs = append(errs, fmt.Errorf("--cluster-name %q must match %s", o.ClusterName, clusterNamePattern))
	}
	if o.APIServerEndpoint == "" {
		errs = append(errs, errors.New("--api-server-endpoint (SCARPENTER_API_SERVER_ENDPOINT) is required"))
	} else if err := validateURL("--api-server-endpoint", o.APIServerEndpoint, "https"); err != nil {
		errs = append(errs, err)
	}
	if err := validateURL("--keeper-url", o.KeeperURL, "http", "https"); err != nil {
		errs = append(errs, err)
	}
	for name, v := range map[string]string{"--sysext-base-url": o.SysextBaseURL, "--flatcar-base-url": o.FlatcarBaseURL} {
		if v == "" {
			continue
		}
		if err := validateURL(name, v, "http", "https"); err != nil {
			errs = append(errs, err)
		}
	}
	if len(o.Vendors) == 0 {
		errs = append(errs, errors.New("--vendors must list at least one vendor"))
	}
	seen := map[string]bool{}
	for _, v := range o.Vendors {
		switch {
		case !vendorPattern.MatchString(v):
			errs = append(errs, fmt.Errorf("--vendors: %q is not a vendor name (lower-case letters and digits)", v))
		case seen[v]:
			errs = append(errs, fmt.Errorf("--vendors: %q is listed twice", v))
		}
		seen[v] = true
	}
	if o.BootstrapTokenTTL > 0 && o.BootstrapTokenTTL < bootstrapTokenMinTTL {
		errs = append(errs, fmt.Errorf("--bootstrap-token-ttl must be at least %s (core gives a NodeClaim 15 minutes to register), got %s",
			bootstrapTokenMinTTL, o.BootstrapTokenTTL))
	}
	for _, ip := range o.ClusterDNS {
		if net.ParseIP(ip) == nil {
			errs = append(errs, fmt.Errorf("--cluster-dns: %q is not an IP address", ip))
		}
	}
	errs = append(errs, o.validateEmbeddedVendors()...)
	for _, p := range o.TailnetPrefixes {
		if _, err := netip.ParsePrefix(p); err != nil {
			errs = append(errs, fmt.Errorf("--tailnet-prefixes: %q is not a CIDR", p))
		}
	}
	if o.ClusterDomain == "" {
		errs = append(errs, errors.New("--cluster-domain must not be empty"))
	}
	if o.ResourceFullSweepEvery < 1 {
		errs = append(errs, fmt.Errorf("--resource-full-sweep-every must be at least 1, got %d", o.ResourceFullSweepEvery))
	}
	if o.MaxLaunchAttempts < 1 {
		errs = append(errs, fmt.Errorf("--max-launch-attempts must be at least 1, got %d", o.MaxLaunchAttempts))
	}
	for name, d := range map[string]time.Duration{
		"--catalog-refresh-interval":          o.CatalogRefreshInterval,
		"--catalog-max-age":                   o.CatalogMaxAge,
		"--bootstrap-token-ttl":               o.BootstrapTokenTTL,
		"--quota-refresh-interval":            o.QuotaRefreshInterval,
		"--quota-limits-refresh-interval":     o.QuotaLimitsRefreshInterval,
		"--quota-max-staleness":               o.QuotaMaxStaleness,
		"--quota-launchable-refresh-interval": o.QuotaLaunchableInterval,
		"--gc-interval":                       o.GCInterval,
		"--gc-grace-period":                   o.GCGracePeriod,
		"--terminating-grace":                 o.TerminatingGrace,
		"--resource-default-ttl":              o.ResourceDefaultTTL,
		"--resource-per-use-grace":            o.ResourcePerUseGrace,
		"--resource-check-interval":           o.ResourceCheckInterval,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", name, d))
		}
	}
	if o.CatalogMaxAge > 0 && o.CatalogRefreshInterval > o.CatalogMaxAge {
		errs = append(errs, fmt.Errorf("--catalog-max-age (%s) must not be shorter than --catalog-refresh-interval (%s)",
			o.CatalogMaxAge, o.CatalogRefreshInterval))
	}
	for name, d := range map[string]time.Duration{
		"--unavailable-insufficient-capacity-ttl": o.UnavailableInsufficientCapacityTTL,
		"--unavailable-unsupported-ttl":           o.UnavailableUnsupportedTTL,
		"--unavailable-quota-exceeded-ttl":        o.UnavailableQuotaExceededTTL,
		"--unavailable-zone-unusable-ttl":         o.UnavailableZoneUnusableTTL,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative, got %s", name, d))
		}
	}
	if o.AWSHomeRegion == "" {
		errs = append(errs, errors.New("--aws-home-region must not be empty"))
	}
	return errors.Join(errs...)
}

// VendorEnabled reports whether a vendor driver is enabled.
func (o *Options) VendorEnabled(vendor string) bool {
	return slices.Contains(o.Vendors, vendor)
}

// validateEmbeddedVendors checks the names of --embedded-catalog-vendors (that
// each has an embedded catalog is checked when the catalog is built).
func (o *Options) validateEmbeddedVendors() []error {
	var errs []error
	seen := map[string]bool{}
	for _, v := range o.EmbeddedCatalogVendors {
		switch {
		case !vendorPattern.MatchString(v):
			errs = append(errs, fmt.Errorf("--embedded-catalog-vendors: %q is not a vendor name (lower-case letters and digits)", v))
		case seen[v]:
			errs = append(errs, fmt.Errorf("--embedded-catalog-vendors: %q is listed twice", v))
		}
		seen[v] = true
	}
	return errs
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func validateURL(flagName, raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q must be an absolute %s URL: %w", flagName, raw, strings.Join(schemes, " or "), err)
	}
	for _, s := range schemes {
		if u.Scheme == s && u.Host != "" {
			return nil
		}
	}
	return fmt.Errorf("%s %q must be an absolute %s URL", flagName, raw, strings.Join(schemes, " or "))
}

// ToContext stores the options in the context.
func (o *Options) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, o)
}

// ToContext stores opts in the context.
func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

// FromContext returns the options stored in the context, or nil.
func FromContext(ctx context.Context) *Options {
	v, _ := ctx.Value(optionsKey{}).(*Options)
	return v
}
