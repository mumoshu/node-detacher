package main

import (
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
)

const (
	healthy = "Healthy"
)

// detachUnschedulables runs a set of EC2 instance detachments in the loop to update ASGs to not manage unschedulable K8s nodes
func detachUnschedulables(asgSvc autoscalingiface.AutoScalingAPI, k8sSvc KubernetesService, detachingInstances map[string]map[string]bool) error {
	unschedulableNodes, err := k8sSvc.getUnschedulableNodes()
	if err != nil {
		return err
	}

	unschedulableInstances := map[string]bool{}

	var newInstancesToBeDetached []string

	for _, unschedulableNode := range unschedulableNodes {
		instanceId, err := getInstanceID(unschedulableNode)
		if err != nil {
			return err
		}

		unschedulableInstances[instanceId] = true

		_, ok := detachingInstances[instanceId]
		if !ok {
			newInstancesToBeDetached = append(newInstancesToBeDetached, instanceId)
		}
	}

	// Enqueue and cache ASGs to detach the instance from, per each instance ID
	//
	// To avoid AWS API rate limiting, we do only one set of paged DescribeAutoScalingInstances requests per loop.
	// In other words, the number of AWS API calls doesn't increase proportionally to number of nodes nor ASGs.
	if len(newInstancesToBeDetached) > 0 {
		newInstanceIDToASGs, err := getIdToASGs(asgSvc, newInstancesToBeDetached)
		if err != nil {
			return err
		}

		for instanceID, asgs := range newInstanceIDToASGs {
			m := map[string]bool{}

			for _, asg := range asgs {
				m[asg] = true
			}

			detachingInstances[instanceID] = m
		}
	}

	iteration := map[string]map[string]bool{}

	for instanceID, asgs := range detachingInstances {
		iteration[instanceID] = asgs
	}

	// Keep trying to detach each instance from attached ASGs while the node is present and unschedulable
	for instanceID, asgs := range iteration {
		var asgNames []string

		for asgName := range asgs {
			asgNames = append(asgNames, asgName)
		}

		unschedulableNodeExists := unschedulableInstances[instanceID]

		// We should give up detaching instances as probably they are already gone.
		if !unschedulableNodeExists {
			delete(detachingInstances, instanceID)

			continue
		}

		for _, asgName := range asgNames {
			detached := asgs[asgName]
			if !detached {
				if err := awsDetachNodes(asgSvc, asgName, []string{instanceID}); err != nil {
					return err
				}

				asgs[asgName] = true
			}
		}
	}

	return nil
}
