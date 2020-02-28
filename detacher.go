package main

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
)

const (
	healthy = "Healthy"
)

// detachUnschedulables runs a set of EC2 instance detachments in the loop to update ASGs to not manage unschedulable K8s nodes
func (n *Nodes) detachUnschedulables() error {
	unschedulableNodes, err := n.k8sSvc.getUnschedulableNodes()
	if err != nil {
		return err
	}

	return n.detachNodes(unschedulableNodes)
}

func (n *Nodes) detachNodes(unschedulableNodes []corev1.Node) error {
	for _, node := range unschedulableNodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		labelUpdates := map[string]string{}

		LabelValueDetached := "detached"

		for k, v := range node.Labels {
			ks := strings.Split("k", "/")
			if len(ks) != 2 || !strings.Contains(k, NodeLabelPrefix) || k == KeyLabeled || v == LabelValueDetached {
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
				if err := deregisterInstancesFromTGs(n.elbv2Svc, id, []string{instanceId}); err != nil {
					return err
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
				node.Labels[k] = v
			}

			if err := n.client.Update(context.Background(), &latest); err != nil {
				return err
			}
		}
	}

	return nil
}
