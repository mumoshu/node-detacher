package main

import (
	"context"
	"github.com/mumoshu/node-detacher/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	LabelValueAttached = "attached"
	LabelValueDetached = "detached"

	healthy = "Healthy"
)

// deprecatedDetachUnschedulables runs a set of EC2 instance detachments in the loop to update ASGs to not manage unschedulable K8s nodes
func (n *NodeAttachments) deprecatedDetachUnschedulables() error {
	return nil
}

func (n *NodeAttachments) detachNodes(unschedulableNodes []corev1.Node) (bool, error) {
	var processed int

	for _, node := range unschedulableNodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return false, err
		}

		var attachment v1alpha1.Attachment

		ctx := context.Background()

		if err := n.client.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: n.namespace}, &attachment); err != nil {
			n.Log.Error(err, "Failed to get attachment %q: Please ensure that you've correctly set up AWS credentials to fetch AWS API to cache attachments", "node", node.Name)

			continue
		}

		var specUpdates int

		for i, t := range attachment.Spec.AwsTargets {
			if t.Detached {
				continue
			}

			// Prevents alb-ingress-controller from re-registering the target
			// i.e. avoids race between node-detacher and the alb-ingress-controller)
			var latest corev1.Node

			if err := n.client.Get(context.Background(), types.NamespacedName{Name: node.Name}, &latest); err != nil {
				return false, err
			}

			// See https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/27e5d2a7dc8584123e3997a5dd3d80a58fa7bbd7/internal/ingress/annotations/class/main.go#L52
			latest.Labels["alpha.service-controller.kubernetes.io/exclude-balancer"] = "true"

			if err := n.client.Update(context.Background(), &latest); err != nil {
				return false, err
			}

			// Note that we continue by de-registering the target on our own, instead of waiting for the
			// alb-ingress-controller to do it for us in favor of "alpha.service-controller.kubernetes.io/exclude-balancer"
			// just to start de-registering the target earlier.

			if t.Port != nil {
				if err := deregisterInstanceFromTG(n.elbv2Svc, t.ARN, instanceId, *t.Port); err != nil {
					return false, err
				}
			} else {
				if err := deregisterInstancesFromTGs(n.elbv2Svc, t.ARN, []string{instanceId}); err != nil {
					return false, err
				}
			}

			specUpdates++

			attachment.Spec.AwsTargets[i].Detached = true
		}

		for i, l := range attachment.Spec.AwsLoadBalancers {
			if l.Detached {
				continue
			}

			if err := deregisterInstancesFromCLBs(n.elbSvc, l.Name, []string{instanceId}); err != nil {
				return false, err
			}

			specUpdates++

			attachment.Spec.AwsLoadBalancers[i].Detached = true
		}

		if specUpdates > 0 {
			if err := n.client.Update(ctx, &attachment); err != nil {
				return false, err
			}

			processed++
		}
	}

	return processed > 0, nil
}
