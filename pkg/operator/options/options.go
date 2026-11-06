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
	DefaultCatalogRefreshInterval     = time.Hour
	DefaultCatalogMaxAge              = 24 * time.Hour
	DefaultBootstrapTokenTTL          = time.Hour
	DefaultMaxLaunchAttempts          = 3
	DefaultQuotaRefreshInterval       = 2 * time.Minute
	DefaultQuotaLimitsRefreshInterval = 15 * time.Minute
	DefaultQuotaMaxStaleness          = 15 * time.Minute
	DefaultGCInterval                 = 2 * time.Minute
	DefaultResourceDefaultTTL         = 24 * time.Hour
	DefaultAWSHomeRegion              = "us-east-1"
)

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
	// Vendors are the enabled drivers.
	Vendors []string
	// KeeperURL is the base URL of the Keeper API.
	KeeperURL string
	// KeeperAPIKeyFile holds an optional Keeper API key.
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
	// MaxLaunchAttempts is the number of vendor calls one Create may make.
	MaxLaunchAttempts int
	// QuotaRefreshInterval and QuotaLimitsRefreshInterval are the refresh
	// periods of quota usage and limits; QuotaMaxStaleness is the age after
	// which a region's quota counts as unknown.
	QuotaRefreshInterval       time.Duration
	QuotaLimitsRefreshInterval time.Duration
	QuotaMaxStaleness          time.Duration
	// GCInterval is the period of the garbage collector.
	GCInterval time.Duration
	// ResourceDefaultTTL is the idleTTL of managed shared resources when a
	// SCNodeClass does not set one.
	ResourceDefaultTTL time.Duration
	// AWSHomeRegion is the region of the AWS global endpoints.
	AWSHomeRegion string

	vendorsRaw string
}

// AddFlags registers the flags. Their defaults come from the SCARPENTER_*
// environment variables, then from the constants above.
func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ClusterName, "cluster-name", env.WithDefaultString("SCARPENTER_CLUSTER_NAME", ""), "[REQUIRED] The cluster name: a tag value on vendor resources and part of their names.")
	fs.StringVar(&o.APIServerEndpoint, "api-server-endpoint", env.WithDefaultString("SCARPENTER_API_SERVER_ENDPOINT", ""), "[REQUIRED] The API server URL nodes use to reach the control plane, for example https://10.0.0.1:6443.")
	fs.StringVar(&o.ClusterCAFile, "cluster-ca-file", env.WithDefaultString("SCARPENTER_CLUSTER_CA_FILE", ""), "File with the cluster CA bundle given to nodes. If empty, the kube-root-ca.crt ConfigMap of the controller's namespace is used.")
	fs.StringVar(&o.vendorsRaw, "vendors", env.WithDefaultString("SCARPENTER_VENDORS", DefaultVendors), "Comma-separated list of enabled vendor drivers.")
	fs.StringVar(&o.KeeperURL, "keeper-url", env.WithDefaultString("SCARPENTER_KEEPER_URL", DefaultKeeperURL), "Base URL of the Spare Cores Keeper API.")
	fs.StringVar(&o.KeeperAPIKeyFile, "keeper-api-key-file", env.WithDefaultString("SCARPENTER_KEEPER_API_KEY_FILE", ""), "File with an optional Keeper API key (higher rate limits).")
	fs.DurationVar(&o.CatalogRefreshInterval, "catalog-refresh-interval", env.WithDefaultDuration("SCARPENTER_CATALOG_REFRESH_INTERVAL", DefaultCatalogRefreshInterval), "How often the catalog is refreshed from Keeper (with 10% jitter).")
	fs.DurationVar(&o.CatalogMaxAge, "catalog-max-age", env.WithDefaultDuration("SCARPENTER_CATALOG_MAX_AGE", DefaultCatalogMaxAge), "Age of the last good catalog snapshot after which SCNodeClasses report CatalogReady=False.")
	fs.StringVar(&o.SysextBaseURL, "sysext-base-url", env.WithDefaultString("SCARPENTER_SYSEXT_BASE_URL", ""), "Mirror of the Flatcar sysext-bakery releases. Empty means upstream.")
	fs.StringVar(&o.FlatcarBaseURL, "flatcar-base-url", env.WithDefaultString("SCARPENTER_FLATCAR_BASE_URL", ""), "Mirror of the Flatcar release files. Empty means upstream.")
	fs.DurationVar(&o.BootstrapTokenTTL, "bootstrap-token-ttl", env.WithDefaultDuration("SCARPENTER_BOOTSTRAP_TOKEN_TTL", DefaultBootstrapTokenTTL), "Lifetime of a node's bootstrap token.")
	fs.IntVar(&o.MaxLaunchAttempts, "max-launch-attempts", env.WithDefaultInt("SCARPENTER_MAX_LAUNCH_ATTEMPTS", DefaultMaxLaunchAttempts), "Vendor calls one NodeClaim creation may make; candidates skipped for lack of quota headroom do not count.")
	fs.DurationVar(&o.QuotaRefreshInterval, "quota-refresh-interval", env.WithDefaultDuration("SCARPENTER_QUOTA_REFRESH_INTERVAL", DefaultQuotaRefreshInterval), "How often quota usage is refreshed per vendor and region in scope.")
	fs.DurationVar(&o.QuotaLimitsRefreshInterval, "quota-limits-refresh-interval", env.WithDefaultDuration("SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL", DefaultQuotaLimitsRefreshInterval), "How often quota limits are refreshed per vendor and region in scope.")
	fs.DurationVar(&o.QuotaMaxStaleness, "quota-max-staleness", env.WithDefaultDuration("SCARPENTER_QUOTA_MAX_STALENESS", DefaultQuotaMaxStaleness), "Age after which a region whose quota refreshes fail counts as unknown quota.")
	fs.DurationVar(&o.GCInterval, "gc-interval", env.WithDefaultDuration("SCARPENTER_GC_INTERVAL", DefaultGCInterval), "How often the garbage collector looks for orphans.")
	fs.DurationVar(&o.ResourceDefaultTTL, "resource-default-ttl", env.WithDefaultDuration("SCARPENTER_RESOURCE_DEFAULT_TTL", DefaultResourceDefaultTTL), "idleTTL of managed shared vendor resources when an SCNodeClass does not set one.")
	fs.StringVar(&o.AWSHomeRegion, "aws-home-region", env.WithDefaultString("SCARPENTER_AWS_HOME_REGION", DefaultAWSHomeRegion), "AWS region of the global endpoints (pricing and quota APIs).")
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
	if o.MaxLaunchAttempts < 1 {
		errs = append(errs, fmt.Errorf("--max-launch-attempts must be at least 1, got %d", o.MaxLaunchAttempts))
	}
	for name, d := range map[string]time.Duration{
		"--catalog-refresh-interval":      o.CatalogRefreshInterval,
		"--catalog-max-age":               o.CatalogMaxAge,
		"--bootstrap-token-ttl":           o.BootstrapTokenTTL,
		"--quota-refresh-interval":        o.QuotaRefreshInterval,
		"--quota-limits-refresh-interval": o.QuotaLimitsRefreshInterval,
		"--quota-max-staleness":           o.QuotaMaxStaleness,
		"--gc-interval":                   o.GCInterval,
		"--resource-default-ttl":          o.ResourceDefaultTTL,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", name, d))
		}
	}
	if o.CatalogMaxAge > 0 && o.CatalogRefreshInterval > o.CatalogMaxAge {
		errs = append(errs, fmt.Errorf("--catalog-max-age (%s) must not be shorter than --catalog-refresh-interval (%s)",
			o.CatalogMaxAge, o.CatalogRefreshInterval))
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
