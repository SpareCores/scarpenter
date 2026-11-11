/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// FieldManager owns the status fields this controller writes. The quota status
// controller writes its own fields under its own manager, so the two never
// overwrite each other.
const FieldManager = "scarpenter-nodeclass"

// Condition reasons.
const (
	ReasonReady               = "Ready"
	ReasonResolved            = "Resolved"
	ReasonReleaseNotFound     = "ReleaseNotFound"
	ReasonResolutionFailed    = "ResolutionFailed"
	ReasonValid               = "Valid"
	ReasonInvalidKubelet      = "InvalidKubeletSettings"
	ReasonInvalidUse          = "InvalidResourceUse"
	ReasonVersionSkew         = "KubernetesVersionNotSupported"
	ReasonReferenceNotFound   = "ReferenceNotFound"
	ReasonReferenceUnreadable = "ReferenceUnreadable"
	ReasonTemplateInvalid     = "TemplateInvalid"
	ReasonClusterDNSUnknown   = "ClusterDNSUnknown"
	ReasonVendorNotEnabled    = "VendorNotEnabled"
	ReasonVendorInvalid       = "ValidationFailed"
	ReasonUnknownRegions      = "UnknownRegions"
	ReasonImagesNotReady      = "ImagesNotReady"
	ReasonImagesFailed        = "ImageResolutionFailed"
	ReasonImagesNotReported   = "NotReported"
	// ReasonImagesPartial: some regions the NodeClass does not name have no image;
	// their offerings are unavailable, the condition stays True.
	ReasonImagesPartial        = "ImagesPartial"
	ReasonMetricsNotRegistered = "MetricLabelsNotRegistered"
	ReasonConfigurationWarning = "SettingHasNoEffect"
)

// Event reasons (on the SCNodeClass); Warning Events are deduplicated.
const (
	EventArtifactsFailed  = "ArtifactsNotResolved"
	EventValidationFailed = "ValidationFailed"
	EventVendorNotReady   = "VendorNotReady"
	// EventIPv6OnlyNeedsMirror: IPv6-only nodes cannot reach the default sysext
	// source (GitHub releases have no IPv6).
	EventIPv6OnlyNeedsMirror  = "IPv6OnlyNeedsMirror"
	EventRestartRequired      = "RestartRequired"
	EventConfigurationWarning = "ConfigurationWarning"
	// EventHardeningNotOffered: the NodeClass enables a hardening the vendor's
	// driver does not offer; it applies to the vendors that do.
	EventHardeningNotOffered = "HardeningNotOffered"
	EventMetricInvalid       = "MetricInvalid"
	eventDedupe              = 10 * time.Minute
)

// Defaults of the intervals.
const (
	// DefaultInterval re-reconciles a ready NodeClass: the catalog, the
	// referenced ConfigMaps and Secrets and the vendors change without any
	// NodeClass event.
	DefaultInterval = time.Minute
	// notReadyInterval re-reconciles a NodeClass that is not ready, which
	// typically waits for the first catalog snapshot.
	notReadyInterval = 10 * time.Second
	// versionTTL is how long the API server's version is kept.
	versionTTL = 5 * time.Minute
	// maxKubeletMinorSkew is how many minors older than the API server the
	// kubelet may be.
	maxKubeletMinorSkew = 3
	// maxMessage bounds condition messages.
	maxMessage = 1000
)

// architectures are the architectures whose artifacts are resolved; both, so
// that a NodeClass is ready for every instance type of its vendors.
var architectures = []string{artifacts.ArchAMD64, artifacts.ArchARM64}

// Options configures the controller.
type Options struct {
	// Client reads the NodeClass (cached) and writes its status.
	Client   client.Client
	Catalog  *catalog.Catalog
	Registry *vendors.Registry
	// Artifacts resolves the sysext digests.
	Artifacts *artifacts.Resolver
	// Renderer validates a trial rendering, Refs reads the referenced
	// ConfigMaps and Secrets for the drift hash.
	Renderer *bootstrap.Renderer
	Refs     bootstrap.References
	// Regions says which regions a driver can use (the facts); nil skips the
	// check of the NodeClass's regions.
	Regions interface{ Regions(vendor string) []string }
	// ImageGate, if set, is told the regions without a boot image, whose offerings
	// are then unavailable (overview section 9).
	// SysextMirror says --sysext-base-url points at a mirror: the default source
	// (GitHub releases) has no IPv6, so IPv6-only nodes need one.
	SysextMirror bool
	ImageGate    interface {
		SetImageGaps(class, vendor string, gaps map[string]string)
	}
	// ServerVersion returns the API server's version, for example v1.37.1; nil
	// skips the skew check.
	ServerVersion func(ctx context.Context) (string, error)
	Recorder      events.Recorder
	Clock         clock.Clock
	// Interval defaults to DefaultInterval.
	Interval time.Duration
}

// Controller makes a SCNodeClass Ready: it resolves the artifacts, validates
// the NodeClass, reports the readiness of each vendor and publishes the drift
// hashes (overview sections 6.2 and 6.3).
type Controller struct {
	opts Options

	mu         sync.Mutex
	version    string
	versionAt  time.Time
	versionErr error
}

// New returns the controller; Register adds it to a manager.
func New(opts Options) *Controller {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	return &Controller{opts: opts}
}

// Register watches SCNodeClasses with the manager.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("nodeclass").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&v1alpha1.SCNodeClass{}).Complete(c)
}

// Reconcile computes the status of the NodeClass and applies it.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nc := &v1alpha1.SCNodeClass{}
	if err := c.opts.Client.Get(ctx, req.NamespacedName, nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	desired := c.compute(ctx, nc)
	if err := c.apply(ctx, nc, desired); err != nil {
		return reconcile.Result{}, err
	}
	if desired.StatusConditions().Root().IsTrue() {
		return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
	}
	return reconcile.Result{RequeueAfter: notReadyInterval}, nil
}

// compute returns the NodeClass with the status the controller derives.
func (c *Controller) compute(ctx context.Context, nc *v1alpha1.SCNodeClass) *v1alpha1.SCNodeClass {
	desired := nc.DeepCopy()
	cs := desired.StatusConditions(status.WithClock(c.opts.Clock))
	desired.Status.VendorNames = nc.Spec.Vendors.Names()

	artifactsOK := c.resolve(ctx, desired, cs)
	inputs, complete := c.validate(ctx, desired, cs, artifactsOK)

	st := c.opts.Catalog.Status(nc.Name)
	if st.Ready {
		cs.SetTrueWithReason(v1alpha1.ConditionTypeCatalogReady, ReasonReady, "")
	} else {
		cs.SetFalse(v1alpha1.ConditionTypeCatalogReady,
			orDefault(st.Reason, v1alpha1.ConditionTypeCatalogReady), trim(st.Message))
	}
	c.restartRequired(desired, cs)

	desired.Status.Vendors = map[string]v1alpha1.VendorStatus{}
	for _, vendor := range desired.Spec.Vendors.Names() {
		desired.Status.Vendors[vendor] = c.vendor(ctx, nc, desired, vendor, inputs, complete)
	}
	return desired
}

// resolve resolves the artifacts of the NodeClass into its status and reports
// whether they are usable. A failure leaves status.resolved as it was: the drift
// hash covers the digests, and a release server that fails for a while must not
// make every node of the NodeClass drift and come back.
func (c *Controller) resolve(ctx context.Context, desired *v1alpha1.SCNodeClass, cs status.ConditionSet) bool {
	resolved, err := c.resolveArtifacts(ctx, desired)
	if err == nil {
		desired.Status.Resolved = resolved
		cs.SetTrueWithReason(v1alpha1.ConditionTypeArtifactsReady, ReasonResolved, "")
		return true
	}
	reason := ReasonResolutionFailed
	if errors.Is(err, artifacts.ErrNotFound) || errors.Is(err, artifacts.ErrChecksumMissing) {
		reason = ReasonReleaseNotFound
	}
	// An artifact that is already pinned in the status stays usable while the
	// release server is unreachable: its digest does not change.
	if reason == ReasonResolutionFailed && c.stillPinned(desired) {
		log.FromContext(ctx).Error(err, "keeping the resolved artifacts, the release server failed",
			"nodeclass", desired.Name)
		cs.SetTrueWithReason(v1alpha1.ConditionTypeArtifactsReady, ReasonResolved,
			trim("keeping the resolved artifacts; refreshing them failed: "+err.Error()))
		return true
	}
	cs.SetFalse(v1alpha1.ConditionTypeArtifactsReady, reason, trim(err.Error()))
	c.warn(desired, EventArtifactsFailed, err.Error())
	return false
}

func (c *Controller) resolveArtifacts(
	ctx context.Context, nc *v1alpha1.SCNodeClass,
) (*v1alpha1.ResolvedArtifacts, error) {
	out := &v1alpha1.ResolvedArtifacts{Kubernetes: map[string]v1alpha1.Artifact{}}
	var errs []error
	for _, arch := range architectures {
		a, err := c.opts.Artifacts.Sysext(ctx, "kubernetes", nc.Spec.Kubernetes.Version, arch)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out.Kubernetes[arch] = v1alpha1.Artifact{URL: a.URL, SHA256: a.SHA256}
	}
	if ts := nc.Spec.Network.Tailscale; nc.Spec.Network.Template == v1alpha1.NetworkTemplateTailscale && ts != nil {
		out.Tailscale = map[string]v1alpha1.Artifact{}
		for _, arch := range architectures {
			a, err := c.opts.Artifacts.Sysext(ctx, "tailscale", ts.Version, arch)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out.Tailscale[arch] = v1alpha1.Artifact{URL: a.URL, SHA256: a.SHA256}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// stillPinned reports whether the status already holds the artifacts of the
// versions the spec names.
func (c *Controller) stillPinned(nc *v1alpha1.SCNodeClass) bool {
	r := nc.Status.Resolved
	if r == nil {
		return false
	}
	check := func(name, version string, have map[string]v1alpha1.Artifact) bool {
		for _, arch := range architectures {
			want, err := c.opts.Artifacts.SysextURL(name, version, arch)
			if err != nil || have[arch].URL != want || have[arch].SHA256 == "" {
				return false
			}
		}
		return true
	}
	if !check("kubernetes", nc.Spec.Kubernetes.Version, r.Kubernetes) {
		return false
	}
	if ts := nc.Spec.Network.Tailscale; nc.Spec.Network.Template == v1alpha1.NetworkTemplateTailscale && ts != nil {
		return check("tailscale", ts.Version, r.Tailscale)
	}
	return true
}

// validate runs the semantic checks and returns the loaded references for the
// drift hash. complete is false when a check that decides what the hash covers
// did not get as far as loading every input (a skew check or a reference that
// failed, for a moment or not): the hash is then kept as it was, so that a
// transient failure never makes the nodes of the NodeClass drift.
func (c *Controller) validate(
	ctx context.Context, desired *v1alpha1.SCNodeClass, cs status.ConditionSet, artifactsOK bool,
) (inputs bootstrap.Inputs, complete bool) {
	fail := func(reason string, err error) {
		cs.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, reason, trim(err.Error()))
		c.warn(desired, EventValidationFailed, err.Error())
	}
	if _, err := catalog.ClassFromNodeClass(desired); err != nil {
		fail(ReasonInvalidKubelet, err)
		return inputs, false
	}
	if err := resources.CheckUsesSelect(desired); err != nil {
		fail(ReasonInvalidUse, err)
		return inputs, false
	}
	if err := c.checkSkew(ctx, desired); err != nil {
		fail(ReasonVersionSkew, err)
		return inputs, false
	}
	inputs, err := bootstrap.LoadInputs(ctx, c.opts.Refs, desired)
	if err != nil {
		reason := ReasonReferenceUnreadable
		if errors.Is(err, bootstrap.ErrNotFound) {
			reason = ReasonReferenceNotFound
		}
		fail(reason, err)
		return inputs, false
	}
	// A trial rendering needs the artifacts; without them ArtifactsReady says
	// what is wrong.
	if artifactsOK {
		for _, arch := range architectures {
			if err := c.opts.Renderer.ValidateWith(ctx, desired, arch, inputs, c.bootFragments(desired)); err != nil {
				reason := ReasonTemplateInvalid
				if _, ok := errors.AsType[*bootstrap.ClusterDNSError](err); ok {
					reason = ReasonClusterDNSUnknown
				}
				fail(reason, fmt.Errorf("rendering for %s: %w", arch, err))
				return inputs, true
			}
		}
	}
	cs.SetTrueWithReason(v1alpha1.ConditionTypeValidationSucceeded, ReasonValid, "")
	return inputs, true
}

// checkSkew enforces the kubelet version policy: at most three minors older
// than the API server and never newer.
func (c *Controller) checkSkew(ctx context.Context, nc *v1alpha1.SCNodeClass) error {
	if c.opts.ServerVersion == nil {
		return nil
	}
	server, err := c.serverVersion(ctx)
	if err != nil {
		// Not the NodeClass's fault: keep going without the check.
		log.FromContext(ctx).Error(err, "could not read the API server's version, skipping the version skew check")
		return nil
	}
	return CheckSkew(nc.Spec.Kubernetes.Version, server)
}

// CheckSkew checks the kubelet version against the API server's: the same
// major version, at most three minors older, never newer.
func CheckSkew(kubelet, server string) error {
	kv, err := utilversion.ParseGeneric(kubelet)
	if err != nil {
		return fmt.Errorf("parsing the Kubernetes version %q: %w", kubelet, err)
	}
	sv, err := utilversion.ParseGeneric(server)
	if err != nil {
		return fmt.Errorf("parsing the API server version %q: %w", server, err)
	}
	switch {
	case kv.Major() != sv.Major() || kv.Minor() > sv.Minor():
		return fmt.Errorf("kubernetes %s is newer than the API server %s: the kubelet must not be newer", kubelet, server)
	case sv.Minor()-kv.Minor() > maxKubeletMinorSkew:
		return fmt.Errorf("kubernetes %s is more than %d minor versions older than the API server %s",
			kubelet, maxKubeletMinorSkew, server)
	}
	return nil
}

func (c *Controller) serverVersion(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.versionAt.IsZero() && c.opts.Clock.Since(c.versionAt) < versionTTL && c.versionErr == nil {
		return c.version, nil
	}
	v, err := c.opts.ServerVersion(ctx)
	if err != nil {
		return "", err
	}
	c.version, c.versionAt, c.versionErr = v, c.opts.Clock.Now(), nil
	return v, nil
}

// restartRequired sets the informational condition for metrics whose labels
// were not registered with core at start-up.
func (c *Controller) restartRequired(nc *v1alpha1.SCNodeClass, cs status.ConditionSet) {
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		return // ValidationSucceeded reports it
	}
	if missing := c.opts.Catalog.RestartRequired(&class); len(missing) > 0 {
		slices.Sort(missing)
		msg := "the labels of these metrics are not selectable until the controller restarts: " + strings.Join(missing, ", ")
		cs.SetTrueWithReason(v1alpha1.ConditionTypeRestartRequired, ReasonMetricsNotRegistered, trim(msg))
		c.warn(nc, EventRestartRequired, msg)
	} else {
		_ = cs.Clear(v1alpha1.ConditionTypeRestartRequired)
	}
	metrics, selections := c.opts.Catalog.Invalid(&class)
	for name, why := range metrics {
		c.warn(nc, EventMetricInvalid, fmt.Sprintf("metric %s cannot be evaluated: %s", name, why))
	}
	for name, why := range selections {
		c.warn(nc, EventMetricInvalid, fmt.Sprintf("selection %s cannot be evaluated: %s", name, why))
	}
}

// trialName stands for the region, zone and instance type of a trial rendering.
const trialName = "validation"

// bootFragments returns the fragments the drivers of the NodeClass's vendors
// contribute, with every hardening the NodeClass enables, for the trial rendering.
func (c *Controller) bootFragments(nc *v1alpha1.SCNodeClass) bootstrap.FragmentsFunc {
	return func(vendor, arch string) ([]bootstrap.Fragment, error) {
		drv, ok := c.opts.Registry.Driver(vendor)
		if !ok {
			return nil, nil
		}
		frags, err := vendors.BootFragmentsFor(drv, nc.Spec.HardeningNames(), vendors.BootFragmentRequest{
			Region: trialName, Zone: trialName, InstanceType: trialName, Arch: arch,
		})
		out := make([]bootstrap.Fragment, 0, len(frags))
		for _, f := range frags {
			out = append(out, bootstrap.Fragment{Name: f.Name, Butane: f.Butane})
		}
		return out, err
	}
}

// vendor computes the status of one vendor: readiness, the images and the
// drift hash. current is the NodeClass as read (for transition times).
func (c *Controller) vendor(
	ctx context.Context, current, desired *v1alpha1.SCNodeClass, vendor string, inputs bootstrap.Inputs,
	complete bool,
) v1alpha1.VendorStatus {
	existing := current.Status.Vendors[vendor].Conditions
	now := metav1.NewTime(c.opts.Clock.Now())
	vs := v1alpha1.VendorStatus{}
	cond := func(typ string, ok bool, reason, msg string) {
		vs.Conditions = append(vs.Conditions, vendorCondition(existing, typ, ok, reason, msg, current.Generation, now))
	}

	if prev := current.Status.Vendors[vendor].Hash; !complete && prev != "" {
		// An input could not be loaded: keep the hash, a gap in what the hash
		// covers must not make the nodes drift.
		vs.Hash = prev
	} else {
		hash, err := Hash(desired, vendor, Inputs{
			Resolved: desired.Status.Resolved, CustomTemplate: inputs.CustomTemplate, UserButane: inputs.UserButane,
			AuthKeyVersion: inputs.AuthKeyVersion, Hardening: c.appliedHardening(desired, vendor),
		})
		if err != nil {
			// The hash of a NodeClass that cannot be encoded is a programming
			// error; an empty hash makes nothing drift.
			log.FromContext(ctx).Error(err, "computing the drift hash", "nodeclass", desired.Name, "vendor", vendor)
		}
		vs.Hash = hash
	}

	ready, reason, msg := c.vendorReady(ctx, desired, vendor)
	cond(v1alpha1.VendorConditionReady, ready, reason, msg)
	if !ready {
		c.warn(desired, EventVendorNotReady, fmt.Sprintf("vendor %s: %s: %s", vendor, reason, msg), vendor)
	}

	if ready {
		c.warnIPv6Only(desired, vendor)
		c.warnHardening(desired, vendor)
		if msg := c.configurationWarnings(desired, vendor); msg != "" {
			vs.Conditions = append(vs.Conditions, vendorCondition(existing, v1alpha1.VendorConditionConfigurationWarning,
				true, ReasonConfigurationWarning, msg, current.Generation, now))
			c.warn(desired, EventConfigurationWarning, fmt.Sprintf("vendor %s: %s", vendor, msg), vendor)
		}
	}
	images, ok, reason, msg := c.images(ctx, desired, vendor, ready)
	kept := false
	if reason == ReasonImagesFailed {
		// A lookup that failed (throttling, a timeout) is no verdict: keep the last one,
		// the condition and the images, and leave the gated regions as they are.
		for _, prev := range existing {
			if prev.Type == v1alpha1.VendorConditionImagesReady && prev.Reason != ReasonImagesFailed {
				images, ok, reason, msg = current.Status.Vendors[vendor].Images, prev.Status == metav1.ConditionTrue,
					prev.Reason, prev.Message
				kept = true
			}
		}
	}
	if c.opts.ImageGate != nil && !kept {
		c.opts.ImageGate.SetImageGaps(desired.Name, vendor, imageGaps(images))
	}
	vs.Images = images
	cond(v1alpha1.VendorConditionImagesReady, ok, reason, msg)
	return vs
}

// vendorReady checks the enabled driver, the regions and the driver's own
// validation of the vendor block.
func (c *Controller) vendorReady(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string,
) (ready bool, reason, msg string) {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return false, ReasonVendorNotEnabled, fmt.Sprintf("no driver serves the vendor %q (enabled: %s)",
			vendor, strings.Join(c.opts.Registry.Vendors(), ", "))
	}
	if c.opts.Regions != nil {
		if want, _ := nc.Spec.Vendors.Regions(vendor); len(want) > 0 {
			if known := c.opts.Regions.Regions(vendor); len(known) > 0 {
				var unknown []string
				for _, r := range want {
					if !slices.Contains(known, r) {
						unknown = append(unknown, r)
					}
				}
				if len(unknown) > 0 {
					return false, ReasonUnknownRegions, trim(fmt.Sprintf("the driver cannot use the regions %s",
						strings.Join(unknown, ", ")))
				}
			}
		}
	}
	if v, ok := drv.(vendors.NodeClassValidator); ok {
		if err := v.ValidateNodeClass(ctx, nc); err != nil {
			return false, ReasonVendorInvalid, trim(err.Error())
		}
	}
	// Every `use` block must be within what the driver declares it supports.
	if mr, ok := drv.(vendors.ManagedResources); ok {
		if err := resources.ValidateUses(nc, vendor, mr); err != nil {
			return false, ReasonVendorInvalid, trim(err.Error())
		}
	}
	// The vendor block and every profile must merge with the driver's defaults.
	if no, ok := drv.(vendors.NodeOptions); ok {
		in, _, err := options.InputFromNodeClass(nc, vendor)
		if err == nil {
			err = options.Validate(no, in)
		}
		if err != nil {
			return false, ReasonVendorInvalid, trim(err.Error())
		}
	}
	return true, ReasonReady, ""
}

// images asks the driver for its boot images; a driver without the capability
// resolves them at launch and counts as ready. Images are informational: a
// region without one has unavailable offerings, the NodeClass stays Ready.
func (c *Controller) images(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, vendorReady bool,
) (images []v1alpha1.ImageStatus, ready bool, reason, msg string) {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok || !vendorReady {
		return nil, false, ReasonImagesNotReady, "the vendor is not ready"
	}
	reporter, ok := drv.(vendors.ImageReporter)
	if !ok {
		return nil, true, ReasonImagesNotReported, "the driver resolves images at launch"
	}
	images, err := reporter.ReportImages(ctx, nc)
	if err != nil {
		return nil, false, ReasonImagesFailed, trim(err.Error())
	}
	var notReady []string
	readyIn := map[string]bool{}
	regions := map[string]bool{}
	for _, img := range images {
		regions[img.Region] = true
		if img.Ready {
			readyIn[img.Region] = true
		} else {
			notReady = append(notReady, fmt.Sprintf("%s/%s", img.Region, img.Arch))
		}
	}
	slices.Sort(notReady)
	if len(notReady) == 0 {
		return images, true, ReasonReady, ""
	}
	// An image that is not the pinned release (it says another version, or none)
	// is a fault of what the NodeClass named, not a gap of the vendor: the images
	// are not ready, with the image's own reason, and the region's offerings are
	// unavailable. Never booted (ADR-017).
	for _, img := range images {
		if !img.Ready && vendors.IsImageVersionCode(img.Reason) {
			return images, false, img.Reason, trim(img.Message)
		}
	}
	// A region the NodeClass names must have its images; without a region list
	// (every region of the vendor) a gap only costs the offerings of that region,
	// unless no region has an image at all.
	// The reason is the driver's own when a not-ready image gives one (for
	// example terms the account has not accepted), with that image's message.
	named, _ := nc.Spec.Vendors.Regions(vendor)
	for _, img := range images {
		if !img.Ready && slices.Contains(named, img.Region) || len(readyIn) == 0 {
			gap := "no usable image for " + strings.Join(notReady, ", ")
			for _, why := range images {
				if !why.Ready && why.Reason != "" {
					return images, false, why.Reason, trim(why.Message + "; " + gap)
				}
			}
			return images, false, ReasonImagesNotReady, trim(gap)
		}
	}
	return images, true, ReasonImagesPartial, trim("no usable image for " + strings.Join(notReady, ", ") +
		"; the offerings of those regions are unavailable")
}

// imageGaps returns the regions in which no image is ready, with the reason.
func imageGaps(images []v1alpha1.ImageStatus) map[string]string {
	ready := map[string]bool{}
	why := map[string]string{}
	for _, img := range images {
		ready[img.Region] = ready[img.Region] || img.Ready
		if !img.Ready && why[img.Region] == "" {
			why[img.Region] = img.Message
		}
	}
	gaps := map[string]string{}
	for region, ok := range ready {
		if !ok {
			gaps[region] = why[region]
		}
	}
	return gaps
}

// project is the part of the status this controller owns, for the comparison
// that decides whether anything needs writing.
type project struct {
	Conditions  []status.Condition
	VendorNames []string
	Resolved    *v1alpha1.ResolvedArtifacts
	Vendors     map[string]v1alpha1.VendorStatus
}

func projection(s *v1alpha1.SCNodeClassStatus) project {
	p := project{
		Conditions:  slices.Clone(s.Conditions),
		VendorNames: slices.Clone(s.VendorNames),
		Resolved:    s.Resolved,
		Vendors:     map[string]v1alpha1.VendorStatus{},
	}
	slices.SortFunc(p.Conditions, func(a, b status.Condition) int { return strings.Compare(a.Type, b.Type) })
	for name, vs := range s.Vendors {
		mine := v1alpha1.VendorStatus{Hash: vs.Hash, Images: vs.Images}
		for _, cond := range vs.Conditions {
			if cond.Type == v1alpha1.VendorConditionReady || cond.Type == v1alpha1.VendorConditionImagesReady ||
				cond.Type == v1alpha1.VendorConditionConfigurationWarning {
				mine.Conditions = append(mine.Conditions, cond)
			}
		}
		slices.SortFunc(mine.Conditions, func(a, b status.Condition) int { return strings.Compare(a.Type, b.Type) })
		p.Vendors[name] = mine
	}
	return p
}

// apply writes the status with server-side apply under this controller's field
// manager, unless nothing changed.
func (c *Controller) apply(ctx context.Context, current, desired *v1alpha1.SCNodeClass) error {
	if equality.Semantic.DeepEqual(projection(&current.Status), projection(&desired.Status)) {
		return nil
	}
	// Only the vendors this controller wrote are removed from its ownership:
	// the apply lists exactly the desired ones.
	body := struct {
		Conditions  []status.Condition               `json:"conditions,omitempty"`
		VendorNames []string                         `json:"vendorNames,omitempty"`
		Resolved    *v1alpha1.ResolvedArtifacts      `json:"resolved,omitempty"`
		Vendors     map[string]v1alpha1.VendorStatus `json:"vendors,omitempty"`
	}{desired.Status.Conditions, desired.Status.VendorNames, desired.Status.Resolved, desired.Status.Vendors}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&body)
	if err != nil {
		return fmt.Errorf("encoding the status of %s: %w", desired.Name, err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": desired.Name},
		"status":     u,
	}}
	err = c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the status of %s: %w", desired.Name, err)
	}
	return nil
}

// vendorCondition builds a condition of a vendor; the transition time stays when
// the status did not change.
func vendorCondition(
	existing []status.Condition, typ string, ok bool, reason, message string, generation int64, now metav1.Time,
) status.Condition {
	state := metav1.ConditionFalse
	if ok {
		state = metav1.ConditionTrue
	}
	ltt := now
	for _, cond := range existing {
		if cond.Type == typ && cond.Status == state {
			ltt = cond.LastTransitionTime
		}
	}
	return status.Condition{
		Type: typ, Status: state, Reason: orDefault(reason, typ), Message: trim(message),
		ObservedGeneration: generation, LastTransitionTime: ltt,
	}
}

func (c *Controller) warn(nc *v1alpha1.SCNodeClass, reason, message string, dedupe ...string) {
	if c.opts.Recorder == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: nc,
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        trim(message),
		DedupeValues:   append([]string{nc.Name, message}, dedupe...),
		DedupeTimeout:  eventDedupe,
	})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxMessage {
		return s[:maxMessage-3] + "..."
	}
	return s
}

// warnHardening says that the NodeClass enables a hardening the vendor's driver
// does not offer (AWS offers none: its metadata hop limit already keeps Pods off
// the metadata service), so that an opt-in is never silently ignored.
func (c *Controller) warnHardening(nc *v1alpha1.SCNodeClass, vendor string) {
	enabled := nc.Spec.HardeningNames()
	if len(enabled) == 0 {
		return
	}
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return
	}
	if _, unoffered := vendors.OfferedHardenings(drv, enabled); len(unoffered) > 0 {
		c.warn(nc, EventHardeningNotOffered, fmt.Sprintf("vendor %s offers no hardening %s: it is not applied to its nodes",
			vendor, strings.Join(unoffered, ", ")), vendor)
	}
}

// configurationWarnings returns what the driver says about settings that have no
// effect for the vendor, joined; empty when there is none.
func (c *Controller) configurationWarnings(nc *v1alpha1.SCNodeClass, vendor string) string {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return ""
	}
	w, ok := drv.(vendors.NodeClassWarner)
	if !ok {
		return ""
	}
	return strings.Join(w.NodeClassWarnings(nc), "; ")
}

// appliedHardening returns the hardenings of the NodeClass that the vendor's
// driver offers: those that reach its nodes.
func (c *Controller) appliedHardening(nc *v1alpha1.SCNodeClass, vendor string) []string {
	enabled := nc.Spec.HardeningNames()
	if len(enabled) == 0 {
		return nil
	}
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return nil
	}
	offered, _ := vendors.OfferedHardenings(drv, enabled)
	return offered
}

// warnIPv6Only warns about nodes with IPv6 only (publicIPv4 false, publicIPv6 true)
// while the sysexts come from the default source, which the nodes cannot reach: the
// release host of the sysext-bakery has no IPv6. A sysext mirror with IPv6 fixes it.
func (c *Controller) warnIPv6Only(nc *v1alpha1.SCNodeClass, vendor string) {
	if c.opts.SysextMirror {
		return
	}
	in, ok, err := options.InputFromNodeClass(nc, vendor)
	if err != nil || !ok {
		return
	}
	if n := in.Networking; n.PublicIPv4 != nil && !*n.PublicIPv4 && n.PublicIPv6 != nil && *n.PublicIPv6 {
		c.warn(nc, EventIPv6OnlyNeedsMirror, fmt.Sprintf("vendor %s: the nodes have IPv6 only, and the default sysext "+
			"source (GitHub releases) has no IPv6: set --sysext-base-url to a mirror that is reachable over IPv6",
			vendor), vendor)
	}
}
