package main

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
)

func getInstanceID(node corev1.Node) (string, error) {
	labels := node.GetLabels()

	instanceID, ok := labels[NodeLabelInstanceID]
	if !ok {
		return "", fmt.Errorf("node must be labeled with `alpha.eksctl.io/instance-id` for this operator to work")
	}

	return instanceID, nil
}
