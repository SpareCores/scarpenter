//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is the development driver: instances live in memory, and for
// every created instance it creates a Node object that kwok-controller keeps
// Ready (the approach of core's KWOK provider, kwok/cloudprovider at
// 8fb3abde2be3111cd26e0c0f45ac0babdeb8e9c0). It impersonates the catalog of a
// Keeper vendor (--fake-catalog-vendor): its Vendor() is that vendor's ID, so
// NodeClasses configure it as spec.vendors.<vendor> and the instance types are
// the vendor's. Errors, quotas, launchable types and orphans are driven by a
// ConfigMap and the driver publishes its calls and instances in another one,
// so L3 scenarios can both steer and assert (overview section 13.2).
package fake

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

// Registration name of the driver.
const Name = "fake"

// Defaults of the flags.
const (
	DefaultCatalogVendor   = "aws"
	DefaultConfigNamespace = "scarpenter-system"
	DefaultConfigName      = "scarpenter-fake"
	DefaultStateName       = "scarpenter-fake-state"
	DefaultResourcesName   = "scarpenter-fake-resources"
	defaultStateInterval   = 500 * time.Millisecond
	defaultConfigTTL       = time.Second
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

// Options are the flags of the fake driver, injected next to core's options.
type Options struct {
	// CatalogVendor is the Keeper vendor whose catalog the driver impersonates.
	CatalogVendor string
	// ConfigNamespace and ConfigName locate the steering ConfigMap;
	// StateName names the ConfigMap the driver publishes its state in (same
	// namespace).
	ConfigNamespace string
	ConfigName      string
	StateName       string
	// ResourcesName names the ConfigMap that holds the fake account's managed
	// resources, so that they survive a restart of the controller like the
	// resources of a real vendor account do.
	ResourcesName string
	// LaunchDelay is the simulated duration of CreateInstance, unless the
	// ConfigMap sets launchDelay.
	LaunchDelay time.Duration

	// StateInterval debounces writes of the state ConfigMap and ConfigTTL
	// caches the steering ConfigMap; not flags (tests shorten them).
	StateInterval time.Duration
	ConfigTTL     time.Duration
}

// DefaultOptions returns the options with every default.
func DefaultOptions() Options {
	return Options{
		CatalogVendor: DefaultCatalogVendor, ConfigNamespace: DefaultConfigNamespace,
		ConfigName: DefaultConfigName, StateName: DefaultStateName, ResourcesName: DefaultResourcesName,
		StateInterval: defaultStateInterval, ConfigTTL: defaultConfigTTL,
	}
}

// AddFlags registers the flags; the defaults come from SCARPENTER_FAKE_*.
func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.CatalogVendor, "fake-catalog-vendor", env.WithDefaultString("SCARPENTER_FAKE_CATALOG_VENDOR", DefaultCatalogVendor), "Keeper vendor whose catalog the fake driver impersonates; it serves that vendor ID.")
	fs.StringVar(&o.ConfigNamespace, "fake-config-namespace", env.WithDefaultString("SCARPENTER_FAKE_CONFIG_NAMESPACE", DefaultConfigNamespace), "Namespace of the fake driver's ConfigMaps.")
	fs.StringVar(&o.ConfigName, "fake-config-name", env.WithDefaultString("SCARPENTER_FAKE_CONFIG_NAME", DefaultConfigName), "Name of the ConfigMap that steers the fake driver (errors, quotas, orphans).")
	fs.StringVar(&o.StateName, "fake-state-name", env.WithDefaultString("SCARPENTER_FAKE_STATE_NAME", DefaultStateName), "Name of the ConfigMap the fake driver publishes its calls and instances in.")
	fs.StringVar(&o.ResourcesName, "fake-resources-name", env.WithDefaultString("SCARPENTER_FAKE_RESOURCES_NAME", DefaultResourcesName), "Name of the ConfigMap that holds the fake account's managed resources.")
	fs.DurationVar(&o.LaunchDelay, "fake-launch-delay", env.WithDefaultDuration("SCARPENTER_FAKE_LAUNCH_DELAY", 0), "Simulated duration of a fake instance launch.")
}

// Parse parses the flags and validates the options.
func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags: %w", err)
	}
	o.StateInterval, o.ConfigTTL = defaultStateInterval, defaultConfigTTL
	if o.CatalogVendor == "" || o.ConfigNamespace == "" || o.ConfigName == "" || o.StateName == "" || o.ResourcesName == "" {
		return errors.New("the --fake-* names must not be empty")
	}
	if o.LaunchDelay < 0 {
		return errors.New("--fake-launch-delay must not be negative")
	}
	return nil
}

// ToContext stores the options in the context.
func (o *Options) ToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, optionsKey{}, o)
}

// FromContext returns the options stored in the context, or nil.
func FromContext(ctx context.Context) *Options {
	v, _ := ctx.Value(optionsKey{}).(*Options)
	return v
}
