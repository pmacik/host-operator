package templateupdaterequest

import (
	"context"
	"strings"
	"time"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	tierutil "github.com/codeready-toolchain/host-operator/controllers/nstemplatetier/util"
	"github.com/codeready-toolchain/host-operator/controllers/toolchainconfig"
	"github.com/codeready-toolchain/toolchain-common/pkg/condition"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	errs "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&toolchainv1alpha1.TemplateUpdateRequest{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&source.Kind{Type: &toolchainv1alpha1.MasterUserRecord{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &toolchainv1alpha1.Space{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

// Reconciler reconciles a TemplateUpdateRequest object
type Reconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=templateupdaterequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=templateupdaterequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=templateupdaterequests/finalizers,verbs=update

// Reconcile reads that state of the cluster for a TemplateUpdateRequest object and makes changes based on the state read
// and what is in the TemplateUpdateRequest.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling TemplateUpdateRequest")

	// Fetch the TemplateUpdateRequest tur
	tur := &toolchainv1alpha1.TemplateUpdateRequest{}
	err := r.Client.Get(context.TODO(), request.NamespacedName, tur)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "unable to get the current TemplateUpdateRequest")
		return reconcile.Result{}, errs.Wrap(err, "unable to get the current TemplateUpdateRequest")
	}

	if tur.Spec.CurrentTierHash != "" {
		return r.handleSpaceUpdate(logger, request, tur)
	}
	return r.handleMURUpdate(logger, request, tur)
}

func (r *Reconciler) handleSpaceUpdate(logger logr.Logger, request ctrl.Request, tur *toolchainv1alpha1.TemplateUpdateRequest) (ctrl.Result, error) {
	// lookup the Space with the same name as the TemplateUpdateRequest tur
	space := &toolchainv1alpha1.Space{}
	if err := r.Client.Get(context.TODO(), request.NamespacedName, space); err != nil {
		if errors.IsNotFound(err) {
			// Space object not found, could have been deleted after reconcile request.
			// Marking this TemplateUpdateRequest as failed
			return reconcile.Result{}, r.addFailureStatusCondition(tur, err)
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Unable to get the Space associated with the TemplateUpdateRequest")
		return reconcile.Result{}, errs.Wrap(err, "unable to get the Space associated with the TemplateUpdateRequest")
	}

	labelKey := tierutil.TemplateTierHashLabelKey(space.Spec.TierName)
	// if the tier hash has changed and the Space is in ready state then the update is complete
	if tur.Spec.CurrentTierHash != space.Labels[labelKey] && condition.IsTrue(space.Status.Conditions, toolchainv1alpha1.ConditionReady) {
		// once the Space is up-to-date, we can delete this TemplateUpdateRequest
		logger.Info("Space is up-to-date. Marking the TemplateUpdateRequest as complete")
		return reconcile.Result{}, r.setCompleteStatusCondition(tur)
	}

	// otherwise, we need to wait
	logger.Info("Space still being updated...")
	if err := r.addUpdatingStatusCondition(tur, map[string]string{}); err != nil {
		logger.Error(err, "Unable to update the TemplateUpdateRequest status")
		return reconcile.Result{}, errs.Wrap(err, "unable to update the TemplateUpdateRequest status")
	}
	// no explicit requeue: expect new reconcile loop when Space changes
	return reconcile.Result{}, nil
}

func (r *Reconciler) handleMURUpdate(logger logr.Logger, request ctrl.Request, tur *toolchainv1alpha1.TemplateUpdateRequest) (ctrl.Result, error) {
	config, err := toolchainconfig.GetToolchainConfig(r.Client)
	if err != nil {
		return reconcile.Result{}, errs.Wrapf(err, "unable to get ToolchainConfig")
	}

	// lookup the MasterUserRecord with the same name as the TemplateUpdateRequest tur
	mur := &toolchainv1alpha1.MasterUserRecord{}
	if err := r.Client.Get(context.TODO(), request.NamespacedName, mur); err != nil {
		if errors.IsNotFound(err) {
			// MUR object not found, could have been deleted after reconcile request.
			// Marking this TemplateUpdateRequest as failed
			return reconcile.Result{}, r.addFailureStatusCondition(tur, err)
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Unable to get the MasterUserRecord associated with the TemplateUpdateRequest")
		return reconcile.Result{}, errs.Wrap(err, "unable to get the MasterUserRecord associated with the TemplateUpdateRequest")
	}
	if len(tur.Status.SyncIndexes) == 0 {
		// if the TemplateUpdateRequest was just created (ie, `Status.SyncIndexes` is empty),
		// then we should update the associated MasterUserRecord
		// and retain its current syncIndexex in the status
		// NOTE: indexes need to be "captured" before updating the MURs
		syncIndexes := syncIndexes(tur.Spec.TierName, *mur)
		if err := r.updateTemplateRefs(logger, *tur, mur); err != nil {
			// we want to give ourselves a few chances before marking this MasterUserRecord update as "failed":
			logger.Error(err, "Unable to update the MasterUserRecord associated with the TemplateUpdateRequest")
			err = errs.Wrap(err, "unable to update the MasterUserRecord associated with the TemplateUpdateRequest")
			// log the failure in the status...
			if err2 := r.addFailureStatusCondition(tur, err); err2 != nil {
				return reconcile.Result{}, err2
			}
			if maxUpdateFailuresReached(*tur, config.Users().MasterUserRecordUpdateFailureThreshold()) {
				// exit reconcile loop but don't requeue
				// in other words, give up with the MasterUserRecord update :(
				return reconcile.Result{}, nil
			}
			// requeue with a delay (and cross fingers for the update to succeed next time)
			logger.Info("Retaining the failure in the TemplateUpdateRequest 'status.conditions'")
			return reconcile.Result{Requeue: true, RequeueAfter: 5 * time.Second}, err
		}
		// update the TemplateUpdateRequest status and requeue to keep tracking the MUR changes
		logger.Info("MasterUserRecord update started. Updating TemplateUpdateRequest status accordingly")
		if err = r.addUpdatingStatusCondition(tur, syncIndexes); err != nil {
			logger.Error(err, "Unable to update the TemplateUpdateRequest status")
			return reconcile.Result{}, errs.Wrap(err, "unable to update the TemplateUpdateRequest status")
		}
		// no explicit requeue: expect new reconcile loop when MasterUserRecord changes
		return reconcile.Result{}, nil
	}
	// otherwise, we should compare the sync indexes of the MasterUserRecord until all tier-related values changed
	if r.allSyncIndexesChanged(logger, *tur, *mur) && condition.IsTrue(mur.Status.Conditions, toolchainv1alpha1.ConditionReady) {
		// once MasterUserRecord is up-to-date, we can delete this TemplateUpdateRequest
		logger.Info("MasterUserRecord is up-to-date. Marking the TemplateUpdateRequest as complete")
		return reconcile.Result{}, r.setCompleteStatusCondition(tur)
	}
	// otherwise, we need to wait
	logger.Info("MasterUserRecord still being updated...")
	// no explicit requeue: expect new reconcile loop when MasterUserRecord changes
	return reconcile.Result{}, nil
}

// maxUpdateFailuresReached checks if the maximum number of attempts to update the MasterUserRecord was reached
func maxUpdateFailuresReached(tur toolchainv1alpha1.TemplateUpdateRequest, threshod int) bool {
	return condition.Count(tur.Status.Conditions,
		toolchainv1alpha1.TemplateUpdateRequestComplete,
		corev1.ConditionFalse,
		toolchainv1alpha1.TemplateUpdateRequestUnableToUpdateReason) >= threshod
}

func (r Reconciler) updateTemplateRefs(logger logr.Logger, tur toolchainv1alpha1.TemplateUpdateRequest, mur *toolchainv1alpha1.MasterUserRecord) error {
	// update MasterUserRecord accounts whose tier matches the TemplateUpdateRequest
	for i, ua := range mur.Spec.UserAccounts {
		if ua.Spec.NSTemplateSet != nil && ua.Spec.NSTemplateSet.TierName == tur.Spec.TierName {
			logger.Info("updating templaterefs", "tier", tur.Spec.TierName, "target_cluster", ua.TargetCluster)
			namespaces := make(map[string]toolchainv1alpha1.NSTemplateSetNamespace, len(ua.Spec.NSTemplateSet.Namespaces))
			// now, add the new templateRefs, unless there's a custom template in use
			for _, ns := range tur.Spec.Namespaces {
				t := namespaceType(ns.TemplateRef)
				namespaces[t] = toolchainv1alpha1.NSTemplateSetNamespace(ns)
			}
			// finally, set the new namespace templates in the user account
			ua.Spec.NSTemplateSet.Namespaces = []toolchainv1alpha1.NSTemplateSetNamespace{}
			for _, ns := range namespaces {
				ua.Spec.NSTemplateSet.Namespaces = append(ua.Spec.NSTemplateSet.Namespaces, ns)
			}
			// now, let's take care about the cluster resources
			if tur.Spec.ClusterResources != nil {
				ua.Spec.NSTemplateSet.ClusterResources = &toolchainv1alpha1.NSTemplateSetClusterResources{
					TemplateRef: tur.Spec.ClusterResources.TemplateRef,
				}
			} else {
				ua.Spec.NSTemplateSet.ClusterResources = nil
			}
			mur.Spec.UserAccounts[i] = ua
			// also, update the tier template hash label
			hash, err := tierutil.ComputeHashForNSTemplateSetSpec(*ua.Spec.NSTemplateSet)
			if err != nil {
				return err
			}
			mur.Labels[tierutil.TemplateTierHashLabelKey(tur.Spec.TierName)] = hash
		}
	}
	logger.Info("updating the MUR")
	return r.Client.Update(context.TODO(), mur)

}

// extract the type from the given templateRef
// templateRef format: `<tier>-<type>-<hash>`
func namespaceType(templateRef string) string {
	parts := strings.Split(templateRef, "-")
	return parts[1] // TODO: check for index out of range errors
}

// allSyncIndexesChanged compares the sync indexes in the given TemplateUpdateRequest status vs the given MasterUserRecord
// returns `true` if ALL values are DIFFERENT, meaning that all user accounts were updated on the target clusters where the tier is in use
func (r Reconciler) allSyncIndexesChanged(logger logr.Logger, tur toolchainv1alpha1.TemplateUpdateRequest, mur toolchainv1alpha1.MasterUserRecord) bool {
	murIndexes := syncIndexes(tur.Spec.TierName, mur)
	for targetCluster, syncIndex := range tur.Status.SyncIndexes {
		if current, ok := murIndexes[targetCluster]; ok && current == syncIndex {
			logger.Info("Sync index still unchanged", "target_cluster", targetCluster, "sync_index", syncIndex)
			return false
		}
	}
	logger.Info("All sync indexes have been updated")
	return true
}

// --------------------------------------------------
// status updates
// --------------------------------------------------

// ToFailure condition when an error occurred
func ToFailure(err error) toolchainv1alpha1.Condition {
	return toolchainv1alpha1.Condition{
		Type:    toolchainv1alpha1.TemplateUpdateRequestComplete,
		Status:  corev1.ConditionFalse,
		Reason:  toolchainv1alpha1.TemplateUpdateRequestUnableToUpdateReason,
		Message: err.Error(),
	}
}

// ToBeUpdating condition when the update is in progress
func ToBeUpdating() toolchainv1alpha1.Condition {
	return toolchainv1alpha1.Condition{
		Type:               toolchainv1alpha1.TemplateUpdateRequestComplete,
		Status:             corev1.ConditionFalse,
		Reason:             toolchainv1alpha1.TemplateUpdateRequestUpdatingReason,
		LastTransitionTime: metav1.Now(),
	}
}

// ToBeComplete condition when the update completed with success
func ToBeComplete() toolchainv1alpha1.Condition {
	return toolchainv1alpha1.Condition{
		Type:               toolchainv1alpha1.TemplateUpdateRequestComplete,
		Status:             corev1.ConditionTrue,
		Reason:             toolchainv1alpha1.TemplateUpdateRequestUpdatedReason,
		LastTransitionTime: metav1.Now(),
	}
}

// addUpdatingStatusCondition sets the TemplateUpdateRequest status condition to `complete=false/reason=updating` and retains the sync indexes
func (r *Reconciler) addUpdatingStatusCondition(tur *toolchainv1alpha1.TemplateUpdateRequest, syncIndexes map[string]string) error {
	tur.Status.SyncIndexes = syncIndexes
	tur.Status.Conditions = []toolchainv1alpha1.Condition{ToBeUpdating()}
	return r.Client.Status().Update(context.TODO(), tur)
}

// addFailureStatusCondition appends a new TemplateUpdateRequest status condition to `complete=false/reason=updating`
func (r *Reconciler) addFailureStatusCondition(tur *toolchainv1alpha1.TemplateUpdateRequest, err error) error {
	tur.Status.Conditions = condition.AddStatusConditions(tur.Status.Conditions, ToFailure(err))
	return r.Client.Status().Update(context.TODO(), tur)
}

// setCompleteStatusCondition sets the TemplateUpdateRequest status condition to `complete=true/reason=updated` and clears all previous conditions of the same type
func (r *Reconciler) setCompleteStatusCondition(tur *toolchainv1alpha1.TemplateUpdateRequest) error {
	tur.Status.Conditions = []toolchainv1alpha1.Condition{ToBeComplete()}
	return r.Client.Status().Update(context.TODO(), tur)
}

// syncIndexes returns the sync indexes related to the given tier, indexed by target cluster
func syncIndexes(tierName string, mur toolchainv1alpha1.MasterUserRecord) map[string]string {
	indexes := map[string]string{}
	for _, ua := range mur.Spec.UserAccounts {
		if ua.Spec.NSTemplateSet != nil && ua.Spec.NSTemplateSet.TierName == tierName {
			indexes[ua.TargetCluster] = ua.SyncIndex
		}
	}
	return indexes
}
