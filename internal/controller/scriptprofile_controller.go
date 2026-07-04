/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sysctlv1alpha1 "sysctl-operator/api/v1alpha1"
)

const (
	// scriptConfigMountPath is where the rendered scripts file is mounted in
	// each runner pod.
	scriptConfigMountPath = "/etc/sysctl-operator/scripts"
	// scriptConfigFileName is the single rendered file the runner parses.
	scriptConfigFileName = "scripts.txt"
	// scriptReportURL is where script-runner pods POST their findings. As with
	// the sysctl workers it assumes a Service fronting the operator on 9090.
	// TODO: make this configurable / inject the operator namespace.
	scriptReportURL = "http://sysctl-operator-report.sysctl-operator-system:9090/api/v1/reports"
	// labelScriptProfile tags resources owned by a ScriptProfile so its
	// DaemonSet selector never collides with a SysctlProfile of the same name.
	labelScriptProfile = "sysctl.k8s.io/script-profile"
)

// ScriptProfileReconciler reconciles a ScriptProfile object.
type ScriptProfileReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=scriptprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=scriptprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=scriptprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile renders the profile's scripts into a ConfigMap and ensures a
// DaemonSet (pinned to the profile's nodeSelector) runs them on each node,
// tearing the DaemonSet down once every selected node has reported success.
func (r *ScriptProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var profile sysctlv1alpha1.ScriptProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		// Ignore not-found: owned ConfigMap/DaemonSet are garbage-collected via
		// their owner reference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: run cleanup, then drop the finalizer.
	if !profile.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&profile, finalizer) {
			if err := r.cleanupScript(ctx, &profile); err != nil {
				log.Error(err, "cleanup failed")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&profile, finalizer)
			if err := r.Update(ctx, &profile); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before creating any owned resources.
	if !controllerutil.ContainsFinalizer(&profile, finalizer) {
		controllerutil.AddFinalizer(&profile, finalizer)
		if err := r.Update(ctx, &profile); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-triggers reconcile; continue on the next pass.
		return ctrl.Result{}, nil
	}

	// Snapshot status so we only write it back when something actually changed
	// (a no-op status write would re-trigger reconcile and hot-loop).
	originalStatus := profile.Status.DeepCopy()

	content := renderScripts(&profile)
	hash := hashContent(content)
	name := scriptResourceName(&profile)
	labels := scriptLabelsFor(&profile)

	// 1. Reconcile the ConfigMap holding the rendered scripts.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace},
	}
	cmOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{scriptConfigFileName: content}
		return controllerutil.SetControllerReference(&profile, cm, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile ConfigMap", "name", name)
		return ctrl.Result{}, err
	}
	log.Info("reconciled ConfigMap", "name", name, "operation", cmOp)

	// 2. Determine the set of nodes this profile targets.
	selected, err := r.scriptSelectedNodeNames(ctx, &profile)
	if err != nil {
		log.Error(err, "failed to list selected nodes")
		return ctrl.Result{}, err
	}

	// 3. Roll out vs. tear down. The DaemonSet exists exactly while some selected
	// node still hasn't run the scripts at the current hash. Evaluating this
	// against the live selected set (rather than short-circuiting on TeardownHash)
	// is what lets a newly-joined node re-trigger a rollout: it isn't Applied yet,
	// so the DaemonSet is recreated, runs on the new node, and is torn down again
	// once every node has reported success. teardownHash is kept for observability.
	rolloutComplete := scriptAllNodesApplied(&profile, selected, hash)

	switch {
	case rolloutComplete:
		if err := r.deleteScriptDaemonSet(ctx, &profile); err != nil {
			log.Error(err, "failed to tear down DaemonSet", "name", name)
			return ctrl.Result{}, err
		}
		if profile.Status.TeardownHash != hash {
			log.Info("rollout complete, tore down runner DaemonSet",
				"name", name, "nodes", len(selected))
		}
		profile.Status.TeardownHash = hash
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "RolloutComplete",
			Message:            "all selected nodes ran the scripts; runner DaemonSet torn down",
			ObservedGeneration: profile.Generation,
		})
	default:
		if err := r.ensureScriptDaemonSet(ctx, &profile, name, hash, labels); err != nil {
			log.Error(err, "failed to reconcile DaemonSet", "name", name)
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "RollingOut",
			Message:            "runner DaemonSet active",
			ObservedGeneration: profile.Generation,
		})
	}

	// 4. Update status with the observed config hash and desired node count.
	// AppliedNodes/FailedNodes are maintained by the report API as pods report.
	profile.Status.ObservedHash = hash
	profile.Status.DesiredNodes = int32(len(selected))
	if !apiequality.Semantic.DeepEqual(originalStatus, &profile.Status) {
		if err := r.Status().Update(ctx, &profile); err != nil {
			log.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ensureScriptDaemonSet creates or updates the runner DaemonSet for the profile.
func (r *ScriptProfileReconciler) ensureScriptDaemonSet(ctx context.Context, profile *sysctlv1alpha1.ScriptProfile, name, hash string, labels map[string]string) error {
	log := logf.FromContext(ctx)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		ds.Labels = labels

		// Selector is immutable after creation; keep it minimal and stable.
		ds.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{labelScriptProfile: profile.Name},
		}

		ds.Spec.Template.Labels = labels
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}
		// Bumping this annotation when the rendered config changes triggers a
		// rolling update of the runner pods.
		ds.Spec.Template.Annotations[configHashAnn] = hash

		ds.Spec.Template.Spec.NodeSelector = profile.Spec.NodeSelector.MatchLabels

		// Runner pods act on the host: share the host network/PID/IPC namespaces
		// so scripts observe and affect the node, not the pod's namespaces.
		ds.Spec.Template.Spec.HostNetwork = true
		ds.Spec.Template.Spec.HostPID = true
		ds.Spec.Template.Spec.HostIPC = true
		ds.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet

		ds.Spec.Template.Spec.Volumes = []corev1.Volume{
			{
				Name: configVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: name},
					},
				},
			},
			{
				Name: hostSysVolumeName,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: hostSysPath},
				},
			},
		}
		ds.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:            "runner",
			Image:           applierImage,
			ImagePullPolicy: imagePullPolicy,
			SecurityContext: privilegedSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      configVolumeName,
					MountPath: scriptConfigMountPath,
					ReadOnly:  true,
				},
			},
			Env: []corev1.EnvVar{
				{Name: "MODE", Value: "script"},
				{Name: "CONFIG_PATH", Value: scriptConfigMountPath + "/" + scriptConfigFileName},
				{Name: "PROFILE", Value: profile.Name},
				{Name: "NAMESPACE", Value: profile.Namespace},
				{Name: "CONFIG_HASH", Value: hash},
				{Name: "REPORT_URL", Value: scriptReportURL},
				{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
				}},
			},
		}}
		return controllerutil.SetControllerReference(profile, ds, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.Info("reconciled DaemonSet", "name", name, "operation", op)
	return nil
}

// deleteScriptDaemonSet removes the runner DaemonSet (and its pods).
func (r *ScriptProfileReconciler) deleteScriptDaemonSet(ctx context.Context, profile *sysctlv1alpha1.ScriptProfile) error {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: scriptResourceName(profile), Namespace: profile.Namespace},
	}
	return client.IgnoreNotFound(r.Delete(ctx, ds))
}

// scriptSelectedNodeNames returns the names of nodes matching the profile selector.
func (r *ScriptProfileReconciler) scriptSelectedNodeNames(ctx context.Context, profile *sysctlv1alpha1.ScriptProfile) ([]string, error) {
	sel, err := metav1.LabelSelectorAsSelector(&profile.Spec.NodeSelector)
	if err != nil {
		return nil, err
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		names = append(names, nodes.Items[i].Name)
	}
	return names, nil
}

// scriptAllNodesApplied reports whether every selected node ran the scripts at
// the given hash. Returns false when no nodes are selected so the DaemonSet is
// kept rather than torn down prematurely.
func scriptAllNodesApplied(profile *sysctlv1alpha1.ScriptProfile, selected []string, hash string) bool {
	if len(selected) == 0 {
		return false
	}
	applied := make(map[string]bool, len(profile.Status.NodeStatuses))
	for _, ns := range profile.Status.NodeStatuses {
		if ns.Phase == sysctlv1alpha1.NodePhaseApplied && ns.AppliedHash == hash {
			applied[ns.NodeName] = true
		}
	}
	for _, n := range selected {
		if !applied[n] {
			return false
		}
	}
	return true
}

// cleanupScript removes the resources a profile owns.
func (r *ScriptProfileReconciler) cleanupScript(ctx context.Context, profile *sysctlv1alpha1.ScriptProfile) error {
	log := logf.FromContext(ctx)
	name := scriptResourceName(profile)

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace}}
	if err := r.Delete(ctx, ds); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting DaemonSet %s: %w", name, err)
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace}}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting ConfigMap %s: %w", name, err)
	}

	log.Info("cleaned up profile-owned resources", "name", name)
	return nil
}

// renderScripts renders the profile's scripts into a single parseable file.
// Each script is emitted as a block the worker can split back apart:
//
//	#!script name=<name> interpreter=<interpreter>
//	<content>
//	#!endscript
//
// Keeping the whole set in one file (hashed as raw bytes) mirrors the sysctl
// drop-in model, so the worker verifies it received the exact config intended.
func renderScripts(p *sysctlv1alpha1.ScriptProfile) string {
	var b strings.Builder
	for _, s := range p.Spec.Scripts {
		interp := s.Interpreter
		if interp == "" {
			interp = "/bin/sh"
		}
		fmt.Fprintf(&b, "#!script name=%s interpreter=%s\n", s.Name, interp)
		b.WriteString(s.Content)
		if !strings.HasSuffix(s.Content, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("#!endscript\n")
	}
	return b.String()
}

// scriptResourceName is the shared name for the ConfigMap and DaemonSet.
func scriptResourceName(p *sysctlv1alpha1.ScriptProfile) string {
	return p.Name + "-script"
}

// scriptLabelsFor returns the common labels applied to managed resources.
func scriptLabelsFor(p *sysctlv1alpha1.ScriptProfile) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "sysctl-operator",
		"app.kubernetes.io/managed-by": "sysctl-operator",
		labelScriptProfile:             p.Name,
	}
}

// profilesForNode maps a Node event to reconcile requests for every
// ScriptProfile whose nodeSelector matches that node. This is what makes a
// newly-joined (or newly-relabeled) node re-trigger a rollout even after the
// runner DaemonSet was torn down.
func (r *ScriptProfileReconciler) profilesForNode(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	var list sysctlv1alpha1.ScriptProfileList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.NodeSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(node.Labels)) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScriptProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sysctlv1alpha1.ScriptProfile{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.profilesForNode)).
		Named("scriptprofile").
		Complete(r)
}
