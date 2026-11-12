package controller

import (
	"context"
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

// AIGridReconciler reconciles AIGrid objects.
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids/finalizers,verbs=update
// +kubebuilder:rbac:groups=llm.redhat.com,resources=llmworkloadenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
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
	// joining, GitOps syncing). Phases run strictly in order except Wiring
	// and GitOpsSynced, which overlap by design: the grid's own connect
	// checks retry, so the grid can install while DNS wiring lands.
	steps := []struct {
		condition string
		run       func(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error)
	}{
		{aigridv1alpha1.ConditionProvisioned, r.provision},
		{aigridv1alpha1.ConditionImported, r.importSites},
		{aigridv1alpha1.ConditionWired, r.wire},
		{aigridv1alpha1.ConditionGitOpsSynced, r.installGitOps},
		{aigridv1alpha1.ConditionGridReady, r.awaitGrid},
	}

	requeue := false
	for _, step := range steps {
		done, err := step.run(ctx, grid)
		if err != nil {
			logger.Error(err, "phase failed", "condition", step.condition)
			setGridCondition(grid, step.condition, metav1.ConditionFalse, "PhaseError", err.Error())
			if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
				// Retryable object-state races: back off briefly.
				return ctrl.Result{RequeueAfter: 5 * time.Second}, r.updateStatus(ctx, grid)
			}
			return ctrl.Result{}, r.updateStatus(ctx, grid)
		}
		if !done {
			setGridCondition(grid, step.condition, metav1.ConditionFalse, "InProgress", "phase in progress")
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

var errNoHub = fmt.Errorf("no hub site in grid")
