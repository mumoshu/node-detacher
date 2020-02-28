package main

import (
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	corev1 "k8s.io/api/core/v1"
)

const (
	healthy = "Healthy"
)

// detachUnschedulables runs a set of EC2 instance detachments in the loop to update ASGs to not manage unschedulable K8s nodes
func detachUnschedulables(asgSvc autoscalingiface.AutoScalingAPI, elbSvc elbiface.ELBAPI, elbv2Svc elbv2iface.ELBV2API, k8sSvc KubernetesService, detachingInstances map[string]map[string]bool, deregisteringInstances map[string]map[string]bool, deregisteringCLBInstances map[string]map[string]bool) error {
	unschedulableNodes, err := k8sSvc.getUnschedulableNodes()
	if err != nil {
		return err
	}

	return detachNodes(asgSvc, elbSvc, elbv2Svc, unschedulableNodes, detachingInstances, deregisteringInstances, deregisteringCLBInstances)
}

func detachNodes(asgSvc autoscalingiface.AutoScalingAPI, elbSvc elbiface.ELBAPI, elbv2Svc elbv2iface.ELBV2API, unschedulableNodes []corev1.Node, instanceASGDetachments map[string]map[string]bool, instanceTGDetachments map[string]map[string]bool, instanceCLBDetachments map[string]map[string]bool) error {
	unschedulableInstances := map[string]bool{}

	instanceToNode := map[string]corev1.Node{}

	var newInstancesToBeDetached []string

	for _, unschedulableNode := range unschedulableNodes {
		instanceId, err := getInstanceID(unschedulableNode)
		if err != nil {
			return err
		}

		instanceToNode[instanceId] = unschedulableNode

		unschedulableInstances[instanceId] = true

		_, ok := instanceASGDetachments[instanceId]
		if !ok {
			newInstancesToBeDetached = append(newInstancesToBeDetached, instanceId)
		}
	}

	// Enqueue and cache ASGs to detach the instance from, per each instance ID
	//
	// To avoid AWS API rate limiting, we do only one set of paged DescribeAutoScalingInstances requests per loop.
	// In other words, the number of AWS API calls doesn't increase proportionally to number of nodes nor ASGs.
	if len(newInstancesToBeDetached) > 0 {
		// ASGs

		newInstanceIDToASGs, err := getIdToASGs(asgSvc, newInstancesToBeDetached)
		if err != nil {
			return err
		}

		for instanceID, asgs := range newInstanceIDToASGs {
			m := map[string]bool{}

			for _, asg := range asgs {
				m[asg] = true
			}

			instanceASGDetachments[instanceID] = m
		}

		// CLBs

		newInstanceIDToCLBs, err := getIdToCLBs(elbSvc, newInstancesToBeDetached)
		if err != nil {
			return err
		}

		for instanceID, tgs := range newInstanceIDToCLBs {
			m := map[string]bool{}

			for _, tg := range tgs {
				m[tg] = true
			}

			instanceCLBDetachments[instanceID] = m
		}

		// Target Groups

		newInstanceIDToTGs, err := getIdToTGs(elbv2Svc, newInstancesToBeDetached)
		if err != nil {
			return err
		}

		for instanceID, tgs := range newInstanceIDToTGs {
			m := map[string]bool{}

			for _, tg := range tgs {
				m[tg] = true
			}

			instanceTGDetachments[instanceID] = m
		}
	}

	// AutoScaling Groups

	instToASGs := map[string]map[string]bool{}

	for instanceID, asgs := range instanceASGDetachments {
		instToASGs[instanceID] = asgs
	}

	// Keep trying to detach each instance from attached ASGs while the node is present and unschedulable
ASG:
	for instanceID, asgs := range instToASGs {
		node := instanceToNode[instanceID]

		for _, taint := range node.Spec.Taints {
			// Detaching from ASGs prevents cluster-autoscaler to be unable to track the progress of terminating the node
			// We avoid that by skipping detachments from ASGs on our own and let CA do it.
			// Even though we don't deal with ASGs, we do detach and deregister from TGs and CLBs so you can still
			// benefit from zero downtime termination.
			//
			// We detect that this node is being terminated by CA by looking into the specific node taint that is
			// added by CA before turning the node into Unschedulable.
			// See https://github.com/kubernetes/autoscaler/blob/912d923484b826b6986046405d243f9083ceb764/cluster-autoscaler/utils/deletetaint/delete.go#L36
			if taint.Key == "ToBeDeletedByClusterAutoscaler" {
				continue ASG
			}
		}

		var asgNames []string

		for asgName := range asgs {
			asgNames = append(asgNames, asgName)
		}

		unschedulableNodeExists := unschedulableInstances[instanceID]

		// We should give up detaching instances as probably they are already gone.
		if !unschedulableNodeExists {
			delete(instanceASGDetachments, instanceID)

			continue
		}

		for _, asgName := range asgNames {
			detached := asgs[asgName]
			if !detached {
				if err := detachInstancesFromASGs(asgSvc, asgName, []string{instanceID}); err != nil {
					return err
				}

				asgs[asgName] = true
			}
		}
	}

	// CLBs

	instToCLBs := map[string]map[string]bool{}

	for instanceID, clbs := range instanceCLBDetachments {
		instToCLBs[instanceID] = clbs
	}

	// Keep trying to detach each instance from attached Clbs while the node is present and unschedulable
	for instanceID, clbs := range instToCLBs {
		var clbNames []string

		for clbName := range clbs {
			clbNames = append(clbNames, clbName)
		}

		unschedulableNodeExists := unschedulableInstances[instanceID]

		// We should give up detaching instances as probably they are already gone.
		if !unschedulableNodeExists {
			delete(instanceCLBDetachments, instanceID)

			continue
		}

		for _, clbName := range clbNames {
			detached := clbs[clbName]
			if !detached {
				if err := deregisterInstancesFromCLBs(elbSvc, clbName, []string{instanceID}); err != nil {
					return err
				}

				clbs[clbName] = true
			}
		}
	}

	// Target Groups

	instToTGs := map[string]map[string]bool{}

	for instanceID, tgs := range instanceTGDetachments {
		instToTGs[instanceID] = tgs
	}

	// Keep trying to detach each instance from attached Tgs while the node is present and unschedulable
	for instanceID, tgs := range instToTGs {
		var tgNames []string

		for tgName := range tgs {
			tgNames = append(tgNames, tgName)
		}

		unschedulableNodeExists := unschedulableInstances[instanceID]

		// We should give up detaching instances as probably they are already gone.
		if !unschedulableNodeExists {
			delete(instanceTGDetachments, instanceID)

			continue
		}

		for _, tgName := range tgNames {
			detached := tgs[tgName]
			if !detached {
				if err := deregisterInstancesFromTGs(elbv2Svc, tgName, []string{instanceID}); err != nil {
					return err
				}

				tgs[tgName] = true
			}
		}
	}

	return nil
}
