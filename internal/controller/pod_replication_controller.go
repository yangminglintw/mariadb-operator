package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/go-multierror"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v25/api/v1alpha1"
	"github.com/mariadb-operator/mariadb-operator/v25/pkg/builder"
	"github.com/mariadb-operator/mariadb-operator/v25/pkg/controller/replication"

	"github.com/mariadb-operator/mariadb-operator/v25/pkg/refresolver"
	"github.com/mariadb-operator/mariadb-operator/v25/pkg/statefulset"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	ErrDelayAutomaticFailover = errors.New("delaying automatic failover")
)

// PodReplicationController reconciles a Pod object
type PodReplicationController struct {
	client.Client
	recorder    record.EventRecorder
	builder     *builder.Builder
	refResolver *refresolver.RefResolver
	replConfig  *replication.ReplicationConfigClient
}

func NewPodReplicationController(client client.Client, recorder record.EventRecorder, builder *builder.Builder,
	refResolver *refresolver.RefResolver, replConfig *replication.ReplicationConfigClient) PodReadinessController {
	return &PodReplicationController{
		Client:      client,
		recorder:    recorder,
		builder:     builder,
		refResolver: refResolver,
		replConfig:  replConfig,
	}
}

func (r *PodReplicationController) ReconcilePodReady(ctx context.Context, pod corev1.Pod, mariadb *mariadbv1alpha1.MariaDB) error {
	logger := log.FromContext(ctx).WithName("pod-replication")
	if mariadb.Status.CurrentPrimaryPodIndex == nil {
		logger.V(1).Info("'status.currentPrimaryPodIndex' must be set. Skipping")
		return nil
	}
	logger.V(1).Info("Reconciling Pod in Ready state", "pod", pod.Name)

	index, err := statefulset.PodIndex(pod.Name)
	if err != nil {
		return fmt.Errorf("error getting Pod index: %v", err)
	}
	if *index != *mariadb.Status.CurrentPrimaryPodIndex {
		return nil
	}

	if mariadb.Status.CurrentPrimaryFailingSince != nil {
		return r.patchStatus(ctx, mariadb, func(status *mariadbv1alpha1.MariaDBStatus) {
			status.CurrentPrimaryFailingSince = nil
		})
	}

	return nil
}

func (r *PodReplicationController) ReconcilePodNotReady(ctx context.Context, pod corev1.Pod, mariadb *mariadbv1alpha1.MariaDB) error {
	if !shouldReconcile(mariadb) {
		return nil
	}
	logger := log.FromContext(ctx).WithName("pod-replication")
	if mariadb.Status.CurrentPrimaryPodIndex == nil {
		logger.V(1).Info("'status.currentPrimaryPodIndex' must be set. Skipping")
		return nil
	}
	logger.V(1).Info("Reconciling Pod in non Ready state", "pod", pod.Name)

	index, err := statefulset.PodIndex(pod.Name)
	if err != nil {
		return fmt.Errorf("error getting Pod index: %v", err)
	}
	if *index != *mariadb.Status.CurrentPrimaryPodIndex {
		return nil
	}

	now := time.Now()

	if mariadb.Status.CurrentPrimaryFailingSince == nil {
		err := r.patchStatus(ctx, mariadb, func(status *mariadbv1alpha1.MariaDBStatus) {
			status.CurrentPrimaryFailingSince = &metav1.Time{Time: now}
		})
		if err != nil {
			return fmt.Errorf("error patching MariaDB: %v", err)
		}
	}

	autoFailoverDelay := mariadb.GetAutomaticFailoverDelay()
	if autoFailoverDelay > 0 {
		failoverTime := mariadb.Status.CurrentPrimaryFailingSince.Add(autoFailoverDelay)
		if failoverTime.After(now) {
			// To delay automatic failover we must abort and requeue later.
			// When the 'PodController' controller receives the 'ErrDelayAutomaticFailover' error, it requeues without error.
			// See: https://github.com/mariadb-operator/mariadb-operator/pull/1287
			return ErrDelayAutomaticFailover
		}
	}

	primary := mariadb.Status.CurrentPrimaryPodIndex

	newPrimaryName, err := replication.NewFailoverHandler(
		r.Client,
		mariadb,
		log.FromContext(ctx).WithName("failover").V(1),
	).FurthestAdvancedReplica(ctx)
	if err != nil {
		return fmt.Errorf("error getting promotion candidate: %v", err)
	}
	newPrimary, err := statefulset.PodIndex(newPrimaryName)
	if err != nil {
		return fmt.Errorf("error getting new primary Pod index: %v", err)
	}

	var errBundle *multierror.Error
	err = r.patch(ctx, mariadb, func(mdb *mariadbv1alpha1.MariaDB) {
		mdb.Spec.Replication.Primary.PodIndex = newPrimary
	})
	errBundle = multierror.Append(errBundle, err)

	err = r.patchStatus(ctx, mariadb, func(status *mariadbv1alpha1.MariaDBStatus) {
		status.CurrentPrimaryFailingSince = nil
	})
	errBundle = multierror.Append(errBundle, err)

	if err := errBundle.ErrorOrNil(); err != nil {
		return fmt.Errorf("error patching MariaDB: %v", err)
	}

	logger.Info("Switching primary", "primary", primary, "new-primary", *newPrimary)
	r.recorder.Eventf(mariadb, corev1.EventTypeNormal, mariadbv1alpha1.ReasonPrimarySwitching,
		"Switching primary from index '%d' to index '%d'", *primary, *newPrimary)

	return nil
}

// ReconcilePodTerminating handles graceful switchover when primary pod is being deleted.
// This is called when DeletionTimestamp is set but pod is still Ready.
func (r *PodReplicationController) ReconcilePodTerminating(ctx context.Context, pod corev1.Pod,
	mariadb *mariadbv1alpha1.MariaDB) error {
	logger := log.FromContext(ctx).WithName("pod-replication")

	// Only process if replication is enabled and auto-failover is on
	if !shouldReconcile(mariadb) {
		return nil
	}

	// Check if this is the primary pod
	if mariadb.Status.CurrentPrimaryPodIndex == nil {
		logger.V(1).Info("'status.currentPrimaryPodIndex' must be set. Skipping")
		return nil
	}

	index, err := statefulset.PodIndex(pod.Name)
	if err != nil {
		return fmt.Errorf("error getting Pod index: %v", err)
	}

	if *index != *mariadb.Status.CurrentPrimaryPodIndex {
		logger.V(1).Info("Replica pod terminating, no action needed", "pod", pod.Name)
		return nil
	}

	// Primary pod is being deleted - initiate graceful switchover
	logger.Info("Primary pod terminating, initiating graceful switchover", "pod", pod.Name)
	r.recorder.Eventf(mariadb, corev1.EventTypeNormal, mariadbv1alpha1.ReasonPrimarySwitching,
		"Primary pod %s terminating, initiating graceful switchover", pod.Name)

	// Find the best replica to promote (highest GTID position)
	newPrimaryName, err := replication.NewFailoverHandler(
		r.Client,
		mariadb,
		logger.WithName("graceful-switchover").V(1),
	).FurthestAdvancedReplica(ctx)
	if err != nil {
		return fmt.Errorf("error finding switchover target: %v", err)
	}

	newPrimaryIndex, err := statefulset.PodIndex(newPrimaryName)
	if err != nil {
		return fmt.Errorf("error getting new primary index: %v", err)
	}

	primary := mariadb.Status.CurrentPrimaryPodIndex

	// Patch the MariaDB spec to trigger switchover
	// The existing reconcileSwitchover() in mariadb_controller will handle the 6-phase process
	if err := r.patch(ctx, mariadb, func(mdb *mariadbv1alpha1.MariaDB) {
		mdb.Spec.Replication.Primary.PodIndex = newPrimaryIndex
	}); err != nil {
		return fmt.Errorf("error patching MariaDB for switchover: %v", err)
	}

	logger.Info("Graceful switchover initiated", "from-primary", *primary, "to-primary", *newPrimaryIndex)
	return nil
}

func shouldReconcile(mdb *mariadbv1alpha1.MariaDB) bool {
	if mdb.IsMaxScaleEnabled() || mdb.IsSwitchingPrimary() || mdb.IsReplicationSwitchoverRequired() ||
		mdb.IsRestoringBackup() || mdb.IsResizingStorage() || mdb.IsSuspended() {
		return false
	}
	primaryRepl := ptr.Deref(mdb.Spec.Replication, mariadbv1alpha1.Replication{}).Primary
	autoFailover := ptr.Deref(primaryRepl.AutoFailover, true)
	return mdb.IsReplicationEnabled() && autoFailover && mdb.HasConfiguredReplica()
}

func (r *PodReplicationController) patch(ctx context.Context, mariadb *mariadbv1alpha1.MariaDB,
	patcher func(*mariadbv1alpha1.MariaDB)) error {
	patch := client.MergeFrom(mariadb.DeepCopy())
	patcher(mariadb)

	if err := r.Patch(ctx, mariadb, patch); err != nil {
		return fmt.Errorf("error patching MariaDB: %v", err)
	}
	return nil
}

func (r *PodReplicationController) patchStatus(ctx context.Context, mariadb *mariadbv1alpha1.MariaDB,
	patcher func(*mariadbv1alpha1.MariaDBStatus)) error {
	patch := client.MergeFrom(mariadb.DeepCopy())
	patcher(&mariadb.Status)

	if err := r.Client.Status().Patch(ctx, mariadb, patch); err != nil {
		return fmt.Errorf("error patching MariaDB status: %v", err)
	}
	return nil
}
