// Copyright 2019 The Cloud Robotics Authors

//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chartassignment

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	apps "github.com/googlecloudrobotics/core/src/go/pkg/apis/apps/v1alpha1"
	"github.com/googlecloudrobotics/core/src/go/pkg/gcr"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validation"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// Allow the Service Account Controller some time to create the default
	// SA in a new namespace.
	defaultServiceAccountDeadline = time.Minute

	fieldIndexNamespace = "spec.namespaceName"
)

// Add adds a controller and validation webhook for the ChartAssignment resource type
// to the manager and server.
// Handled ChartAssignments are filtered by the provided cluster.
func Add(mgr manager.Manager, cluster string) error {
	r := &Reconciler{
		kube:     mgr.GetClient(),
		recorder: mgr.GetEventRecorderFor("chartassignment-controller"),
		cluster:  cluster,
	}
	var err error
	r.releases, err = newReleases(mgr.GetConfig(), r.recorder)
	if err != nil {
		return err
	}

	c, err := controller.New("chartassignment", mgr, controller.Options{
		Reconciler: r,
	})
	if err != nil {
		return err
	}
	err = mgr.GetCache().IndexField(&apps.ChartAssignment{}, fieldIndexNamespace,
		func(o runtime.Object) []string {
			return []string{o.(*apps.ChartAssignment).Spec.NamespaceName}
		},
	)
	if err != nil {
		return errors.Wrap(err, "add field indexer")
	}
	err = c.Watch(
		&source.Kind{Type: &apps.ChartAssignment{}},
		&handler.EnqueueRequestForObject{},
	)
	if err != nil {
		return err
	}
	err = c.Watch(
		&source.Kind{Type: &core.Pod{}},
		&handler.Funcs{
			CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
				r.enqueueForPod(e.Meta, q)
			},
			UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
				r.enqueueForPod(e.MetaNew, q)
			},
			DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
				r.enqueueForPod(e.Meta, q)
			},
		},
	)
	if err != nil {
		return errors.Wrap(err, "watch Apps")
	}
	return nil
}

func (r *Reconciler) enqueueForPod(m meta.Object, q workqueue.RateLimitingInterface) {
	var cas apps.ChartAssignmentList
	err := r.kube.List(context.TODO(), &cas, kclient.MatchingField(fieldIndexNamespace, m.GetNamespace()))
	if err != nil {
		log.Printf("List ChartAssignments for namespace %s failed: %s", m.GetNamespace(), err)
		return
	}
	for _, ca := range cas.Items {
		q.Add(reconcile.Request{
			NamespacedName: kclient.ObjectKey{Name: ca.Name},
		})
	}
}

// Reconciler provides an idempotent function that brings the cluster into a
// state consistent with the specification of a ChartAssignment.
type Reconciler struct {
	kube     kclient.Client
	recorder record.EventRecorder
	cluster  string // Cluster for which to handle ChartAssignments.
	releases *releases
}

// Reconcile creates and updates a Synk ResourceSet for the given chart
// assignment. It rolls back releases to the previous revision if an upgrade
// failed. It continuously requeues the ChartAssignment for reconciliation to
// monitor the status of the ResourceSet.
func (r *Reconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	ctx := context.TODO()

	var as apps.ChartAssignment
	err := r.kube.Get(ctx, req.NamespacedName, &as)

	if as.Spec.ClusterName != r.cluster {
		return reconcile.Result{}, nil
	}
	if k8serrors.IsNotFound(err) {
		// Assignment was already deleted. We did all required cleanup
		// when removing the finalizer. Thus, there's nothing to do.
		log.Printf("ChartAssignment %q no longer exists, skipping reconciliation...", req.NamespacedName)
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting ChartAssignment %q failed: %s", req, err)
	}
	return r.reconcile(ctx, &as)
}

const (
	// The finalizer that's applied to assignments to block their garbage collection
	// until the Synk ResourceSet is deleted.
	finalizer = "helm.apps.cloudrobotics.com"
	// Requeue interval when the underlying Synk ResourceSet is not in a stable state yet.
	requeueFast = 3 * time.Second
	// Requeue interval after the underlying Synk ResourceSete reached a stable state.
	requeueSlow = 3 * time.Minute
)

// namespaceDeletionError indicates that a namespace could not be created
// because a previously-created namespace with the same name is pending
// deletion. This occurs when you delete and recreate a chartassignment. It is
// transient, but may last seconds or minutes if the namespace contains
// resources that are slow to delete.
type namespaceDeletionError struct {
	msg string
}

func (e *namespaceDeletionError) Error() string { return e.msg }

// missingServiceAccountError indicates that the default ServiceAccount has not
// yet been created, and that the chart should not be updated to avoid creating
// pods before the ImagePullSecrets have been applied.
type missingServiceAccountError struct {
	msg string
}

func (e *missingServiceAccountError) Error() string { return e.msg }

func (r *Reconciler) ensureNamespace(ctx context.Context, as *apps.ChartAssignment) (*core.Namespace, error) {
	// Create application namespace if it doesn't exist.
	var ns core.Namespace
	err := r.kube.Get(ctx, kclient.ObjectKey{Name: as.Spec.NamespaceName}, &ns)

	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting Namespace %q failed: %s", as.Spec.NamespaceName, err)
	}
	if ns.DeletionTimestamp != nil {
		return nil, &namespaceDeletionError{
			msg: fmt.Sprintf("namespace %q was marked for deletion at %s, skipping", as.Spec.NamespaceName, ns.DeletionTimestamp),
		}
	}

	createNamespace := k8serrors.IsNotFound(err)
	ns.Name = as.Spec.NamespaceName
	ns.Labels = map[string]string{"app": as.Name}

	// Add ourselves to the owners if we aren't already.
	_true := true
	added := setOwnerReference(&ns.ObjectMeta, meta.OwnerReference{
		APIVersion:         as.APIVersion,
		Kind:               as.Kind,
		Name:               as.Name,
		UID:                as.UID,
		BlockOwnerDeletion: &_true,
	})
	if !added {
		return &ns, nil
	}
	if createNamespace {
		return &ns, r.kube.Create(ctx, &ns)
	}
	return &ns, r.kube.Update(ctx, &ns)
}

// ensureServiceAccount makes sure we have an image pull secret for gcr.io inside the apps namespace
// and the default service account configured to use it. This is needed to make apps work that
// reference images from a private container registry.
// TODO(ensonic): Put this behind a flag to only do this as needed.
func (r *Reconciler) ensureServiceAccount(ctx context.Context, ns *core.Namespace, as *apps.ChartAssignment) error {
	if r.cluster == "cloud" {
		// We don't need any of this for cloud charts.
		return nil
	}

	// Copy imagePullSecret from 'default' namespace, since service accounts cannot reference
	// secrets in other namespaces.
	var secret core.Secret
	err := r.kube.Get(ctx, kclient.ObjectKey{Namespace: as.Spec.NamespaceName, Name: gcr.SecretName}, &secret)
	if k8serrors.IsNotFound(err) {
		err = r.kube.Get(ctx, kclient.ObjectKey{Namespace: "default", Name: gcr.SecretName}, &secret)
		if k8serrors.IsNotFound(err) {
			log.Printf("Failed to get Secret \"default:%s\" (this is expected when simulating a robot on GKE)", gcr.SecretName)
			return nil
		} else if err != nil {
			return fmt.Errorf("getting Secret \"default:%s\" failed: %s", gcr.SecretName, err)
		}
		// Don't reuse full metadata in created secret.
		secret.ObjectMeta = meta.ObjectMeta{
			Namespace: ns.Name,
			Name:      gcr.SecretName,
		}
		err = r.kube.Create(ctx, &secret)
		if err != nil {
			return fmt.Errorf("creating Secret \"%s:%s\" failed: %s", as.Spec.NamespaceName, gcr.SecretName, err)
		}
	}

	// Configure the default service account in the namespace.
	var sa core.ServiceAccount
	err = r.kube.Get(ctx, kclient.ObjectKey{Namespace: as.Spec.NamespaceName, Name: "default"}, &sa)
	if err != nil {
		if k8serrors.IsNotFound(err) && time.Since(ns.CreationTimestamp.Time) < defaultServiceAccountDeadline {
			// The Service Account Controller hasn't created the default SA yet.
			return &missingServiceAccountError{
				msg: fmt.Sprintf("ServiceAccount \"%s:default\" not yet created", ns.Name),
			}
		}
		return fmt.Errorf("getting ServiceAccount \"%s:default\" failed: %s", as.Spec.NamespaceName, err)
	}

	// Only add the secret once.
	ips := core.LocalObjectReference{Name: gcr.SecretName}
	found := false
	for _, s := range sa.ImagePullSecrets {
		if s == ips {
			found = true
			break
		}
	}
	if !found {
		sa.ImagePullSecrets = append(sa.ImagePullSecrets, ips)
	}
	return r.kube.Update(ctx, &sa)
}

func (r *Reconciler) reconcile(ctx context.Context, as *apps.ChartAssignment) (reconcile.Result, error) {
	// If we are scheduled for deletion, delete the Synk ResourceSet and drop our
	// finalizer so garbage collection can continue.
	if as.DeletionTimestamp != nil {
		log.Printf("Ensure ChartAssignment %q cleanup", as.Name)

		if err := r.ensureDeleted(ctx, as); err != nil {
			return reconcile.Result{}, fmt.Errorf("ensure deleted: %s", err)
		}
		if err := r.setStatus(ctx, as); err != nil {
			return reconcile.Result{}, fmt.Errorf("set status: %s", err)
		}
		// Requeue to track deletion progress.
		return reconcile.Result{Requeue: true, RequeueAfter: requeueFast}, nil
	}

	ns, err := r.ensureNamespace(ctx, as)
	if err != nil {
		if _, ok := err.(*namespaceDeletionError); ok {
			log.Printf("ensure namespace: %s", err)
			// Requeue to track deletion progress.
			return reconcile.Result{Requeue: true, RequeueAfter: requeueFast}, nil
		}
		return reconcile.Result{}, fmt.Errorf("ensure namespace: %s", err)
	}
	if err := r.ensureServiceAccount(ctx, ns, as); err != nil {
		if _, ok := err.(*missingServiceAccountError); ok {
			log.Printf("Failed: %q. This is expected to occur rarely.", err)
			return reconcile.Result{Requeue: true, RequeueAfter: requeueFast}, nil
		} else {
			return reconcile.Result{}, fmt.Errorf("ensure service-account: %s", err)
		}
	}
	// Ensure a finalizer on the ChartAssignment so we don't get deleted before
	// we've properly deleted the associated Synk ResourceSet.
	if !stringsContain(as.Finalizers, finalizer) {
		as.Finalizers = append(as.Finalizers, finalizer)
		if err := r.kube.Update(ctx, as); err != nil {
			return reconcile.Result{}, errors.Wrap(err, "add finalizer")
		}
	}

	r.releases.ensureUpdated(as)

	if err := r.setStatus(ctx, as); err != nil {
		if k8serrors.IsConflict(err) {
			// The cache has an old status. This can be ignored, as
			// controller-runtime will reconcile again when the cache updates:
			// https://github.com/kubernetes-sigs/controller-runtime/issues/377
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrap(err, "update status")
	}
	// Quickly requeue for status updates when deployment is in progress.
	switch as.Status.Phase {
	case apps.ChartAssignmentPhaseReady, apps.ChartAssignmentPhaseFailed:
		return reconcile.Result{Requeue: true, RequeueAfter: requeueSlow}, nil
	}
	return reconcile.Result{Requeue: true, RequeueAfter: requeueFast}, nil

}

func condition(b bool) core.ConditionStatus {
	if b {
		return core.ConditionTrue
	}
	return core.ConditionFalse
}

func (r *Reconciler) setStatus(ctx context.Context, as *apps.ChartAssignment) error {
	status, ok := r.releases.status(as.Name)
	if !ok {
		return nil
	} else if status.phase == apps.ChartAssignmentPhaseDeleted {
		// The assignment may have been garbage collected already, so
		// don't try to update the status.
		return nil
	}

	as.Status.ObservedGeneration = as.Generation
	as.Status.Phase = status.phase

	if c := condition(status.phase == apps.ChartAssignmentPhaseSettled); status.err == nil {
		setCondition(as, apps.ChartAssignmentConditionSettled, c, "")
	} else {
		setCondition(as, apps.ChartAssignmentConditionSettled, c, status.err.Error())
	}

	var ns core.Namespace
	if err := r.kube.Get(ctx, kclient.ObjectKey{Name: as.Spec.NamespaceName}, &ns); err != nil {
		if k8serrors.IsNotFound(err) {
			setCondition(as, apps.ChartAssignmentConditionReady, condition(false),
				"waiting for namespace creation")
		} else {
			return errors.Wrap(err, "get namespace")
		}
	} else {
		// Determine readiness based on pods in the app namespace being ready.
		// This is an incomplete heuristic but it should catch the vast majority of errors.
		var pods core.PodList
		// Note, this return 0 is the namespace has not been created!
		if err := r.kube.List(ctx, &pods, kclient.InNamespace(as.Spec.NamespaceName)); err != nil {
			return errors.Wrap(err, "list pods")
		}
		ready, total := 0, len(pods.Items)

		for _, p := range pods.Items {
			switch p.Status.Phase {
			case core.PodRunning, core.PodSucceeded:
				ready++
			}
		}
		// Readiness is only given if the release is settled to begin with.
		if status.phase != apps.ChartAssignmentPhaseSettled {
			setCondition(as, apps.ChartAssignmentConditionReady, core.ConditionFalse,
				"Release not settled yet")
		} else {
			if ready == total {
				as.Status.Phase = apps.ChartAssignmentPhaseReady
			}
			setCondition(as, apps.ChartAssignmentConditionReady, condition(ready == total),
				fmt.Sprintf("%d/%d pods are running or succeeded", ready, total))
		}
	}
	return r.kube.Status().Update(ctx, as)
}

// ensureDeleted ensures that the Synk ResourceSet is deleted and the finalizer gets removed.
func (r *Reconciler) ensureDeleted(ctx context.Context, as *apps.ChartAssignment) error {
	r.releases.ensureDeleted(as)
	status, ok := r.releases.status(as.Name)
	if !ok {
		return fmt.Errorf("release status not found")
	}

	if status.phase != apps.ChartAssignmentPhaseDeleted {
		// Deletion still in progress, check again later.
		return nil
	}
	if !stringsContain(as.Finalizers, finalizer) {
		return nil
	}
	as.Finalizers = stringsDelete(as.Finalizers, finalizer)
	if err := r.kube.Update(ctx, as); err != nil {
		return fmt.Errorf("update failed: %s", err)
	}
	return nil
}

func stringsContain(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stringsDelete(list []string, s string) (res []string) {
	for _, x := range list {
		if x != s {
			res = append(res, x)
		}
	}
	return res
}

// setOwnerReference ensures the owner reference is set and returns true if it did
// not exist before. Existing references are detected based on the UID field.
func setOwnerReference(om *meta.ObjectMeta, ref meta.OwnerReference) bool {
	for i, or := range om.OwnerReferences {
		if ref.UID == or.UID {
			om.OwnerReferences[i] = ref
			return false
		}
	}
	om.OwnerReferences = append(om.OwnerReferences, ref)
	return true
}

// inCondition returns true if the ChartAssignment has a condition of the given
// type in state true.
func inCondition(as *apps.ChartAssignment, c apps.ChartAssignmentConditionType) bool {
	for _, cond := range as.Status.Conditions {
		if cond.Type == c && cond.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

// setCondition adds or updates a condition. Existing conditions are detected
// based on the Type field.
func setCondition(as *apps.ChartAssignment, t apps.ChartAssignmentConditionType, v core.ConditionStatus, msg string) {
	now := meta.Now()

	for i, c := range as.Status.Conditions {
		if c.Type != t {
			continue
		}
		// Update existing condition.
		if c.Status != v || c.Message != msg {
			c.LastUpdateTime = now
		}
		if c.Status != v {
			c.LastTransitionTime = now
		}
		c.Message = msg
		c.Status = v
		as.Status.Conditions[i] = c
		return
	}
	// Condition set for the first time.
	as.Status.Conditions = append(as.Status.Conditions, apps.ChartAssignmentCondition{
		Type:               t,
		LastUpdateTime:     now,
		LastTransitionTime: now,
		Status:             v,
		Message:            msg,
	})
}

// NewValidationWebhook returns a new webhook that validates ChartAssignments.
func NewValidationWebhook(mgr manager.Manager) *admission.Webhook {
	return &admission.Webhook{Handler: newChartAssignmentValidator(mgr.GetScheme())}
}

// NewValidationWebhookForEdgeCluster returns a webhook that checks
// ChartAssignments are valid and apply to a cluster with the given name.
func NewValidationWebhookForEdgeCluster(mgr manager.Manager, clusterName string) *admission.Webhook {
	v := newChartAssignmentValidator(mgr.GetScheme())
	v.clusterName = clusterName
	return &admission.Webhook{Handler: v}
}

// chartAssignmentValidator implements a validation webhook.
type chartAssignmentValidator struct {
	decoder     runtime.Decoder
	clusterName string
}

func newChartAssignmentValidator(sc *runtime.Scheme) *chartAssignmentValidator {
	return &chartAssignmentValidator{
		decoder: serializer.NewCodecFactory(sc).UniversalDeserializer(),
	}
}

func (v *chartAssignmentValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	cur := &apps.ChartAssignment{}
	old := &apps.ChartAssignment{}

	if err := runtime.DecodeInto(v.decoder, req.AdmissionRequest.Object.Raw, cur); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if len(req.AdmissionRequest.OldObject.Raw) > 0 {
		if err := runtime.DecodeInto(v.decoder, req.AdmissionRequest.OldObject.Raw, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	} else {
		old = nil
	}
	if err := v.validate(cur, old); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

func (v *chartAssignmentValidator) validate(cur, old *apps.ChartAssignment) error {
	if cur.Spec.ClusterName == "" {
		return fmt.Errorf("cluster name missing")
	}
	if v.clusterName != "" && cur.Spec.ClusterName != v.clusterName {
		return fmt.Errorf("invalid cluster name %q, expected %q", cur.Spec.ClusterName, v.clusterName)
	}
	if cur.Spec.NamespaceName == "" {
		return fmt.Errorf("namespace name missing")
	}
	errs := validation.ValidateNamespaceName(cur.Spec.NamespaceName, false)
	if len(errs) > 0 {
		return fmt.Errorf("invalid namespace name %q: %s", cur.Spec.NamespaceName, strings.Join(errs, ", "))
	}
	errs = validation.ValidateClusterName(cur.Spec.ClusterName, false)
	if len(errs) > 0 {
		return fmt.Errorf("invalid cluster name %q: %s", cur.Spec.ClusterName, strings.Join(errs, ", "))
	}
	if old != nil {
		if cur.Spec.NamespaceName != old.Spec.NamespaceName {
			return fmt.Errorf("target namespace name must not be changed")
		}
		if cur.Spec.ClusterName != old.Spec.ClusterName {
			return fmt.Errorf("target cluster name must not be changed")
		}
	}
	c := cur.Spec.Chart
	if c.Inline != "" {
		if c.Repository != "" || c.Name != "" || c.Version != "" {
			return fmt.Errorf("chart repository, name, and version must be empty for inline charts")
		}
	} else if c.Repository == "" || c.Name == "" || c.Version == "" {
		return fmt.Errorf("non-inline chart must be fully specified")
	}
	return nil
}
