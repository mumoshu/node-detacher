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
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	NodeLabelInstanceID                  = "alpha.eksctl.io/instance-id"
	NodeTaintKeyDetaching                = "node-detacher.variant.run/detaching"
	NodeTaintToBeDeletedByCA             = "ToBeDeletedByClusterAutoscaler"
	NodeAnnotationKeyDetached            = "node-detacher.variant.run/detached"
	NodeAnnotationKeyDetaching           = "node-detacher.variant.run/detaching"
	NodeAnnotationKeyDetachmentTimestamp = "node-detacher.variant.run/detachment-timestamp"
	NodeAnnotationKeyAttachmentTimestamp = "node-detacher.variant.run/attachment-timestamp"

	DaemonSetAnnotationKeyManagedBy     = "node-detacher.variant.run/managed-by"
	PodAnnotationKeyPodDeletionPriority = "node-detacher.variant.run/deletion-priority"
	DaemonSetFieldManagedBy             = ".managedby"

	PodAnnotationDisableEviction = "node-detacher.variant.run/disable-eviction"

	NodeConditionTypeNodeBeingDetached = corev1.NodeConditionType("NodeBeingDetached")
	NodeEventReasonNodeBeingDetached   = "NodeBeingDetached"
)

// +kubebuilder:rbac:groups=node-detacher.variant.run,resources=attachments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node-detacher.variant.run,resources=attachments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;create;update;patch

// NodeController reconciles a Node object
type NodeController struct {
	// Name is the name of the manager used from within daemonset annotations to specify which node-detacher instance to manage the daemonset
	Name string

	client.Client
	Log             logr.Logger
	recorder        record.EventRecorder
	Scheme          *runtime.Scheme
	nodeAttachments *NodeAttachments

	// AWS enables AWS support including ELB v1, ELB v2(target group) integrations. Also specify enable-(static|dynamic)(alb|clb|nlb)-integration flags for detailed configuration
	AWSEnabled bool

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

	// ManageDaemonSets, when set to true, instructs denotes that this a daemonset reconciler
	ManageDaemonSets bool

	// ManageDaemonSetPods, when set to true, instructs denotes that this a daemonset pod reconciler
	ManageDaemonSetPods bool

	// Namespace is the namespace in which `attachment` resources are created
	Namespace string

	asgSvc   autoscalingiface.AutoScalingAPI
	elbSvc   elbiface.ELBAPI
	elbv2Svc elbv2iface.ELBV2API

	synced bool

	CoreV1Client v1.CoreV1Interface
}

// staticMode returns true when node-detacher's static mode is enabled.
//
// In static mode, node-to-clb and/or node-to-targetgroup relationship is static and can be known at the time of the
// node being created.
func (r *NodeController) staticMode() bool {
	return !r.dynamicMode()
}

func (r *NodeController) dynamicMode() bool {
	return r.ALBIngressIntegrationEnabled || r.DynamicNLBIntegrationEnabled || r.DynamicCLBIntegrationEnabled
}

func (r *NodeController) shouldHandleTargetGroups() bool {
	return r.StaticCLBIntegrationEnabled || r.ALBIngressIntegrationEnabled || r.DynamicNLBIntegrationEnabled
}

func (r *NodeController) shouldHandleCLBs() bool {
	return r.StaticCLBIntegrationEnabled || r.DynamicCLBIntegrationEnabled
}

func (r *NodeController) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	log := r.Log.WithValues("node", req.NamespacedName)

	if r.nodeAttachments == nil {
		r.nodeAttachments = &NodeAttachments{
			Log:              ctrl.Log.WithName("models").WithName("NodeAttachments"),
			client:           r.Client,
			asgSvc:           r.asgSvc,
			elbSvc:           r.elbSvc,
			elbv2Svc:         r.elbv2Svc,
			shouldHandleCLBs: r.shouldHandleCLBs(),
			shouldHandleTGs:  r.shouldHandleTargetGroups(),
			namespace:        r.Namespace,
		}
	}

	var node corev1.Node

	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		log.Error(err, "Failed to get node %q", req.Name)

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	manageAttachment := r.AWSEnabled // || r.GCPEnabled
	// Do detach from ASG only on AWS
	if _, err := getInstanceID(node); err != nil {
		manageAttachment = false
	}

	if manageAttachment {
		if !r.synced && r.staticMode() {
			log.Info("Labeling all nodes on startup")

			if err := r.nodeAttachments.cacheAllNodeAttachments(); err != nil {
				log.Error(err, "Unable to label all nodes")
			}

			r.synced = true
		}

		if r.staticMode() && !r.nodeAttachments.Cached(node) {
			log.Info("Labeling node on init", "node", node.Name)

			if err := r.nodeAttachments.cacheNodeAttachments([]corev1.Node{node}); err != nil {
				log.Error(err, "Unable to label node")
			}
		}
	}

	var isMasterNode bool

	for _, t := range node.Spec.Taints {
		if t.Key == "node-role.kubernetes.io/master" {
			isMasterNode = true

			break
		}
	}

	if isMasterNode {
		log.Info("Skipped master node")

		return ctrl.Result{}, nil
	}

	var (
		nodeBeingDetached bool
		nodeRequireDetached bool
	)

	for _, cond := range node.Status.Conditions {
		if cond.Type == NodeConditionTypeNodeBeingDetached && cond.Status == corev1.ConditionTrue {
			nodeBeingDetached = true

			break
		}
	}

	for k, v := range node.Annotations {
		if k == NodeAnnotationKeyDetaching && v == "true" {
			nodeBeingDetached = true
		}

		if k == NodeAnnotationKeyDetached {
			nodeRequireDetached = true
		}
	}

	var toBeDeletedByCA bool

	var hasAnyCustomTaint bool

	var hasAnyK8sTaint bool

	NodeTaintKeyK8sNode := "node.kubernetes.io/"

	ignoredTaintPrefixes := []string{
		NodeTaintToBeDeletedByCA,
		NodeTaintKeyDetaching,
		NodeTaintKeyK8sNode,
	}

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
		if taint.Key == NodeTaintToBeDeletedByCA {
			toBeDeletedByCA = true
		}

		if strings.HasPrefix(taint.Key, NodeTaintKeyK8sNode) {
			hasAnyK8sTaint = true
		}

		ignored := false
		for _, key := range ignoredTaintPrefixes {
			if strings.HasPrefix(taint.Key, key) {
				ignored = true
			}
		}

		if !ignored {
			hasAnyCustomTaint = true
		}
	}

	// Note:
	// - Node becomes Unschedulable when cordoned
	// - Node should be considered unschedulable when it is already tained by CA for scale down
	// - Node should be considered unschedulable when it is already tained by node-detacher for detachment
	nodeIsSchedulable := !node.Spec.Unschedulable && !toBeDeletedByCA && !hasAnyK8sTaint && !hasAnyCustomTaint &&!nodeRequireDetached

	detachNode := func() (*ctrl.Result, error) {
		if !manageAttachment {
			return nil, nil
		}

		if r.dynamicMode() {
			log.Info("Labeling node on detach", "node", node.Name)

			if err := r.nodeAttachments.cacheNodeAttachments([]corev1.Node{node}); err != nil {
				log.Error(err, "Unable to label node")
			}
		}

		processed, err := r.nodeAttachments.detachNodes(
			[]corev1.Node{node},
		)

		if err != nil {
			log.Error(err, "Failed to detach nodes")

			return &ctrl.Result{RequeueAfter: 1 * time.Second}, err
		}

		if err != nil {
			log.Error(err, "Failed to detach nodes")

			return &ctrl.Result{RequeueAfter: 1 * time.Second}, err
		}

		if !processed {
			log.Info("Skipped detaching node. Already detaching.")
		}

		return nil, nil
	}

	deleteDSPods := func() (*ctrl.Result, error) {
		if nodeRequireDetached {
			return nil, nil
		}

		if err := DeletePods(r.Client, r.CoreV1Client, log, node); err != nil {
			return &ctrl.Result{RequeueAfter: 1 * time.Second}, err
		}

		return nil, nil
	}

	detachAll := func() (*ctrl.Result, error) {
		if r, err := detachNode(); err != nil {
			return r, err
		}

		if r, err := deleteDSPods(); err != nil {
			return r, err
		}

		return nil, nil
	}

	attachNode := func() (*ctrl.Result, error) {
		if !manageAttachment {
			return nil, nil
		}

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
		if err := r.nodeAttachments.attachNodes([]corev1.Node{node}); err != nil {
			log.Error(err, "Failed to reattach nodes")

			return &ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}

		return nil, nil
	}

	if nodeBeingDetached {
		log.Info("Node is already being detached")

		if nodeIsSchedulable {
			log.Info("Node is now schedulable. Re-attaching...")

			if r, err := attachNode(); err != nil {
				return *r, err
			}

			updated := node.DeepCopy()

			updated.Annotations[NodeAnnotationKeyDetaching] = "false"

			updated.Annotations[NodeAnnotationKeyAttachmentTimestamp] = time.Now().Format(time.RFC3339)
			delete(updated.Annotations, NodeAnnotationKeyDetachmentTimestamp)

			updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
				Type:               NodeConditionTypeNodeBeingDetached,
				Status:             corev1.ConditionFalse,
				Reason:             "AttachmentStarted",
				Message:            "Successfully stopped detaching and started re-attaching node",
				LastTransitionTime: metav1.NewTime(time.Now()),
			})

			updated.Labels[NodeLabelKeyCached] = "false"

			untaintNode(updated)

			if err := r.Client.Update(ctx, updated); err != nil {
				log.Error(err, "Failed to update node conditions and annotations", "node", updated.Name)

				return ctrl.Result{}, err
			}

			r.recorder.Event(&node, corev1.EventTypeNormal, "NodeDetatching", "Successfully stopped detaching and started re-attaching node")
			log.Info("Started re-attaching node", "node", node.Name)
		} else {
			log.Info("Ensuring node to be detached")

			if r, err := detachAll(); err != nil {
				return *r, err
			}
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

	if r, err := detachAll(); err != nil {
		return *r, err
	}

	updated := node.DeepCopy()

	if updated.Annotations == nil {
		// Spec.Annotations seems to be guaranteed to be non-nil at runtime, but not in test.
		// If we forget this, our controller does panic due to `assignment to entry in nil map` only in test.
		updated.Annotations = map[string]string{}
	}

	updated.Annotations[NodeAnnotationKeyDetaching] = "true"

	updated.Annotations[NodeAnnotationKeyDetachmentTimestamp] = time.Now().Format(time.RFC3339)
	delete(updated.Annotations, NodeAnnotationKeyAttachmentTimestamp)

	updated.Status.Conditions = append(updated.Status.Conditions, corev1.NodeCondition{
		Type:               NodeConditionTypeNodeBeingDetached,
		Status:             corev1.ConditionFalse,
		Reason:             "DetachmentStarted",
		Message:            "Successfully started detaching node",
		LastTransitionTime: metav1.NewTime(time.Now()),
	})

	taintNode(updated, r.Name)

	if err := r.Client.Update(ctx, updated); err != nil {
		log.Error(err, "Failed to update node conditions and annotations for detach", "node", updated.Name)

		return ctrl.Result{}, err
	}

	log.Info("Successfully tainted node")

	r.recorder.Event(&node, corev1.EventTypeNormal, NodeEventReasonNodeBeingDetached, "Successfully started detaching node")
	log.Info("Started detaching node", "node", node.Name)

	return ctrl.Result{}, nil
}

func (r *NodeController) SetConditions(node *corev1.Node, newConditions []corev1.NodeCondition) error {
	for i := range newConditions {
		// Each time we update the conditions, we update the heart beat time
		newConditions[i].LastHeartbeatTime = metav1.NewTime(time.Now())
	}

	patch, err := generatePatch(newConditions)
	if err != nil {
		return err
	}

	return r.Client.Patch(context.TODO(), node, client.ConstantPatch(types.StrategicMergePatchType, patch))
}

// generatePatch generates condition patch
func generatePatch(conditions []corev1.NodeCondition) ([]byte, error) {
	raw, err := json.Marshal(&conditions)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, raw)), nil
}

func (r *NodeController) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor(r.Name)

	if err := mgr.GetFieldIndexer().IndexField(&corev1.Pod{}, "spec.nodeName", func(rawObj runtime.Object) []string {
		pod := rawObj.(*corev1.Pod)

		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}
