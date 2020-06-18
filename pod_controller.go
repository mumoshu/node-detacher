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
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;create;update;patch

// PodController reconciles daemonset pods
type PodController struct {
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

func CalculateTargets(targetDaemonsets []string) (map[string]bool, map[string]bool) {
	namespaces := map[string]bool{}

	daemonsetNames := map[string]bool{}

	for _, ds := range targetDaemonsets {
		nsName := strings.Split(ds, "/")

		if len(nsName) > 1 {
			namespaces[nsName[0]] = true
			daemonsetNames[nsName[1]] = true
		} else {
			daemonsetNames[nsName[0]] = true
		}
	}

	return namespaces, daemonsetNames
}

func (r *PodController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	log := r.Log.WithValues("pod", req.NamespacedName)

	namespaces, daemonsetNames := CalculateTargets(r.DaemonSets)

	podNamespace := req.Namespace

	// Try to list daemonsets only when there's no concrete list of target daemonsets specified.
	// This gives you an option no not poke K8s API on each reconcilation loop
	if r.Name != "" && len(r.DaemonSets) == 0 {
		var dsList appsv1.DaemonSetList

		if err := r.List(ctx, &dsList, client.InNamespace(podNamespace), client.MatchingFields{DaemonSetFieldManagedBy: r.Name}); err != nil {
			return ctrl.Result{}, err
		}

		for _, ds := range dsList.Items {
			namespaces[ds.GetNamespace()] = true
			daemonsetNames[ds.GetName()] = true
		}
	}

	_, nsTargeted := namespaces[podNamespace]
	if !nsTargeted {
		log.Info("Skipping this pod. Only pods in one of target namespaces are reconciled by me.")

		return ctrl.Result{}, nil
	}

	var latestPod corev1.Pod

	if err := r.Client.Get(ctx, req.NamespacedName, &latestPod); err != nil {
		log.Error(err, "Failed getting pod owner")

		return ctrl.Result{}, err
	}

	owner := metav1.GetControllerOf(&latestPod)

	if owner.Kind != "DaemonSet" {
		log.Info("Skipping this pod. Only daemonset pods are reconciled by me", "kind", owner.Kind)

		return ctrl.Result{}, nil
	}

	// Do thing on non-terminating pod
	// Also - it seems like there's no PodPhase `Terminating` even so terminating pods shown as `Terminating` in `kubectl get po` output.
	// Perhaps it's seeing deletion timestamp? We assume so here.
	// https://github.com/kubernetes/api/blob/b5bd82427fa87d8b6fdf2c0b4cc2a2115c0c6de9/core/v1/types.go#L2414-L2435
	if latestPod.GetDeletionTimestamp() == nil && GetAnnotation(latestPod.GetObjectMeta(), PodAnnotationDetaching) != r.Name {
		return ctrl.Result{}, nil
	}

	_, ownerTargeted := daemonsetNames[owner.Name]

	if !ownerTargeted {
		log.Info("Skipping this pod. Only pods that are managed by one of target daemonsets are reconciled by me", "owner", owner.Name)

		return ctrl.Result{}, nil
	}

	// Continue by reconciling the node on which the pod is running
	nodeKey := types.NamespacedName{
		Namespace: "",
		Name:      latestPod.Spec.NodeName,
	}

	var node corev1.Node

	if err := r.Get(ctx, nodeKey, &node); err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, err
	}

	newNode := node.DeepCopy()

	taintNode(newNode, r.Name)

	if err := r.Patch(ctx, newNode, client.MergeFrom(&node)); err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (r *PodController) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor(r.Name)

	if err := mgr.GetFieldIndexer().IndexField(&appsv1.DaemonSet{}, DaemonSetFieldManagedBy, func(rawObj runtime.Object) []string {
		ds := rawObj.(*appsv1.DaemonSet)

		managedBy := GetManagedBy(ds.GetObjectMeta())

		if managedBy == "" {
			return nil
		}

		return []string{managedBy}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)

}

func GetManagedBy(controllee metav1.Object) string {
	for k, v := range controllee.GetAnnotations() {
		if k == DaemonSetAnnotationKeyManagedBy {
			return v
		}
	}
	return ""
}
