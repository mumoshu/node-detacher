package main

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
)

const (
	LabelValueAttached = "attached"
	LabelValueDetached = "detached"

	healthy = "Healthy"
)

// deprecatedDetachUnschedulables runs a set of EC2 instance detachments in the loop to update ASGs to not manage unschedulable K8s nodes
func (n *Nodes) deprecatedDetachUnschedulables() error {
	return nil
}

func (n *Nodes) detachNodes(unschedulableNodes []corev1.Node) error {
	for _, node := range unschedulableNodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		labelUpdates := map[string]string{}

		for k, v := range node.Labels {
			ks := strings.Split("k", "/")
			if len(ks) < 2 || !strings.Contains(k, NodeLabelPrefix) || k == NodeKeyLabeled || v == LabelValueDetached {
				continue
			}

			id := ks[1]

			domain := strings.Split(ks[0], ".")[0]

			switch domain {
			case "asg":
				if err := detachInstancesFromASGs(n.asgSvc, id, []string{instanceId}); err != nil {
					return err
				}

				labelUpdates[k] = LabelValueDetached
			case "tg":
				{
					// Prevents alb-ingress-controller from re-registering the target
					// i.e. avoids race between node-detacher and the alb-ingress-controller)
					var latest corev1.Node

					if err := n.client.Get(context.Background(), types.NamespacedName{Name: node.Name}, &latest); err != nil {
						return err
					}

					// See https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/27e5d2a7dc8584123e3997a5dd3d80a58fa7bbd7/internal/ingress/annotations/class/main.go#L52
					latest.Labels["alpha.service-controller.kubernetes.io/exclude-balancer"] = "true"

					if err := n.client.Update(context.Background(), &latest); err != nil {
						return err
					}

					// Note that we continue by de-registering the target on our own, instead of waiting for the
					// alb-ingress-controller to do it for us in favor of "alpha.service-controller.kubernetes.io/exclude-balancer"
					// just to start de-registering the target earlier.
				}

				if len(ks) == 3 {
					if err := deregisterInstanceFromTG(n.elbv2Svc, id, instanceId, ks[2]); err != nil {
						return err
					}
				} else {
					if err := deregisterInstancesFromTGs(n.elbv2Svc, id, []string{instanceId}); err != nil {
						return err
					}
				}

				labelUpdates[k] = LabelValueDetached
			case "clb":
				if err := deregisterInstancesFromCLBs(n.elbSvc, id, []string{instanceId}); err != nil {
					return err
				}
			default:
				return fmt.Errorf("node label %q: unsupported domain %q: must be one of asg, tg, clb", k, domain)
			}
		}

		if len(labelUpdates) > 0 {
			var latest corev1.Node

			if err := n.client.Get(context.Background(), types.NamespacedName{Name: node.Name}, &latest); err != nil {
				return err
			}

			for k, v := range labelUpdates {
				latest.Labels[k] = v
			}

			if err := n.client.Update(context.Background(), &latest); err != nil {
				return err
			}
		}
	}

	return nil
}
