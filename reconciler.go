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
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	NodeConditionTypeNodeBeingDetached = corev1.NodeConditionType("NodeBeingDetached")
	NodeEventReasonNodeBeingDetached   = "NodeBeingDetached"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Log      logr.Logger
	recorder record.EventRecorder
	Scheme   *runtime.Scheme
	nodes    *Nodes

	// ALBIngressIntegrationEnabled is set to true when node-detacher should interoperate with
	// aws-alb-ingress-controller(https://github.com/kubernetes-sigs/aws-alb-ingress-controller)
	//
	// When enabled, node-detacher behaves as follows:
	// - Stop labeling all nodes on startup because the desired node-to-targetgroup relationship can't be determined
	//   until the node becomes Unschedulable.
	// - Stop labeling the node on creation due to the same reason as the above
	ALBIngressIntegrationEnabled bool

	// DynamicNLBIntegrationEnabled is set to true when node-detacher should interoperate with
	// NLBs managed via `type: LoadBalancer` services
	DynamicNLBIntegrationEnabled bool

	// DynamicCLBIntegrationEnabled is set to true when node-detacher should interoperate with
	// CLBs managed via `type: LoadBalancer` services
	DynamicCLBIntegrationEnabled bool

	// StaticTargetGroupIntegrationEnabled is set to true when node-detacher should interoperate with
	// target groups managed externally to Kubernetes (e.g. via Terraform or CloudFormation)
	StaticTargetGroupIntegrationEnabled bool

	// StaticCLBIntegrationEnabled is set to true when node-detacher should interoperate with
	// CLBs managed externally to Kubernetes (e.g. via Terraform or CloudFormation)
	StaticCLBIntegrationEnabled bool

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

	asgSvc   autoscalingiface.AutoScalingAPI
	elbSvc   elbiface.ELBAPI
	elbv2Svc elbv2iface.ELBV2API

	synced bool
}

// staticMode returns true when node-detacher's static mode is enabled.
//
// In static mode, node-to-clb and/or node-to-targetgroup relationship is static and can be known at the time of the
// node being created.
func (r *NodeReconciler) staticMode() bool {
	return !r.dynamicMode()
}

func (r *NodeReconciler) dynamicMode() bool {
	return r.ALBIngressIntegrationEnabled || r.DynamicNLBIntegrationEnabled || r.DynamicCLBIntegrationEnabled
}

func (r *NodeReconciler) shouldHandleTargetGroups() bool {
	return r.StaticCLBIntegrationEnabled || r.ALBIngressIntegrationEnabled || r.DynamicNLBIntegrationEnabled
}

func (r *NodeReconciler) shouldHandleCLBs() bool {
	return r.StaticCLBIntegrationEnabled || r.DynamicCLBIntegrationEnabled
}

func (r *NodeReconciler) configuredForDaemonSets() bool {
	return len(r.DaemonSets) > 0
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch

func (r *NodeReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	if r.configuredForDaemonSets() {
		log := r.Log.WithValues("pod", req.NamespacedName)

		namespaces := map[string]bool{}

		daemonsetNames := map[string]bool{}

		for _, ds := range r.DaemonSets {
			nsName := strings.Split(ds, "/")

			if len(nsName) > 1 {
				namespaces[nsName[0]] = true
				daemonsetNames[nsName[1]] = true
			} else {
				daemonsetNames[nsName[0]] = true
			}
		}

		_, nsTargeted := namespaces[req.Namespace]
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

		_, ownerTargeted := daemonsetNames[owner.Name]

		if !ownerTargeted {
			log.Info("Skipping this pod. Only pods that are managed by one of target daemonsets are reconciled by me", "owner", owner.Name)

			return ctrl.Result{}, nil
		}

		// Continue by reconciling the node on which the pod is running
		req.NamespacedName = types.NamespacedName{
			Namespace: "",
			Name:      latestPod.Spec.NodeName,
		}
	}

	log := r.Log.WithValues("node", req.NamespacedName)

	if r.nodes == nil {
		r.nodes = &Nodes{
			Log:              ctrl.Log.WithName("models").WithName("Nodes"),
			client:           r.Client,
			asgSvc:           r.asgSvc,
			elbSvc:           r.elbSvc,
			elbv2Svc:         r.elbv2Svc,
			shouldHandleCLBs: r.shouldHandleCLBs(),
			shouldHandleTGs:  r.shouldHandleTargetGroups(),
		}
	}

	if !r.synced && r.staticMode() {
		log.Info("Labeling all nodes on startup")

		if err := r.nodes.labelAllNodes(); err != nil {
			log.Error(err, "Unable to label all nodes")
		}

		r.synced = true
	}

	var node corev1.Node

	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		log.Error(err, "Failed to get node %q", req.Name)

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.staticMode() && !r.nodes.Labeled(node) {
		log.Info("Labeling node on init", "node", node.Name)

		if err := r.nodes.labelNodes([]corev1.Node{node}); err != nil {
			log.Error(err, "Unable to label node")
		}
	}

	var nodeBeingDetached bool

	for _, cond := range node.Status.Conditions {
		if cond.Type == NodeConditionTypeNodeBeingDetached && cond.Status == corev1.ConditionTrue {
			nodeBeingDetached = true

			break
		}
	}

	var toBeDeletedByCA bool

	for _, taint := range node.Spec.Taints {
		// Cluster Autoscaler tries to make the node unschedulable by adding a taint whose key is
		// `ToBeDeletedByClusterAutoscaler`.
		//
		// References:
		//
		// MarkToBeDeleted:
		// https://github.com/kubernetes/autoscaler/blob/7ecf51e4bfab24b6d9c6520d8a851052e5a447fb/cluster-autoscaler/utils/deletetaint/delete.go#L59-L62
		//
		// ScaleDown.deleteNode:
		// https://github.com/kubernetes/autoscaler/blob/af1dd84305d3c6bebd22373a7bcf7aebad5a91f5/cluster-autoscaler/core/scale_down.go#L1109-L1112
		if taint.Key == "ToBeDeletedByClusterAutoscaler" {
			toBeDeletedByCA = true

			break
		}
	}

	nodeIsSchedulable := !node.Spec.Unschedulable && !toBeDeletedByCA

	if nodeBeingDetached {
		log.Info("Node is already being detached", "node", node.Name)

		if nodeIsSchedulable {
			// Immediately start re-attaching the node to TGs and CLBs that the node is already de-registered from in the previous loop.
			//
			// Why? To interoperate with crashed cluster-autoscaler.
			//
			// The node with the "NodeDetaching" means we did start detaching the node in the previous loop.
			// But the node being schedulable after that means that CA cancelled the scale-down.
			//
			// As CA already cancelled the scale-down, we should do our best to revert the changes on our side, too.
			// More concretely, we should re-attach the node to corresponding TGs and CLBs because those are changes
			// made by node-detacher.
			//
			// See StaticAutoscaler.cleanUpIfRequired for more information on how CA cancels a scale-down after crash:
			// https://github.com/kubernetes/autoscaler/blob/dbbd4572af2b666d32e582bf88c4239163706f8c/cluster-autoscaler/core/static_autoscaler.go#L170-L190
			if err := r.nodes.attachNodes([]corev1.Node{node}); err != nil {
				log.Error(err, "Failed to reattach nodes")

				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}

			updated := node.DeepCopy()

			updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
				Type:   NodeConditionTypeNodeBeingDetached,
				Status: corev1.ConditionFalse,
			})

			if err := r.Update(ctx, updated); err != nil {
				log.Error(err, "Failed to update node condition", "node", updated.Name)

				return ctrl.Result{}, client.IgnoreNotFound(err)
			}

			r.recorder.Event(&node, corev1.EventTypeNormal, "NodeDetatching", "Successfully stopped detaching and started re-attaching node")
			log.Info("Started re-attaching node", "node", node.Name)
		}

		return ctrl.Result{}, nil
	}

	if nodeIsSchedulable {
		// Node is still schedulable.
		//
		// We only detach the node when it is unschedulable.
		// Wait until the node becomes unscheduralble.
		return ctrl.Result{}, nil
	}

	if r.dynamicMode() {
		log.Info("Labeling node on detach", "node", node.Name)

		if err := r.nodes.labelNodes([]corev1.Node{node}); err != nil {
			log.Error(err, "Unable to label node")
		}
	}

	updated := node.DeepCopy()

	if err := r.nodes.detachNodes(
		[]corev1.Node{*updated},
	); err != nil {
		log.Error(err, "Failed to detach nodes")

		return ctrl.Result{RequeueAfter: 1 * time.Second}, err
	}

	updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
		Type:   NodeConditionTypeNodeBeingDetached,
		Status: corev1.ConditionTrue,
	})

	if err := r.Update(ctx, updated); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.recorder.Event(&node, corev1.EventTypeNormal, NodeEventReasonNodeBeingDetached, "Successfully started detaching node")
	log.Info("Started detaching node", "node", node.Name)

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("node-detacher")

	if r.configuredForDaemonSets() {
		err := ctrl.NewControllerManagedBy(mgr).
			For(&corev1.Pod{}).
			Complete(r)

		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}
