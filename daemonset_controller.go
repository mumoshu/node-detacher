/*
Copyright 2020 The node-detacher authors.

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

package main

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"math"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	PodFieldOwnerDaemonSet = ".metadata.daemonsetowner"
	PodAnnotationDetaching = "node-detacher.variant.run/detaching"
)

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;create;update;patch

// DaemonsetController reconciles daemonset pods
type DaemonsetController struct {
	// Name is the name of the manager used from within daemonset annotations to specify which node-detacher instance to manage the daemonset
	Name string

	client.Client
	Log      logr.Logger
	recorder record.EventRecorder

	// DaemonSets is the list of daemonsets whose item is either "NAME" or "NAMESPACE/NAME" of the target daemonset.
	//
	// For example, let's say you'd like node-detacher deployed in kube-system to detach the node which is running the target
	// pod and when the pod becomes `Terminating` state.
	//
	// When the pod is named `contour-<hash>` and it is managed by the daemonset named `contour` in namespace
	// `kube-system`, you'd specifyc the daemonsets list as:
	//
	//  --daemonsets contour`
	//
	// If you'd like to deploy `contour` in another namespace that is different from where `node-detacher` is deployed
	// to - e.g. `ingress` namespace - you'd specify the list as:
	//
	// --daemonsets ingress/contour
	DaemonSets []string

	// Namespace is the default namespace to watch for daemonset pods
	Namespace string
}

func (r *DaemonsetController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	log := r.Log.WithValues("pod", req.NamespacedName)

	namespaces, daemonsetNames := CalculateTargets(r.DaemonSets)

	dsNamespace := req.Namespace

	// Try to list daemonsets only when there's no concrete list of target daemonsets specified.
	// This gives you an option no not poke K8s API on each reconcilation loop
	if r.Name != "" && len(r.DaemonSets) == 0 {
		var dsList appsv1.DaemonSetList

		if err := r.List(ctx, &dsList, client.InNamespace(dsNamespace), client.MatchingFields{DaemonSetFieldManagedBy: r.Name}); err != nil {
			return ctrl.Result{}, err
		}

		for _, ds := range dsList.Items {
			namespaces[ds.GetNamespace()] = true
			daemonsetNames[ds.GetName()] = true
		}
	}

	_, nsTargeted := namespaces[dsNamespace]
	if !nsTargeted {
		log.Info("Skipping this pod. Only pods in one of target namespaces are reconciled by me.")

		return ctrl.Result{}, nil
	}

	var ds appsv1.DaemonSet

	if err := r.Client.Get(ctx, req.NamespacedName, &ds); err != nil {
		log.Error(err, "Failed getting pod owner")

		return ctrl.Result{}, err
	}

	if ds.Spec.UpdateStrategy.Type != appsv1.OnDeleteDaemonSetStrategyType {
		return ctrl.Result{}, nil
	}

	if ds.Status.DesiredNumberScheduled <= ds.Status.UpdatedNumberScheduled {
		// All the daemonset pods are up-to-date. Nothing to do
		return ctrl.Result{}, nil
	}

	var podList corev1.PodList

	if err := r.List(ctx, &podList, client.MatchingFields{PodFieldOwnerDaemonSet: ds.Name}); err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, err
	}

	for _, pod := range podList.Items {
		if GetPodTemplateGeneration(pod.GetObjectMeta()) < ds.Generation {
			// Immediately marks for termination, but defer terminate until we detach the node first
			if GetAnnotation(pod.GetObjectMeta(), PodAnnotationDetaching) != r.Name {
				newPod := pod.DeepCopy()

				SetAnnotation(newPod.GetObjectMeta(), PodAnnotationDetaching, r.Name)

				if err := r.Patch(ctx, newPod, client.MergeFrom(&pod)); err != nil {
					return ctrl.Result{RequeueAfter: 1 * time.Second}, err
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

func SetAnnotation(meta metav1.Object, key string, value string) {
	meta.GetAnnotations()[key] = value
}

func GetAnnotation(meta metav1.Object, key string) string {
	for k, v := range meta.GetAnnotations() {
		if k == key {
			return v
		}
	}

	return ""
}

func GetPodTemplateGeneration(meta metav1.Object) int64 {
	for k, v := range meta.GetLabels() {
		if k == "pod-template-generation" {
			vi, err := strconv.Atoi(v)
			if err != nil {
				panic(fmt.Errorf("error parsing pod-template-generation of pod %s/%s: %w", meta.GetNamespace(), meta.GetName(), err))
			}

			return int64(vi)
		}
	}

	return math.MaxInt64
}

func (r *DaemonsetController) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor(r.Name)

	if err := mgr.GetFieldIndexer().IndexField(&corev1.Pod{}, PodFieldOwnerDaemonSet, func(rawObj runtime.Object) []string {
		pod := rawObj.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil
		}

		if owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != "DaemonSet" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)

}
