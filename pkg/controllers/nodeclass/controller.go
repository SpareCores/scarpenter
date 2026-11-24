/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/imagecopy"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// FieldManager owns the status fields this controller writes. The quota status
// controller writes its own fields under its own manager, so the two never
// overwrite each other.
const FieldManager = "scarpenter-nodeclass"

// Condition reasons.
const (
	ReasonReady               = "Ready"
	ReasonKeeperAuthorized    = "Authorized"
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
	// ReasonTailnetKeysNotConfigured: keySource PerNode without a usable backend.
	ReasonTailnetKeysNotConfigured = "TailnetKeysNotConfigured"
	ReasonClusterDNSUnknown        = "ClusterDNSUnknown"
	ReasonVendorNotEnabled         = "VendorNotEnabled"
	ReasonVendorInvalid            = "ValidationFailed"
	ReasonUnknownRegions           = "UnknownRegions"
	ReasonNoUsableRegions          = "NoUsableRegions"
	ReasonRegionsUnavailable       = "RegionsUnavailable"
	ReasonImagesNotReady           = "ImagesNotReady"
	ReasonImagesFailed             = "ImageResolutionFailed"
	ReasonImagesNotReported        = "NotReported"
	// ReasonImagesPartial: some regions the NodeClass does not name have no image;
	// their offerings are unavailable, the condition stays True.
	ReasonImagesPartial = "ImagesPartial"
	// ReasonImagesCopying: a copy of the image is queued or in flight (plan 29);
	// the condition stays True, a pending copy is not a fault.
	ReasonImagesCopying        = "ImagesCopying"
	ReasonMetricsNotRegistered = "MetricLabelsNotRegistered"
	// ReasonSettingHasNoEffect is the reason of ConfigurationWarning=True.
	ReasonSettingHasNoEffect = "SettingHasNoEffect"
)

// Event reasons (on the SCNodeClass); Warning Events are deduplicated.
const (
	EventArtifactsFailed  = "ArtifactsNotResolved"
	EventValidationFailed = "ValidationFailed"
	EventVendorNotReady   = "VendorNotReady"
	// EventIPv6OnlyEgress: IPv6-only nodes in a network the user brings, with the
	// default sysext source (no IPv6): the egress is the user's job.
	EventIPv6OnlyEgress       = "IPv6OnlyEgress"
	EventRestartRequired      = "RestartRequired"
	EventConfigurationWarning = "ConfigurationWarning"
	EventRegionsUnavailable   = "RegionsUnavailable"
	// EventDeprecatedHardening: the NodeClass still sets spec.bootstrap.hardening.
	EventDeprecatedHardening = "DeprecatedHardening"
	EventMetricInvalid       = "MetricInvalid"
	// EventUserDataTooLarge: a combination of a spot launch with the largest GPU part, the largest
	// storage setup and the agent exceeds the vendors' user-data limit; the NodeClass stays valid.
	EventUserDataTooLarge = "UserDataTooLarge"
	// EventImageBuilding (Normal) and EventImageBuildFailed (Warning): a driver
	// builds a missing boot image (ADR-030).
	EventImageBuilding    = vendors.CodeImageBuilding
	EventImageBuildFailed = vendors.CodeImageBuildFailed
	// EventImageCopying (Normal) and EventImageCopyFailed (Warning): the copy
	// manager copies a missing boot image into a region (ADR-032).
	EventImageCopying    = vendors.CodeImageCopying
	EventImageCopyFailed = vendors.CodeImageCopyFailed
	// imageBuildDedupe is how long the event of one build or copy is not
	// repeated: the longest build.
	imageBuildDedupe = 30 * time.Minute
	eventDedupe      = 10 * time.Minute
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
	// UnavailableRegions are those of them without zones: the credentials cannot
	// use them (unusable) or reading them failed and is retried (retrying).
	Regions interface {
		Regions(vendor string) []string
		UnavailableRegions(vendor string) (unusable, retrying []string)
	}
	// ImageGate, if set, is told the regions without a boot image, whose offerings
	// are then unavailable (overview section 9).
	// SysextMirror says --sysext-base-url points at a mirror: the default source
	// (GitHub releases) has no IPv6, so a NodeClass of IPv6-only nodes needs one.
	SysextMirror bool
	// Tailnet is the backend of per-node tailnet keys; the zero value is none.
	Tailnet   TailnetOptions
	ImageGate interface {
		SetImageGaps(class, vendor string, gaps map[string]catalog.Code)
	}
	// ImageCopies are the copy managers of the vendors with copies on (plan 29).
	// On each signal of a manager the controller hands it the pinned releases
	// (Bump, so that a new release is copied at once where nodes run from a
	// copy) and reconciles the NodeClasses of its vendor.
	ImageCopies []*imagecopy.Manager
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

	mu sync.Mutex
	// bumped are the releases (vendor/channel/version) the copy managers were
	// handed since this process started.
	bumped     map[string]bool
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
	return &Controller{opts: opts, bumped: map[string]bool{}}
}

// Register watches SCNodeClasses with the manager.
func (c *Controller) Register(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).Named("nodeclass").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&v1alpha1.SCNodeClass{})
	for _, m := range c.opts.ImageCopies {
		signals := make(chan event.GenericEvent)
		b = b.WatchesRawSource(source.Channel(signals, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, _ client.Object) []reconcile.Request { return c.classesOf(ctx, m.Vendor()) })))
		err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error { return c.forwardSignals(ctx, m, signals) }))
		if err != nil {
			return fmt.Errorf("adding the image copy signal of %s to the manager: %w", m.Vendor(), err)
		}
	}
	return b.Complete(c)
}

// forwardSignals turns each signal of the copy manager into a Bump with the
// pinned releases and a reconcile of the vendor's NodeClasses (through the
// channel source), so that the status and the gate follow a copy within one
// reconcile. The loop runs on the leader, like the manager's own.
func (c *Controller) forwardSignals(ctx context.Context, m *imagecopy.Manager, out chan<- event.GenericEvent) error {
	logger := log.FromContext(ctx).WithName("imagecopy-signal").WithValues("vendor", m.Vendor())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.Signal():
		}
		// The reconcile first: the status must not wait for the vendor calls of the bump.
		select {
		case <-ctx.Done():
			return nil
		case out <- event.GenericEvent{Object: &v1alpha1.SCNodeClass{}}:
		}
		if err := c.bump(ctx, m); err != nil {
			logger.Error(err, "bumping the pinned releases into the copy regions")
		}
	}
}

// bumpNewPin hands the copy managers of the NodeClass's vendors a release this
// process has not seen pinned before, so that the copy of a new release is
// queued at once where nodes run from a copy of the old one (plan 29, section
// 6), instead of when the next signal or garbage collection comes.
func (c *Controller) bumpNewPin(ctx context.Context, nc *v1alpha1.SCNodeClass) {
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = selector.DefaultChannel
	}
	for _, m := range c.opts.ImageCopies {
		if !slices.Contains(nc.Spec.Vendors.Names(), m.Vendor()) {
			continue
		}
		k := strings.Join([]string{m.Vendor(), channel, nc.Spec.Flatcar.Version}, "/")
		c.mu.Lock()
		seen := c.bumped[k]
		c.mu.Unlock()
		if seen {
			continue
		}
		// Seen only once the bump went through: a failure is tried again at the next reconcile.
		if err := c.bump(ctx, m); err != nil {
			log.FromContext(ctx).Error(err, "bumping the pinned releases into the copy regions", "vendor", m.Vendor())
			continue
		}
		c.mu.Lock()
		c.bumped[k] = true
		c.mu.Unlock()
	}
}

func (c *Controller) bump(ctx context.Context, m *imagecopy.Manager) error {
	pins, err := imagecopy.PinnedReleases(ctx, c.opts.Client, m.Vendor())
	if err != nil {
		return err
	}
	return m.Bump(ctx, pins)
}

// classesOf returns the requests of the NodeClasses that have a block for the vendor.
func (c *Controller) classesOf(ctx context.Context, vendor string) []reconcile.Request {
	var list v1alpha1.SCNodeClassList
	if err := c.opts.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing the NodeClasses for an image copy signal")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if slices.Contains(list.Items[i].Spec.Vendors.Names(), vendor) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
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
	c.bumpNewPin(ctx, nc)
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
		cs.SetTrueWithReason(v1alpha1.ConditionTypeCatalogReady, ReasonReady, trim(st.Message))
	} else {
		cs.SetFalse(v1alpha1.ConditionTypeCatalogReady,
			orDefault(st.Reason, v1alpha1.ConditionTypeCatalogReady), trim(st.Message))
	}
	switch a := st.KeeperAuth; {
	case a.Known && a.Authorized:
		cs.SetTrueWithReason(v1alpha1.ConditionTypeKeeperAuthorized, ReasonKeeperAuthorized, "")
	case a.Known:
		cs.SetFalse(v1alpha1.ConditionTypeKeeperAuthorized, a.Reason, trim(a.Message))
	default:
		_ = cs.Clear(v1alpha1.ConditionTypeKeeperAuthorized)
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
	if err := c.checkTailnetKeys(desired); err != nil {
		fail(ReasonTailnetKeysNotConfigured, err)
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
			warning, err := c.opts.Renderer.ValidateWith(ctx, desired, arch, inputs, c.bootFragments(desired))
			if warning != "" {
				c.warn(desired, EventUserDataTooLarge,
					fmt.Sprintf("rendering for %s: %s; launches of such types are refused one by one", arch, warning))
			}
			if err != nil {
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

// bootFragments returns the fragments the core builds for the NodeClass's vendors
// (the driver's own, the metadata restriction unless the NodeClass says Open, and
// the interruption agent of spot nodes), for the trial rendering.
func (c *Controller) bootFragments(nc *v1alpha1.SCNodeClass) bootstrap.FragmentsFunc {
	return func(vendor, arch string, spot bool) ([]bootstrap.Fragment, error) {
		drv, ok := c.opts.Registry.Driver(vendor)
		if !ok {
			return nil, nil
		}
		fs, err := vendors.BootFragmentsOf(drv, nc.Spec.RestrictsInstanceMetadata(), spot,
			nc.Spec.Network.Template == v1alpha1.NetworkTemplateTailscale)
		if err != nil {
			return nil, err
		}
		out := make([]bootstrap.Fragment, 0, len(fs))
		for _, f := range fs {
			out = append(out, bootstrap.Fragment{Name: f.Name, Butane: f.Butane})
		}
		return out, nil
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
			AuthKeyVersion: inputs.AuthKeyVersion,
		})
		if err != nil {
			// The hash of a NodeClass that cannot be encoded is a programming
			// error; an empty hash makes nothing drift.
			log.FromContext(ctx).Error(err, "computing the drift hash", "nodeclass", desired.Name, "vendor", vendor)
		}
		vs.Hash = hash
	}

	rv := c.checkRegions(desired, vendor)
	ready, reason, msg := c.vendorReady(ctx, desired, vendor, rv, existing)
	cond(v1alpha1.VendorConditionReady, ready, reason, msg)
	if !ready {
		c.warn(desired, EventVendorNotReady, fmt.Sprintf("vendor %s: %s: %s", vendor, reason, msg), vendor)
	}

	c.warnDeprecatedHardening(desired, vendor)
	if ready {
		if cond, ok := c.regionsUnavailable(ctx, desired, vendor, rv, existing, now); ok {
			vs.Conditions = append(vs.Conditions, cond)
		}
		if msg := c.configurationWarnings(desired, vendor); msg != "" {
			w := vendorCondition(existing, v1alpha1.VendorConditionConfigurationWarning,
				true, ReasonSettingHasNoEffect, msg, current.Generation, now)
			vs.Conditions = append(vs.Conditions, w)
			// One event when the warning appears or changes, not one per reconcile:
			// the condition carries it from then on.
			if conditionChanged(existing, w) {
				c.warn(desired, EventConfigurationWarning, fmt.Sprintf("vendor %s: %s", vendor, msg), vendor)
			}
		}
	}
	images, ok, reason, msg := c.images(ctx, desired, vendor, ready, rv.withoutZones())
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
	c.imageBuildEvents(desired, vendor, images)
	cond(v1alpha1.VendorConditionImagesReady, ok, reason, msg)
	return vs
}

// vendorReady checks the enabled driver, the regions and the driver's own
// validation of the vendor block.
func (c *Controller) vendorReady(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, rv regionVerdict, existing []status.Condition,
) (ready bool, reason, msg string) {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return false, ReasonVendorNotEnabled, fmt.Sprintf("no driver serves the vendor %q (enabled: %s)",
			vendor, strings.Join(c.opts.Registry.Vendors(), ", "))
	}
	switch {
	case rv.pending:
		// No verdict yet: a region-based refusal stays as it was.
		for _, prev := range existing {
			if prev.Type == v1alpha1.VendorConditionReady && prev.Status == metav1.ConditionFalse &&
				(prev.Reason == ReasonUnknownRegions || prev.Reason == ReasonNoUsableRegions) {
				return false, prev.Reason, prev.Message
			}
		}
	case len(rv.unknown) > 0:
		return false, ReasonUnknownRegions, trim(fmt.Sprintf(
			"the vendor has no regions %s (neither the driver nor Keeper knows them)", strings.Join(rv.unknown, ", ")))
	case len(rv.want) > 0 && len(rv.unavailable) == len(rv.want):
		return false, ReasonNoUsableRegions, trim(fmt.Sprintf("none of the regions can be used: %s",
			strings.Join(rv.unavailable, ", ")))
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
	if err := c.checkBootstrapReachable(nc, vendor); err != nil {
		return false, ReasonVendorInvalid, trim(err.Error())
	}
	return true, ReasonReady, ""
}

// regionVerdict sorts the regions the NodeClass names for a vendor.
type regionVerdict struct {
	want []string
	// unknown ones exist for neither the driver nor Keeper (a typo): invalid.
	unknown []string
	// unavailable ones exist, but the credentials cannot use them (an opt-in
	// region not enabled, a location not in the subscription).
	unavailable []string
	// retrying ones could not have their zones read yet (retried with backoff).
	retrying []string
	// pending: the driver's regions or Keeper's are not loaded (a restart, a new
	// leader), so there is no verdict and the previous one stays.
	pending bool
}

// withoutZones are the regions that have no offerings for lack of a read.
func (v regionVerdict) withoutZones() []string { return slices.Concat(v.unavailable, v.retrying) }

// checkRegions classifies the regions the NodeClass names for the vendor. A name
// Keeper knows is compared by its vendor name, so an alias or ID of a region the
// driver lists is usable.
func (c *Controller) checkRegions(nc *v1alpha1.SCNodeClass, vendor string) regionVerdict {
	if c.opts.Regions == nil {
		return regionVerdict{}
	}
	v := regionVerdict{}
	v.want, _ = nc.Spec.Vendors.Regions(vendor)
	known := c.opts.Regions.Regions(vendor)
	if len(known) == 0 {
		v.pending = len(v.want) > 0
		return v
	}
	unusable, retrying := c.opts.Regions.UnavailableRegions(vendor)
	for _, r := range v.want {
		name := r
		if !slices.Contains(known, r) {
			apiName, found, loaded := c.opts.Catalog.RegionName(vendor, r)
			switch {
			case !loaded:
				v.pending = true
				continue
			case !found:
				v.unknown = append(v.unknown, r)
				continue
			}
			name = apiName
		}
		switch {
		case slices.Contains(unusable, name) || !slices.Contains(known, name):
			v.unavailable = append(v.unavailable, r)
		case slices.Contains(retrying, name):
			v.retrying = append(v.retrying, r)
		}
	}
	return v
}

// regionsUnavailable returns the vendor condition RegionsUnavailable
// (abnormal-true, informational) while some regions of a ready vendor have no
// offerings, and says so in the log and in one Event per change of the set.
// Without a verdict (the facts are not loaded) the previous condition stays.
func (c *Controller) regionsUnavailable(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, v regionVerdict, existing []status.Condition,
	now metav1.Time,
) (status.Condition, bool) {
	if v.pending {
		for _, prev := range existing {
			if prev.Type == v1alpha1.VendorConditionRegionsUnavailable {
				prev.ObservedGeneration = nc.Generation
				return prev, true
			}
		}
	}
	if len(v.withoutZones()) == 0 {
		return status.Condition{}, false
	}
	var parts []string
	if len(v.unavailable) > 0 {
		parts = append(parts, "the credentials cannot use: "+strings.Join(v.unavailable, ", "))
	}
	if len(v.retrying) > 0 {
		parts = append(parts, "zones could not be read yet, retrying: "+strings.Join(v.retrying, ", "))
	}
	msg := "no offerings in some regions, " + strings.Join(parts, "; ")
	cond := vendorCondition(existing, v1alpha1.VendorConditionRegionsUnavailable,
		true, ReasonRegionsUnavailable, msg, nc.Generation, now)
	if conditionChanged(existing, cond) {
		log.FromContext(ctx).Info("regions of the NodeClass have no offerings, the vendor keeps its other regions",
			"nodeclass", nc.Name, "vendor", vendor, "unavailable", v.unavailable, "retrying", v.retrying)
		c.warn(nc, EventRegionsUnavailable, fmt.Sprintf("vendor %s: %s", vendor, cond.Message), vendor)
	}
	return cond, true
}

// conditionChanged says the condition is new or has another status or message
// than the one in existing: the moment for an Event, not every reconcile.
func conditionChanged(existing []status.Condition, cond status.Condition) bool {
	return !slices.ContainsFunc(existing, func(prev status.Condition) bool {
		return prev.Type == cond.Type && prev.Status == cond.Status && prev.Message == cond.Message
	})
}

// checkBootstrapReachable refuses a NodeClass whose nodes have no IPv4 (publicIPv4
// false, publicIPv6 true) while the sysexts come from the default source: github.com
// and objects.githubusercontent.com have no AAAA record, so the nodes could not
// fetch them. A sysext mirror reachable over IPv6 lifts the refusal. A NodeClass that
// brings its own subnets or networks keeps the IPv4 egress of that network (a NAT
// gateway is the user's job): it gets a Warning Event instead.
func (c *Controller) checkBootstrapReachable(nc *v1alpha1.SCNodeClass, vendor string) error {
	if c.opts.SysextMirror {
		return nil
	}
	in, ok, err := options.InputFromNodeClass(nc, vendor)
	if err != nil || !ok {
		return nil
	}
	n := in.Networking
	if n.PublicIPv4 == nil || *n.PublicIPv4 || n.PublicIPv6 == nil || !*n.PublicIPv6 {
		return nil
	}
	if bringsNetwork(in.Resources) {
		c.warn(nc, EventIPv6OnlyEgress, fmt.Sprintf("vendor %s: the nodes have no public IPv4 address and the sysexts "+
			"come from github.com, which has no IPv6: the IPv4 egress of your own network must reach it (egress is your job)",
			vendor), vendor)
		return nil
	}
	return errors.New("networking: the nodes would have IPv6 only, and the sysexts come from github.com and " +
		"objects.githubusercontent.com, which have no IPv6 address: set --sysext-base-url (it covers the sysexts " +
		"only) to a mirror reachable over IPv6, bring your own subnets or networks with IPv4 egress, or keep publicIPv4 on")
}

// bringsNetwork says the vendor block's resources use a subnet or a network of the
// user's (resources.subnets.use or resources.networks.use).
func bringsNetwork(raw json.RawMessage) bool {
	var r map[string]struct {
		Use json.RawMessage `json:"use"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	for _, kind := range []string{"subnets", "networks"} {
		if u := r[kind].Use; len(u) > 0 && string(u) != "null" {
			return true
		}
	}
	return false
}

// images asks the driver for its boot images; a driver without the capability
// resolves them at launch and counts as ready. Images are informational: a
// region without one has unavailable offerings, the NodeClass stays Ready.
func (c *Controller) images(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, vendorReady bool, noZones []string,
) (images []v1alpha1.ImageStatus, ready bool, reason, msg string) {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok || !vendorReady {
		return nil, false, ReasonImagesNotReady, "the vendor is not ready"
	}
	reporter, ok := drv.(vendors.ImageReporter)
	if !ok {
		return nil, true, ReasonImagesNotReported, "the driver resolves images at launch"
	}
	// The regions without zones have no offerings: no images are asked for them.
	if named, _ := nc.Spec.Vendors.Regions(vendor); len(named) > 0 && len(noZones) > 0 {
		keep := slices.DeleteFunc(slices.Clone(named), func(r string) bool { return slices.Contains(noZones, r) })
		if len(keep) == 0 {
			return nil, false, ReasonImagesNotReady, "none of the regions can be used yet"
		}
		nc = nc.DeepCopy()
		nc.Spec.Vendors.SetRegions(vendor, keep)
	}
	images, err := reporter.ReportImages(ctx, nc)
	if err != nil {
		return nil, false, ReasonImagesFailed, trim(err.Error())
	}
	// An image the copy manager will make (on demand) or is making is not a gap:
	// it never makes ImagesReady False, not even in a region the NodeClass names.
	var notReady, onDemand, copying []string
	readyIn := map[string]bool{}
	for _, img := range images {
		name := fmt.Sprintf("%s/%s", img.Region, img.Arch)
		switch {
		case img.Ready:
			readyIn[img.Region] = true
		case img.Reason == vendors.CodeImageCopyOnDemand:
			onDemand = append(onDemand, name)
		case img.Reason == vendors.CodeImageCopying:
			copying = append(copying, name)
		default:
			notReady = append(notReady, name)
		}
	}
	slices.Sort(notReady)
	if len(notReady) == 0 {
		reason, msg = copyReason(onDemand, copying)
		return images, true, reason, msg
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
	// The reason is the driver's own when an image that blocks gives one (for
	// example terms the account has not accepted), with that image's message.
	named, _ := nc.Spec.Vendors.Regions(vendor)
	// An image that waits for a copy never blocks, and a region that will have one
	// counts as having an image when asking whether any region does.
	noImageAnywhere := len(readyIn) == 0 && len(onDemand) == 0 && len(copying) == 0
	blocks := func(img v1alpha1.ImageStatus) bool {
		if img.Ready || img.Reason == vendors.CodeImageCopyOnDemand || img.Reason == vendors.CodeImageCopying {
			return false
		}
		return slices.Contains(named, img.Region) || noImageAnywhere
	}
	if slices.ContainsFunc(images, blocks) {
		gap := "no usable image for " + strings.Join(notReady, ", ")
		for _, img := range images {
			if blocks(img) && img.Reason != "" && conditionReason.MatchString(img.Reason) {
				return images, false, img.Reason, trim(strings.TrimPrefix(img.Message+"; "+gap, "; "))
			}
		}
		return images, false, ReasonImagesNotReady, trim(gap)
	}
	return images, true, ReasonImagesPartial, trim("no usable image for " + strings.Join(notReady, ", ") +
		"; the offerings of those regions are unavailable" + copyNote(onDemand, copying))
}

// copyReason is the reason and message of ImagesReady=True when no image is
// missing for good: ready, or only copies pending or on demand.
func copyReason(onDemand, copying []string) (reason, msg string) {
	switch {
	case len(copying) > 0:
		return ReasonImagesCopying, trim(strings.TrimPrefix(copyNote(onDemand, copying), "; "))
	case len(onDemand) > 0:
		return ReasonImagesPartial, trim(strings.TrimPrefix(copyNote(onDemand, copying), "; "))
	}
	return ReasonReady, ""
}

// copyNote says which images are being copied or will be copied on demand.
func copyNote(onDemand, copying []string) string {
	var note string
	if len(copying) > 0 {
		slices.Sort(copying)
		note += "; being copied: " + strings.Join(copying, ", ") +
			" (the offerings of those regions return when the copy is available)"
	}
	if len(onDemand) > 0 {
		slices.Sort(onDemand)
		note += "; copied on demand: " + strings.Join(onDemand, ", ")
	}
	return note
}

// imageBuildEvents publishes the image builds and copies a driver reports: one
// Event per architecture and outcome, not one per region or reconcile.
func (c *Controller) imageBuildEvents(nc *v1alpha1.SCNodeClass, vendor string, images []v1alpha1.ImageStatus) {
	if c.opts.Recorder == nil {
		return
	}
	for _, img := range images {
		typ := corev1.EventTypeNormal
		switch img.Reason {
		case EventImageBuilding, EventImageCopying:
		case EventImageBuildFailed, EventImageCopyFailed:
			typ = corev1.EventTypeWarning
		default:
			continue
		}
		c.opts.Recorder.Publish(events.Event{
			InvolvedObject: nc,
			Type:           typ,
			Reason:         img.Reason,
			Message:        trim(fmt.Sprintf("vendor %s: %s", vendor, img.Message)),
			DedupeValues:   []string{nc.Name, vendor, img.Arch, img.Reason},
			DedupeTimeout:  imageBuildDedupe,
		})
	}
}

// conditionReason is the format the API server accepts for a condition's reason
// (metav1.Condition); a driver's image reason outside it is not promoted.
var conditionReason = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// imageGaps returns the regions whose offerings are gated, with the reason
// code: the regions in which no image is ready, except those where every image
// waits for a copy on demand (the demand has to reach a launch first). A region
// with a copy in flight is ImageCopyPending, any other gap ImageGap.
func imageGaps(images []v1alpha1.ImageStatus) map[string]catalog.Code {
	ready := map[string]bool{}
	pending, failed := map[string]bool{}, map[string]bool{}
	for _, img := range images {
		ready[img.Region] = ready[img.Region] || img.Ready
		switch {
		case img.Ready || img.Reason == vendors.CodeImageCopyOnDemand:
		case img.Reason == vendors.CodeImageCopying:
			pending[img.Region] = true
		default:
			failed[img.Region] = true
		}
	}
	gaps := map[string]catalog.Code{}
	for region, ok := range ready {
		switch {
		case ok:
		case pending[region]:
			gaps[region] = catalog.ImageCopyPending
		case failed[region]:
			gaps[region] = catalog.ImageGap
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
				cond.Type == v1alpha1.VendorConditionConfigurationWarning ||
				cond.Type == v1alpha1.VendorConditionRegionsUnavailable {
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

// warnDeprecatedHardening says that the NodeClass still sets the deprecated
// spec.bootstrap.hardening, which has no effect (deduplicated like every Event).
func (c *Controller) warnDeprecatedHardening(nc *v1alpha1.SCNodeClass, vendor string) {
	if nc.Spec.Bootstrap == nil || len(nc.Spec.Bootstrap.Hardening) == 0 {
		return
	}
	c.warn(nc, EventDeprecatedHardening, "spec.bootstrap.hardening is deprecated and has no effect: "+
		"instance metadata is restricted by spec.instanceMetadata; the field will be removed in a later API version", vendor)
}

// configurationWarnings returns what the driver says about settings that have no
// effect for the vendor, sorted and joined (a stable message); empty when there
// is none or the driver has no NodeClassWarner.
func (c *Controller) configurationWarnings(nc *v1alpha1.SCNodeClass, vendor string) string {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return ""
	}
	w, ok := drv.(vendors.NodeClassWarner)
	if !ok {
		return ""
	}
	ws := slices.Clone(w.NodeClassWarnings(nc))
	slices.Sort(ws)
	return strings.Join(slices.Compact(ws), "; ")
}
