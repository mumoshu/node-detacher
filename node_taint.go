package main

import "k8s.io/api/core/v1"

func taintNode(newNode *v1.Node, value string) {
	// Immediately stops scheduling more pods on this node because
	// we're going to drain the node shortly.
	var set bool

	for i := range newNode.Spec.Taints {
		if newNode.Spec.Taints[i].Key == NodeTaintKeyDetaching {
			newNode.Spec.Taints[i].Value = value
			set = true
		}
	}

	if !set {
		newNode.Spec.Taints = append(newNode.Spec.Taints, v1.Taint{
			Key:       NodeTaintKeyDetaching,
			Value:     value,
			Effect:    v1.TaintEffectNoSchedule,
			TimeAdded: nil,
		})
	}
}

func untaintNode(node *v1.Node) {
	var taints []v1.Taint

	for _, t := range node.Spec.Taints {
		if t.Key == NodeTaintKeyDetaching {
			continue
		}

		taints = append(taints, t)
	}

	node.Spec.Taints = taints
}
