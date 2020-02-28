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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
)

const (
	containerName = "runner"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Log                   logr.Logger
	Recorder              record.EventRecorder
	Scheme                *runtime.Scheme
	asgSvc                autoscalingiface.AutoScalingAPI
	elbSvc                elbiface.ELBAPI
	elbv2Svc              elbv2iface.ELBV2API
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
		if err := labelAllNodes(r.Client, r.asgSvc, r.elbSvc, r.elbv2Svc); err != nil {
			log.Error(err, "Unable to label all nodes")
		}

		r.synced = true

		return ctrl.Result{}, nil
	}

	var node corev1.Node

	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		log.Error(err, "Failed to get node %q", req.Name)

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !node.Spec.Unschedulable {
		return ctrl.Result{}, nil
	}

	updated := node.DeepCopy()

	NodeDetatching := corev1.NodeConditionType("NodeDetatching")

	for _, cond := range updated.Status.Conditions {
		if cond.Type == NodeDetatching && cond.Status == corev1.ConditionTrue {
			return ctrl.Result{}, nil
		}
	}

	if err := detachNodes(
		r.asgSvc, r.elbSvc, r.elbv2Svc,
		[]corev1.Node{*updated},
		r.detachingNodes,
		r.deregisteringNodes,
		r.deregisteringNodesCLB,
	); err != nil {
		log.Error(err, "Failed to detach nodes")

		return ctrl.Result{}, err
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
	r.Recorder = mgr.GetEventRecorderFor("runner-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}
