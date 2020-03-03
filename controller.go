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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Log                   logr.Logger
	Recorder              record.EventRecorder
	Scheme                *runtime.Scheme
	nodes                 *Nodes
	detachingNodes        map[string]map[string]bool
	deregisteringNodes    map[string]map[string]bool
	deregisteringNodesCLB map[string]map[string]bool

	synced bool
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch

func (r *NodeReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("node", req.NamespacedName)

	if !r.synced {
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

	NodeDetatching := corev1.NodeConditionType("NodeDetatching")

	var nodeIsDetaching bool

	for _, cond := range node.Status.Conditions {
		if cond.Type == NodeDetatching && cond.Status == corev1.ConditionTrue {
			nodeIsDetaching = true

			break
		}
	}

	nodeIsSchedulable := !node.Spec.Unschedulable

	if nodeIsDetaching {
		if nodeIsSchedulable {
			// Immediately start re-attaching the node to TGs and CLBs that the node is already de-registered from in the previous loop.
			//
			// Why? To interoperate with CA.
			//
			// The node with the "NodeDetaching" means we did start detaching the node in the previous loop.
			// But the node being schedulable after that means that CA cancelled the scale-down.
			//
			// As CA already cancelled the scale-down, we should do our best to revert the changes on our side, too.
			// More concretely, we should re-attach the node to corresponding TGs and CLBs because those are changes
			// made by node-detacher.
			//
			// See StaticAutoscaler.cleanUpIfRequired for more information on how CA cancels a scale-down:
			// https://github.com/kubernetes/autoscaler/blob/dbbd4572af2b666d32e582bf88c4239163706f8c/cluster-autoscaler/core/static_autoscaler.go#L170-L190
			if err := r.nodes.attachNodes([]corev1.Node{node}); err != nil {
				log.Error(err, "Failed to reattach nodes")

				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}

			updated := node.DeepCopy()

			updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
				Type:   NodeDetatching,
				Status: corev1.ConditionFalse,
			})

			if err := r.Update(ctx, updated); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}

			r.Recorder.Event(&node, corev1.EventTypeNormal, "NodeDetatching", "Successfully stopped detaching and started re-attaching node")
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

	updated := node.DeepCopy()

	if err := r.nodes.detachNodes(
		[]corev1.Node{*updated},
	); err != nil {
		log.Error(err, "Failed to detach nodes")

		return ctrl.Result{RequeueAfter: 1 * time.Second}, err
	}

	updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
		Type:   NodeDetatching,
		Status: corev1.ConditionTrue,
	})

	if err := r.Update(ctx, updated); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.Recorder.Event(&node, corev1.EventTypeNormal, "NodeDetatching", "Successfully started detaching node")
	log.Info("Started detaching node", "node", node.Name)

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("node-detacher")

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}
