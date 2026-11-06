/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// The bootstrap token Secret (overview section 8.1).
const (
	// TokenNamespace is where bootstrap token Secrets live; the API server's
	// bootstrap authenticator reads them there.
	TokenNamespace = "kube-system"
	// TokenType is the type of a bootstrap token Secret.
	TokenType = corev1.SecretType("bootstrap.kubernetes.io/token")
	// TokenSecretPrefix is the prefix of a token Secret's name.
	TokenSecretPrefix = "bootstrap-token-"
	// LabelNodeClaim ties a token Secret to its NodeClaim, LabelNodeClass to
	// the NodeClass (for Events).
	LabelNodeClaim = v1alpha1.Group + "/nodeclaim"
	LabelNodeClass = v1alpha1.Group + "/nodeclass"
	// TokenGroup is the group the kubelet authenticates with, besides
	// system:bootstrappers.
	TokenGroup = "system:bootstrappers:scarpenter"
	// MinTokenTTL is the shortest accepted token lifetime: core gives a
	// NodeClaim 15 minutes to register, and the token must outlive that and the
	// retries of Create within it.
	MinTokenTTL = 20 * time.Minute

	tokenIDLength     = 6
	tokenSecretLength = 16
	tokenAlphabet     = "abcdefghijklmnopqrstuvwxyz0123456789"
	maxIDAttempts     = 5
)

// Event reasons for the removal of a token.
const (
	// EventTokenDeleted is published when a token is deleted, with the reason.
	EventTokenDeleted = "BootstrapTokenDeleted"
	// ReasonRegistered: the NodeClaim registered, the token has done its job.
	ReasonRegistered = "NodeClaimRegistered"
	// ReasonNodeClaimGone: the NodeClaim no longer exists.
	ReasonNodeClaimGone = "NodeClaimNotFound"
	// ReasonNodeClaimDeleting: the NodeClaim is being deleted.
	ReasonNodeClaimDeleting = "NodeClaimDeleting"
)

// MinterOptions configures a Minter.
type MinterOptions struct {
	// Client writes the Secrets.
	Client client.Client
	// Reader reads them; nil uses Client. The manager's APIReader avoids a
	// cluster-wide Secret informer and sees a token the instant it was written.
	Reader client.Reader
	// TTL is the lifetime of a token (--bootstrap-token-ttl).
	TTL      time.Duration
	Clock    clock.Clock
	Recorder events.Recorder
}

// Minter creates and deletes the bootstrap token of a NodeClaim. It implements
// the TokenMinter of the CloudProvider.
type Minter struct {
	opts MinterOptions
	mu   sync.Mutex
}

// NewMinter returns a Minter.
func NewMinter(opts MinterOptions) *Minter {
	if opts.Reader == nil {
		opts.Reader = opts.Client
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	return &Minter{opts: opts}
}

// Mint returns the token ("<id>.<secret>") of the NodeClaim, creating its
// Secret when there is none. It is idempotent: a retried Create gets the token
// that an earlier attempt may have put on an instance, never a rotated one. A
// token that expired but still exists (the token cleaner is lazy) is replaced.
func (m *Minter) Mint(ctx context.Context, nodeClaim *karpv1.NodeClaim) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.find(ctx, nodeClaim.Name)
	if err != nil {
		return "", err
	}
	for i := range existing {
		s := &existing[i]
		token, expires, ok := parseToken(s)
		if ok && m.opts.Clock.Now().Before(expires) {
			return token, nil
		}
		// Unusable or expired: replace it.
		if err := m.opts.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("deleting the expired bootstrap token %s: %w", s.Name, err)
		}
	}

	expiration := m.opts.Clock.Now().Add(m.opts.TTL).UTC()
	for range maxIDAttempts {
		id, err := randomString(tokenIDLength)
		if err != nil {
			return "", err
		}
		secret, err := randomString(tokenSecretLength)
		if err != nil {
			return "", err
		}
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: TokenSecretPrefix + id, Namespace: TokenNamespace,
				Labels: map[string]string{LabelNodeClaim: nodeClaim.Name},
			},
			Type: TokenType,
			Data: map[string][]byte{
				"token-id":                       []byte(id),
				"token-secret":                   []byte(secret),
				"expiration":                     []byte(expiration.Format(time.RFC3339)),
				"usage-bootstrap-authentication": []byte("true"),
				"auth-extra-groups":              []byte(TokenGroup),
				"description":                    []byte("SCarpenter bootstrap token of the NodeClaim " + nodeClaim.Name),
			},
		}
		if ref := nodeClaim.Spec.NodeClassRef; ref != nil && ref.Name != "" {
			s.Labels[LabelNodeClass] = ref.Name
		}
		switch err := m.opts.Client.Create(ctx, s); {
		case err == nil:
			return id + "." + secret, nil
		case apierrors.IsAlreadyExists(err):
			continue // a collision of the random ID: try another
		default:
			return "", fmt.Errorf("creating the bootstrap token Secret: %w", err)
		}
	}
	return "", errors.New("creating the bootstrap token Secret: no free token ID after several attempts")
}

// Delete removes the NodeClaim's token (without an Event); a missing token is
// not an error. It implements the TokenMinter of the CloudProvider.
func (m *Minter) Delete(ctx context.Context, nodeClaimName string) error {
	_, err := m.DeleteFor(ctx, nodeClaimName, "", nil)
	return err
}

// DeleteFor removes the NodeClaim's token and publishes an Event with the
// reason (when it deleted one and the reason is not empty): on the NodeClaim
// when it is given, else on the SCNodeClass named by the token's label.
func (m *Minter) DeleteFor(
	ctx context.Context, nodeClaimName, reason string, nodeClaim *karpv1.NodeClaim,
) (deleted bool, err error) {
	secrets, err := m.find(ctx, nodeClaimName)
	if err != nil {
		return false, err
	}
	var errs []error
	for i := range secrets {
		s := &secrets[i]
		if err := m.opts.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("deleting the bootstrap token %s: %w", s.Name, err))
			}
			continue
		}
		deleted = true
		log.FromContext(ctx).V(1).Info("deleted the bootstrap token",
			"nodeclaim", nodeClaimName, "secret", s.Name, "reason", reason)
		if reason != "" {
			m.publish(nodeClaimName, reason, s, nodeClaim)
		}
	}
	return deleted, errors.Join(errs...)
}

func (m *Minter) publish(nodeClaimName, reason string, s *corev1.Secret, nodeClaim *karpv1.NodeClaim) {
	if m.opts.Recorder == nil {
		return
	}
	if nodeClaim == nil {
		// The NodeClaim is gone: an object reference with its name still
		// attaches the Event to it for as long as Events are kept.
		nodeClaim = &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: nodeClaimName}}
	}
	m.opts.Recorder.Publish(events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeNormal,
		Reason:         EventTokenDeleted,
		Message:        fmt.Sprintf("deleted the bootstrap token %s: %s", s.Name, reason),
		DedupeValues:   []string{nodeClaimName, s.Name},
		DedupeTimeout:  time.Minute,
	})
}

func (m *Minter) find(ctx context.Context, nodeClaimName string) ([]corev1.Secret, error) {
	var list corev1.SecretList
	if err := m.opts.Reader.List(ctx, &list, client.InNamespace(TokenNamespace),
		client.MatchingLabels{LabelNodeClaim: nodeClaimName}); err != nil {
		return nil, fmt.Errorf("listing the bootstrap tokens of %s: %w", nodeClaimName, err)
	}
	out := list.Items[:0]
	for _, s := range list.Items {
		if s.Type == TokenType {
			out = append(out, s)
		}
	}
	return out, nil
}

// parseToken returns the token and expiration of a token Secret.
func parseToken(s *corev1.Secret) (token string, expires time.Time, ok bool) {
	get := func(k string) string { return string(s.Data[k]) }
	id, secret := get("token-id"), get("token-secret")
	if id == "" || secret == "" {
		return "", time.Time{}, false
	}
	expires, err := time.Parse(time.RFC3339, get("expiration"))
	if err != nil {
		return "", time.Time{}, false
	}
	return id + "." + secret, expires, true
}

// randomString returns n characters of [a-z0-9] from crypto/rand, uniformly.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(tokenAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		b[i] = tokenAlphabet[v.Int64()]
	}
	return string(b), nil
}
