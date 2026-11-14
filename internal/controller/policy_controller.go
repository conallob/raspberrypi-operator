// Package controller reconciles BootloaderPolicy objects into per-node
// NodeBootloader objects and drives the rollout.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/plan"
)

const (
	// Finalizer makes sure nodes we cordoned are returned to service when a
	// policy is deleted.
	Finalizer = "rpi.conall.dev/uncordon"
	// AnnotationCordoned marks a NodeBootloader whose node this controller
	// cordoned. A node cordoned by someone else is never uncordoned by us.
	AnnotationCordoned = "rpi.conall.dev/cordoned"
	// AnnotationDrainedHash records the target hash a node was drained for.
	AnnotationDrainedHash = "rpi.conall.dev/drained-hash"

	requeueBusy = 15 * time.Second
	requeueIdle = 5 * time.Minute
)

var metav1GetOptions = metav1.GetOptions{}

// PolicyReconciler reconciles BootloaderPolicy.
type PolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Drainer  Drainer
	Recorder record.EventRecorder
	Now      func() time.Time
}

// +kubebuilder:rbac:groups=rpi.conall.dev,resources=bootloaderpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rpi.conall.dev,resources=bootloaderpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rpi.conall.dev,resources=bootloaderpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=rpi.conall.dev,resources=nodebootloaders,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="apps",resources=daemonsets,verbs=get;list;watch

// Reconcile implements reconcile.Reconciler.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pol rpiv1.BootloaderPolicy
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pol.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &pol)
	}
	if controllerutil.AddFinalizer(&pol, Finalizer) {
		if err := r.Update(ctx, &pol); err != nil {
			return ctrl.Result{}, err
		}
	}

	nodes, err := r.selectedNodes(ctx, &pol)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncNodeObjects(ctx, &pol, nodes); err != nil {
		return ctrl.Result{}, err
	}

	var nbs rpiv1.NodeBootloaderList
	if err := r.List(ctx, &nbs); err != nil {
		return ctrl.Result{}, err
	}
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = nodeReady(&n)
	}
	var mine []rpiv1.NodeBootloader
	var states []plan.Node
	for _, nb := range nbs.Items {
		if nb.Spec.PolicyName != pol.Name || !r.isSelected(nodes, nb.Name) {
			continue
		}
		mine = append(mine, nb)
		states = append(states, nodeState(&nb, ready[nb.Name]))
	}

	timeout := time.Duration(pol.Spec.Rollout.RebootTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	res := plan.Plan(states, plan.Params{
		MaxUnavailable: int(pol.Spec.Rollout.MaxUnavailable),
		RebootTimeout:  timeout,
		Paused:         pol.Spec.Paused,
		Now:            r.Now(),
	})

	byName := map[string]*rpiv1.NodeBootloader{}
	for i := range mine {
		byName[mine[i].Name] = &mine[i]
	}
	var actErr error
	for _, a := range res.Actions {
		if err := r.execute(ctx, &pol, byName[a.Node], a); err != nil {
			log.FromContext(ctx).Error(err, "action failed", "action", a.Kind, "node", a.Node)
			r.Recorder.Eventf(&pol, corev1.EventTypeWarning, "ActionFailed", "%s %s: %v", a.Kind, a.Node, err)
			actErr = err
			break // never skip ahead of a failed step
		}
	}

	if err := r.updateStatus(ctx, &pol, res, len(mine)); err != nil {
		return ctrl.Result{}, err
	}
	if actErr != nil {
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}
	if res.UpToDate == len(mine) && res.Halted == "" {
		return ctrl.Result{RequeueAfter: requeueIdle}, nil
	}
	return ctrl.Result{RequeueAfter: requeueBusy}, nil
}

func (r *PolicyReconciler) isSelected(nodes []corev1.Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

func (r *PolicyReconciler) selectedNodes(ctx context.Context, pol *rpiv1.BootloaderPolicy) ([]corev1.Node, error) {
	sel, err := metav1.LabelSelectorAsSelector(&pol.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}
	var list corev1.NodeList
	if err := r.List(ctx, &list, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// syncNodeObjects makes a NodeBootloader per selected node carry this policy's
// target, and lets go of the ones whose node is no longer selected.
//
// Every Pi node already has a NodeBootloader (the agent creates it, Unmanaged),
// so for most nodes this adopts an existing object; it creates one only when the
// agent has not got there yet. A node is never deleted from the cluster's view
// because a policy lost interest in it: it goes back to Unmanaged.
func (r *PolicyReconciler) syncNodeObjects(ctx context.Context, pol *rpiv1.BootloaderPolicy, nodes []corev1.Node) error {
	want := map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		want[n.Name] = true
		var nb rpiv1.NodeBootloader
		err := r.Get(ctx, types.NamespacedName{Name: n.Name}, &nb)
		switch {
		case apierrors.IsNotFound(err):
			nb = rpiv1.NodeBootloader{
				ObjectMeta: metav1.ObjectMeta{Name: n.Name},
				Spec:       rpiv1.NodeBootloaderSpec{PolicyName: pol.Name, Target: pol.Spec.TargetSpec.DeepCopy()},
			}
			if err := controllerutil.SetOwnerReference(n, &nb, r.Scheme); err != nil {
				return err
			}
			// The agent may create it at the same moment; the next pass adopts it.
			if err := r.Create(ctx, &nb); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		case err != nil:
			return err
		case nb.Spec.PolicyName != "" && nb.Spec.PolicyName != pol.Name && r.policyExists(ctx, nb.Spec.PolicyName):
			r.Recorder.Eventf(pol, corev1.EventTypeWarning, "NodeConflict",
				"node %s is already managed by policy %s", n.Name, nb.Spec.PolicyName)
		case nb.Spec.PolicyName != pol.Name || !equality.Semantic.DeepEqual(nb.Spec.Target, &pol.Spec.TargetSpec):
			// Unmanaged (or left by a policy that no longer exists), or the policy
			// changed: take it over and point it at this target.
			orig := nb.DeepCopy()
			nb.Spec.PolicyName = pol.Name
			nb.Spec.Target = pol.Spec.TargetSpec.DeepCopy()
			if err := r.Patch(ctx, &nb, client.MergeFrom(orig)); err != nil {
				return err
			}
		}
	}

	var all rpiv1.NodeBootloaderList
	if err := r.List(ctx, &all); err != nil {
		return err
	}
	for i := range all.Items {
		nb := &all.Items[i]
		if nb.Spec.PolicyName != pol.Name || want[nb.Name] {
			continue
		}
		if err := r.unmanage(ctx, nb); err != nil {
			return err
		}
	}
	return nil
}

// policyExists reports whether a BootloaderPolicy of that name exists, so a node
// left claimed by a policy that was removed without its finalizer can be adopted.
func (r *PolicyReconciler) policyExists(ctx context.Context, name string) bool {
	var p rpiv1.BootloaderPolicy
	err := r.Get(ctx, types.NamespacedName{Name: name}, &p)
	return err == nil && p.DeletionTimestamp.IsZero()
}

// unmanage returns a node to service and to the Unmanaged state: no policy, no
// target, no approvals. The object stays, so the node stays visible.
func (r *PolicyReconciler) unmanage(ctx context.Context, nb *rpiv1.NodeBootloader) error {
	if err := r.release(ctx, nb); err != nil {
		return err
	}
	orig := nb.DeepCopy()
	nb.Spec = rpiv1.NodeBootloaderSpec{}
	delete(nb.Annotations, AnnotationCordoned)
	delete(nb.Annotations, AnnotationDrainedHash)
	return r.Patch(ctx, nb, client.MergeFrom(orig))
}

func nodeState(nb *rpiv1.NodeBootloader, ready bool) plan.Node {
	hash := nb.Spec.TargetHash()
	n := plan.Node{
		Name: nb.Name, Ready: ready,
		Hash: hash, StatusHash: nb.Status.TargetHash, Phase: nb.Status.Phase, Message: nb.Status.Message,
		Approved:         nb.Spec.ApprovedHash == hash,
		RebootAuthorized: nb.Spec.RebootHash == hash,
		Cordoned:         nb.Annotations[AnnotationCordoned] == "true",
		Drained:          nb.Annotations[AnnotationDrainedHash] == hash,
	}
	if nb.Spec.RebootAuthorizedAt != nil {
		n.RebootAuthorizedAt = nb.Spec.RebootAuthorizedAt.Time
	}
	return n
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// execute performs one planner action. Ordering inside each action is
// deliberate: cordon before approving, drain before authorising a reboot.
func (r *PolicyReconciler) execute(ctx context.Context, pol *rpiv1.BootloaderPolicy, nb *rpiv1.NodeBootloader, a plan.Action) error {
	hash := nb.Spec.TargetHash()
	orig := nb.DeepCopy()
	switch a.Kind {
	case plan.Approve:
		if err := r.Drainer.Cordon(ctx, nb.Name); err != nil {
			return fmt.Errorf("cordon: %w", err)
		}
		setAnnotation(nb, AnnotationCordoned, "true")
		delete(nb.Annotations, AnnotationDrainedHash)
		nb.Spec.ApprovedHash = hash
		nb.Spec.RebootHash, nb.Spec.RebootAuthorizedAt = "", nil
		r.Recorder.Eventf(pol, corev1.EventTypeNormal, "Approved", "approved bootloader update on %s", nb.Name)

	case plan.Drain:
		timeout := time.Duration(pol.Spec.Rollout.DrainTimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = 5 * time.Minute
		}
		if err := r.Drainer.Drain(ctx, nb.Name, timeout); err != nil {
			return err
		}
		setAnnotation(nb, AnnotationDrainedHash, hash)

	case plan.AuthorizeReboot:
		now := metav1.NewTime(r.Now())
		nb.Spec.RebootHash, nb.Spec.RebootAuthorizedAt = hash, &now
		r.Recorder.Eventf(pol, corev1.EventTypeNormal, "RebootAuthorized", "node %s drained; cleared to reboot", nb.Name)

	case plan.Uncordon, plan.Revoke:
		if err := r.release(ctx, nb); err != nil {
			return err
		}
		if a.Kind == plan.Revoke {
			nb.Spec.ApprovedHash, nb.Spec.RebootHash, nb.Spec.RebootAuthorizedAt = "", "", nil
		}
		delete(nb.Annotations, AnnotationCordoned)
		delete(nb.Annotations, AnnotationDrainedHash)
		if a.Kind == plan.Uncordon {
			r.Recorder.Eventf(pol, corev1.EventTypeNormal, "Updated", "node %s is up to date and back in service", nb.Name)
		}
	}
	return r.Patch(ctx, nb, client.MergeFrom(orig))
}

// release uncordons the node if (and only if) this controller cordoned it.
func (r *PolicyReconciler) release(ctx context.Context, nb *rpiv1.NodeBootloader) error {
	if nb.Annotations[AnnotationCordoned] != "true" {
		return nil
	}
	if err := r.Drainer.Uncordon(ctx, nb.Name); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("uncordon: %w", err)
	}
	return nil
}

func setAnnotation(nb *rpiv1.NodeBootloader, k, v string) {
	if nb.Annotations == nil {
		nb.Annotations = map[string]string{}
	}
	nb.Annotations[k] = v
}

func (r *PolicyReconciler) finalize(ctx context.Context, pol *rpiv1.BootloaderPolicy) error {
	if !controllerutil.ContainsFinalizer(pol, Finalizer) {
		return nil
	}
	var all rpiv1.NodeBootloaderList
	if err := r.List(ctx, &all); err != nil {
		return err
	}
	for i := range all.Items {
		nb := &all.Items[i]
		if nb.Spec.PolicyName != pol.Name {
			continue
		}
		// Back to Unmanaged: this uncordons what we cordoned and withdraws every
		// approval, so a staged update is not rebooted into later.
		if err := r.unmanage(ctx, nb); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(pol, Finalizer)
	return r.Update(ctx, pol)
}

func (r *PolicyReconciler) updateStatus(ctx context.Context, pol *rpiv1.BootloaderPolicy, res plan.Result, selected int) error {
	orig := pol.DeepCopy()
	st := &pol.Status
	st.ObservedGeneration = pol.Generation
	st.Selected = int32(selected)
	st.UpToDate, st.Pending, st.InProgress = int32(res.UpToDate), int32(res.Pending), int32(res.InProgress)
	st.Blocked, st.Failed = int32(res.Blocked), int32(res.Failed)

	halted := metav1.Condition{Type: rpiv1.ConditionRolloutHalted, ObservedGeneration: pol.Generation}
	if res.Halted != "" {
		halted.Status, halted.Reason, halted.Message = metav1.ConditionTrue, "NodeFailed", res.Halted
	} else {
		halted.Status, halted.Reason = metav1.ConditionFalse, "Running"
	}
	meta.SetStatusCondition(&st.Conditions, halted)

	complete := metav1.Condition{Type: rpiv1.ConditionComplete, ObservedGeneration: pol.Generation}
	if selected > 0 && res.UpToDate == selected {
		complete.Status, complete.Reason = metav1.ConditionTrue, "AllNodesUpToDate"
	} else {
		complete.Status, complete.Reason = metav1.ConditionFalse, "InProgress"
		complete.Message = fmt.Sprintf("%d of %d nodes up to date", res.UpToDate, selected)
	}
	meta.SetStatusCondition(&st.Conditions, complete)

	if equality.Semantic.DeepEqual(orig.Status, pol.Status) {
		return nil
	}
	return r.Status().Patch(ctx, pol, client.MergeFrom(orig))
}

// SetupWithManager wires the watches.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allPolicies := func(ctx context.Context) []reconcile.Request {
		var list rpiv1.BootloaderPolicyList
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, p := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
		return reqs
	}
	byPolicy := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		nb, ok := o.(*rpiv1.NodeBootloader)
		if !ok {
			return nil
		}
		if nb.Spec.PolicyName == "" {
			// A newly created, unmanaged node: any policy might select it.
			return allPolicies(ctx)
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nb.Spec.PolicyName}}}
	})
	nodeEvents := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return allPolicies(ctx)
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&rpiv1.BootloaderPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&rpiv1.NodeBootloader{}, byPolicy).
		Watches(&corev1.Node{}, nodeEvents, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Named("bootloaderpolicy").
		Complete(r)
}
