package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AIGridReconciler reconciles AIGrid objects. Host-cluster RBAC markers
// live in doc.go at the package level; see that file for why.
type AIGridReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ZoneServerImage is the CoreDNS image used for per-grid zone servers.
	ZoneServerImage string
}

const gridFinalizer = "aigrid.llm.redhat.com/finalizer"

// phaseWait is the requeue interval used while long-running phases
// (site bring-up, import, GitOps fan-out) are still in progress.
const phaseWait = 30 * time.Second

func (r *AIGridReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("grid", req.NamespacedName)

	grid := &aigridv1alpha1.AIGrid{}
	if err := r.Get(ctx, req.NamespacedName, grid); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !grid.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, grid)
	}
	if err := r.ensureFinalizer(ctx, grid); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile is a phase machine; each phase is idempotent and returns
	// done=false while its work is still in flight (sites booting, imports
	// joining, GitOps syncing). Phases are strictly sequential in v1: each
	// must complete before the next begins. The ordering is sound because
	// wiring only needs site control planes (not the grid), and the grid's
	// own connect checks retry — so installing the grid after wiring means
	// it comes up green on the first try. Overlapping Wiring and
	// GitOpsSynced is a possible future optimization, not implemented.
	steps := []struct {
		condition string
		run       func(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, string, error)
	}{
		{aigridv1alpha1.ConditionProvisioned, phaseDetail(r.provision)},
		{aigridv1alpha1.ConditionHubBootstrapped, phaseDetail(r.bootstrapHub)},
		{aigridv1alpha1.ConditionImported, phaseDetail(r.importSites)},
		{aigridv1alpha1.ConditionWired, phaseDetail(r.wire)},
		{aigridv1alpha1.ConditionGitOpsSynced, r.installGitOps},
		{aigridv1alpha1.ConditionGridReady, phaseDetail(r.awaitGrid)},
	}

	requeue := false
	for _, step := range steps {
		done, detail, err := step.run(ctx, grid)
		if err != nil {
			logger.Error(err, "phase failed", "condition", step.condition)
			// Pull-secret failures report their own reason and retry on
			// the phase cadence: fixing the secret self-heals the grid.
			var secretErr *pullSecretError
			if errors.As(err, &secretErr) {
				setGridCondition(grid, step.condition, metav1.ConditionFalse, secretErr.reason, secretErr.message)
				return ctrl.Result{RequeueAfter: phaseWait}, r.updateStatus(ctx, grid)
			}
			setGridCondition(grid, step.condition, metav1.ConditionFalse, "PhaseError", err.Error())
			if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
				// Retryable object-state races: back off briefly.
				return ctrl.Result{RequeueAfter: 5 * time.Second}, r.updateStatus(ctx, grid)
			}
			// Report the failure in status, then return the error so
			// controller-runtime requeues with backoff.
			if statusErr := r.updateStatus(ctx, grid); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}
		if !done {
			message := "phase in progress"
			if detail != "" {
				message = detail
			} else if blocking := blockingSiteMessage(grid); blocking != "" {
				message = blocking
			}
			setGridCondition(grid, step.condition, metav1.ConditionFalse, "InProgress", message)
			requeue = true
			break
		}
		setGridCondition(grid, step.condition, metav1.ConditionTrue, "PhaseComplete", "phase complete")
	}

	grid.Status.ObservedGeneration = grid.Generation
	if err := r.updateStatus(ctx, grid); err != nil {
		return ctrl.Result{}, err
	}
	if requeue {
		return ctrl.Result{RequeueAfter: phaseWait}, nil
	}
	return ctrl.Result{}, nil
}

func (r *AIGridReconciler) ensureFinalizer(ctx context.Context, grid *aigridv1alpha1.AIGrid) error {
	if containsString(grid.Finalizers, gridFinalizer) {
		return nil
	}
	patch := client.MergeFrom(grid.DeepCopy())
	grid.Finalizers = append(grid.Finalizers, gridFinalizer)
	return r.Patch(ctx, grid, patch)
}

// teardown removes hub-side objects that outlive their LWEs (ManagedClusters,
// the root Argo Application). Everything else — sites, control-plane
// namespaces, policies, the zone server — dies with the LWEs, which are
// garbage-collected through owner references.
func (r *AIGridReconciler) teardown(ctx context.Context, grid *aigridv1alpha1.AIGrid) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	hub := r.hubClient(ctx, grid)
	if hub != nil {
		for _, site := range grid.Status.Sites {
			if err := deleteUnstructured(ctx, hub, managedClusterGVK, "", site.Name); err != nil {
				logger.Error(err, "deleting stale ManagedCluster", "site", site.Name)
				return ctrl.Result{}, err
			}
		}
		if err := deleteUnstructured(ctx, hub, applicationGVK, gitOpsNamespace, grid.Name); err != nil {
			logger.Error(err, "deleting root Application")
			return ctrl.Result{}, err
		}
	}

	if containsString(grid.Finalizers, gridFinalizer) {
		patch := client.MergeFrom(grid.DeepCopy())
		grid.Finalizers = removeString(grid.Finalizers, gridFinalizer)
		if err := r.Patch(ctx, grid, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *AIGridReconciler) updateStatus(ctx context.Context, grid *aigridv1alpha1.AIGrid) error {
	existing := &aigridv1alpha1.AIGrid{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: grid.Namespace, Name: grid.Name}, existing); err != nil {
		return client.IgnoreNotFound(err)
	}
	existing.Status = grid.Status
	return r.Status().Update(ctx, existing)
}

func setGridCondition(grid *aigridv1alpha1.AIGrid, conditionType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&grid.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: grid.Generation,
	})
}

func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(items []string, s string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != s {
			out = append(out, item)
		}
	}
	return out
}

// SetupWithManager registers the controller. LWEs are watched so that site
// readiness transitions requeue their grids promptly.
func (r *AIGridReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aigridv1alpha1.AIGrid{}).
		Complete(r)
}

func siteNamespacedName(grid *aigridv1alpha1.AIGrid, site aigridv1alpha1.SiteSpec) types.NamespacedName {
	return types.NamespacedName{Namespace: grid.Namespace, Name: SiteName(grid, site)}
}

// phaseDetail adapts a (bool, error) phase function to the message-carrying
// shape used by the phase machine.
func phaseDetail(run func(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error)) func(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, string, error) {
	return func(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, string, error) {
		done, err := run(ctx, grid)
		return done, "", err
	}
}

var errNoHub = fmt.Errorf("no hub site in grid")
