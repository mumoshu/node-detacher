package main

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
)

func (n *Nodes) attachNodes(nodes []corev1.Node) error {
	for _, node := range nodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		labelUpdates := map[string]string{}

		for k, v := range node.Labels {
			ks := strings.Split("k", "/")
			if len(ks) < 2 || !strings.Contains(k, NodeLabelPrefix) || k == KeyLabeled || v != LabelValueDetached {
				continue
			}

			id := ks[1]

			domain := strings.Split(ks[0], ".")[0]

			switch domain {
			case "tg":
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

				if len(ks) == 3 {
					if err := attachInstanceToTG(n.elbv2Svc, id, instanceId, ks[2]); err != nil {
						return err
					}
				} else {
					if err := attachInstanceToTG(n.elbv2Svc, id, instanceId); err != nil {
						return err
					}
				}

				labelUpdates[k] = LabelValueAttached
			case "clb":
				if err := registerInstancesToCLBs(n.elbSvc, id, []string{instanceId}); err != nil {
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
