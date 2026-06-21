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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sysctlv1alpha1 "sysctl-operator/api/v1alpha1"
)

const (
	// configVolumeName is the name of the volume that carries the rendered
	// sysctl drop-in into the applier pods.
	configVolumeName = "sysctl-config"
	// configMountPath is where the ConfigMap is mounted inside each pod.
	configMountPath = "/etc/sysctl.d"
	// configFileName is the drop-in file name rendered into the ConfigMap.
	configFileName = "99-sysctl-operator.conf"
	// applierImage is the placeholder image used to run the applier pods.
	// TODO: replace with an image that actually applies the mounted sysctls.
	applierImage = "nginx:latest"
	// driftCheckerImage is the placeholder image for the drift-check CronJob.
	// TODO: replace with the real drift-checker image (passed in later).
	driftCheckerImage = "busybox:latest"
	// reportURL is where drift-check pods POST their findings. It assumes a
	// Service named "sysctl-operator-report" fronts the operator on port 9090.
	// TODO: make this configurable / inject the operator namespace.
	reportURL = "http://sysctl-operator-report:9090/api/v1/reports"

	labelProfile  = "sysctl.k8s.io/profile"
	configHashAnn = "sysctl.k8s.io/config-hash"

	// finalizer guards cleanup of the resources a profile owns.
	finalizer = "sysctl.k8s.io/finalizer"
)

// SysctlProfileReconciler reconciles a SysctlProfile object
type SysctlProfileReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=sysctlprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=sysctlprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sysctl.k8s.io,resources=sysctlprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile renders the profile's sysctls into a ConfigMap and ensures a
// DaemonSet (pinned to the profile's nodeSelector) mounts that ConfigMap into
// the applier pods.
func (r *SysctlProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var profile sysctlv1alpha1.SysctlProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		// Ignore not-found: the owned ConfigMap/DaemonSet are garbage-collected
		// via their owner reference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: run cleanup, then drop the finalizer.
	if !profile.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&profile, finalizer) {
			if err := r.cleanup(ctx, &profile); err != nil {
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

	content := renderSysctlConf(&profile)
	hash := hashContent(content)
	name := resourceName(&profile)
	labels := labelsFor(&profile)

	// 1. Reconcile the ConfigMap holding the rendered sysctl drop-in.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace},
	}
	cmOp, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{configFileName: content}
		return controllerutil.SetControllerReference(&profile, cm, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile ConfigMap", "name", name)
		return ctrl.Result{}, err
	}
	log.Info("reconciled ConfigMap", "name", name, "operation", cmOp)

	// 2. Reconcile the drift-check CronJob (independent of rollout/teardown):
	// it periodically inspects live values and reports to the operator API.
	if profile.Spec.Schedule.DriftCheck != "" {
		if err := r.ensureCronJob(ctx, &profile, hash, labels); err != nil {
			log.Error(err, "failed to reconcile drift-check CronJob")
			return ctrl.Result{}, err
		}
	} else if err := r.ensureNoCronJob(ctx, &profile); err != nil {
		log.Error(err, "failed to remove drift-check CronJob")
		return ctrl.Result{}, err
	}

	// 3. Determine the set of nodes this profile targets.
	selected, err := r.selectedNodeNames(ctx, &profile)
	if err != nil {
		log.Error(err, "failed to list selected nodes")
		return ctrl.Result{}, err
	}

	// 4. Roll out vs. tear down. Once every selected node has reported a
	// successful apply at the current hash, the applier DaemonSet has done its
	// job and is retired. teardownHash prevents it from being recreated until
	// the config changes.
	rolloutComplete := profile.Status.TeardownHash == hash ||
		allNodesApplied(&profile, selected, hash)

	switch {
	case rolloutComplete:
		if err := r.deleteDaemonSet(ctx, &profile); err != nil {
			log.Error(err, "failed to tear down DaemonSet", "name", name)
			return ctrl.Result{}, err
		}
		if profile.Status.TeardownHash != hash {
			log.Info("rollout complete, tore down applier DaemonSet",
				"name", name, "nodes", len(selected))
		}
		profile.Status.TeardownHash = hash
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "RolloutComplete",
			Message:            "all selected nodes applied; applier DaemonSet torn down",
			ObservedGeneration: profile.Generation,
		})
	default:
		if err := r.ensureDaemonSet(ctx, &profile, name, hash, labels); err != nil {
			log.Error(err, "failed to reconcile DaemonSet", "name", name)
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "RollingOut",
			Message:            "applier DaemonSet active",
			ObservedGeneration: profile.Generation,
		})
	}

	// 5. Update status with the observed config hash and desired node count.
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

// ensureDaemonSet creates or updates the applier DaemonSet for the profile.
func (r *SysctlProfileReconciler) ensureDaemonSet(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile, name, hash string, labels map[string]string) error {
	log := logf.FromContext(ctx)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		ds.Labels = labels

		// Selector is immutable after creation; keep it minimal and stable.
		ds.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{labelProfile: profile.Name},
		}

		ds.Spec.Template.Labels = labels
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}
		// Bumping this annotation when the rendered config changes triggers a
		// rolling update of the applier pods.
		ds.Spec.Template.Annotations[configHashAnn] = hash

		// DaemonSet node placement uses matchLabels from the profile selector.
		ds.Spec.Template.Spec.NodeSelector = profile.Spec.NodeSelector.MatchLabels

		ds.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			},
		}}
		ds.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "applier",
			Image: applierImage,
			VolumeMounts: []corev1.VolumeMount{{
				Name:      configVolumeName,
				MountPath: configMountPath,
				ReadOnly:  true,
			}},
		}}
		return controllerutil.SetControllerReference(profile, ds, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.Info("reconciled DaemonSet", "name", name, "operation", op)
	return nil
}

// deleteDaemonSet removes the applier DaemonSet (and its pods) for the profile.
func (r *SysctlProfileReconciler) deleteDaemonSet(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile) error {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName(profile), Namespace: profile.Namespace},
	}
	return client.IgnoreNotFound(r.Delete(ctx, ds))
}

// ensureCronJob creates or updates the periodic drift-check CronJob. Its pods
// inspect live sysctl values and POST findings to the operator's report API.
func (r *SysctlProfileReconciler) ensureCronJob(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile, hash string, labels map[string]string) error {
	log := logf.FromContext(ctx)
	name := cronJobName(profile)

	historyLimit := int32(3)
	failedLimit := int32(1)

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Labels = labels
		cj.Spec.Schedule = profile.Spec.Schedule.DriftCheck
		// Don't stack drift checks if one runs long.
		cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cj.Spec.SuccessfulJobsHistoryLimit = &historyLimit
		cj.Spec.FailedJobsHistoryLimit = &failedLimit

		jobSpec := &cj.Spec.JobTemplate.Spec
		pod := &jobSpec.Template.Spec
		jobSpec.Template.Labels = labels
		pod.RestartPolicy = corev1.RestartPolicyOnFailure
		pod.NodeSelector = profile.Spec.NodeSelector.MatchLabels

		pod.Volumes = []corev1.Volume{{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: resourceName(profile)},
				},
			},
		}}
		pod.Containers = []corev1.Container{{
			Name:  "drift-checker",
			Image: driftCheckerImage,
			Env: []corev1.EnvVar{
				{Name: "PROFILE", Value: profile.Name},
				{Name: "NAMESPACE", Value: profile.Namespace},
				{Name: "CONFIG_HASH", Value: hash},
				{Name: "REPORT_URL", Value: reportURL},
				{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
				}},
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      configVolumeName,
				MountPath: configMountPath,
				ReadOnly:  true,
			}},
		}}
		return controllerutil.SetControllerReference(profile, cj, r.Scheme)
	})
	if err != nil {
		return err
	}
	log.Info("reconciled drift-check CronJob", "name", name, "operation", op)
	return nil
}

// ensureNoCronJob removes the drift-check CronJob when no schedule is set.
func (r *SysctlProfileReconciler) ensureNoCronJob(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile) error {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: cronJobName(profile), Namespace: profile.Namespace},
	}
	return client.IgnoreNotFound(r.Delete(ctx, cj))
}

// selectedNodeNames returns the names of nodes matching the profile selector.
func (r *SysctlProfileReconciler) selectedNodeNames(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile) ([]string, error) {
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

// allNodesApplied reports whether every selected node has a successful apply at
// the given hash. Returns false when no nodes are selected so the DaemonSet is
// kept (and simply schedules nothing) rather than being torn down prematurely.
func allNodesApplied(profile *sysctlv1alpha1.SysctlProfile, selected []string, hash string) bool {
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

// cleanup removes the resources a profile owns. Owner references already enable
// garbage collection, but deleting explicitly lets us control ordering (drain
// the applier DaemonSet before its ConfigMap) and is where restoreOnDelete
// behavior will hook in later.
func (r *SysctlProfileReconciler) cleanup(ctx context.Context, profile *sysctlv1alpha1.SysctlProfile) error {
	log := logf.FromContext(ctx)
	name := resourceName(profile)

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace}}
	if err := r.Delete(ctx, ds); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting DaemonSet %s: %w", name, err)
	}

	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: cronJobName(profile), Namespace: profile.Namespace}}
	if err := r.Delete(ctx, cj); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting CronJob %s: %w", cronJobName(profile), err)
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.Namespace}}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting ConfigMap %s: %w", name, err)
	}

	// TODO(restoreOnDelete): when profile.Spec.RestoreOnDelete is true, launch a
	// one-shot job to remove the persistent drop-in file from the nodes.
	log.Info("cleaned up profile-owned resources", "name", name)
	return nil
}

// renderSysctlConf renders the profile's sysctls into a sysctl.d drop-in file.
func renderSysctlConf(p *sysctlv1alpha1.SysctlProfile) string {
	var b strings.Builder
	for _, s := range p.Spec.Sysctls {
		fmt.Fprintf(&b, "%s = %s\n", s.Name, s.Value)
	}
	return b.String()
}

// hashContent returns a short, stable hash of the rendered config.
func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// resourceName is the shared name for the ConfigMap and DaemonSet of a profile.
func resourceName(p *sysctlv1alpha1.SysctlProfile) string {
	return p.Name + "-sysctl"
}

// cronJobName is the name of the drift-check CronJob for a profile.
func cronJobName(p *sysctlv1alpha1.SysctlProfile) string {
	return p.Name + "-drift"
}

// labelsFor returns the common labels applied to managed resources.
func labelsFor(p *sysctlv1alpha1.SysctlProfile) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "sysctl-operator",
		"app.kubernetes.io/managed-by": "sysctl-operator",
		labelProfile:                   p.Name,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SysctlProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sysctlv1alpha1.SysctlProfile{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&batchv1.CronJob{}).
		Named("sysctlprofile").
		Complete(r)
}
