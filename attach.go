package main

import (
	"context"
	"github.com/mumoshu/node-detacher/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (n *NodeAttachments) attachNodes(nodes []corev1.Node) error {
	for _, node := range nodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		var attachment v1alpha1.Attachment

		ctx := context.Background()

		if err := n.client.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: n.namespace}, &attachment); err != nil {
			n.Log.Error(err, "Failed to get attachment %q", node.Name)

			continue
		}

		var specUpdates int

		for i, tg := range attachment.Spec.AwsTargets {
			{
				// Prevents alb-ingress-controller from re-registering the target
				// i.e. avoids race between node-detacher and the alb-ingress-controller)
				var latest corev1.Node

				if err := n.client.Get(context.Background(), types.NamespacedName{Name: node.Name}, &latest); err != nil {
					return err
				}

				// See https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/27e5d2a7dc8584123e3997a5dd3d80a58fa7bbd7/internal/ingress/annotations/class/main.go#L52
				delete(latest.Labels, "alpha.service-controller.kubernetes.io/exclude-balancer")

				if err := n.client.Update(context.Background(), &latest); err != nil {
					return err
				}

				// Note that we continue by registering the target on our own, instead of waiting for the
				// alb-ingress-controller to do it for us in favor of the removal of "alpha.service-controller.kubernetes.io/exclude-balancer"
			}

			if tg.Port != nil {
				if err := attachInstanceToTG(n.elbv2Svc, tg.ARN, instanceId, *tg.Port); err != nil {
					return err
				}
			} else {
				if err := attachInstanceToTG(n.elbv2Svc, tg.ARN, instanceId); err != nil {
					return err
				}
			}

			specUpdates++

			attachment.Spec.AwsTargets[i].Detached = false
		}

		for i, l := range attachment.Spec.AwsLoadBalancers {
			if err := registerInstancesToCLBs(n.elbSvc, l.Name, []string{instanceId}); err != nil {
				return err
			}

			specUpdates++

			attachment.Spec.AwsLoadBalancers[i].Detached = false
		}

		if specUpdates > 0 {
			if err := n.client.Update(context.Background(), &attachment); err != nil {
				return err
			}
		}
	}

	return nil
}
