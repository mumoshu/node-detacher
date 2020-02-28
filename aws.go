package main

import (
	"fmt"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
)

func getIdToCLBs(svc elbiface.ELBAPI, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idMap := map[string]bool{}
	for _, id := range ids {
		idMap[id] = true
	}

	input := &elb.DescribeLoadBalancersInput{
	}

	clbs := []*elb.LoadBalancerDescription{}

	err := svc.DescribeLoadBalancersPages(input, func(output *elb.DescribeLoadBalancersOutput, lastPage bool) bool {
		clbs = append(clbs, output.LoadBalancerDescriptions...)

		return !lastPage
	})

	if err != nil {
		return nil, fmt.Errorf("Unable to get description for CLBs %v: %v", ids, err)
	}

	idToCLBs := map[string][]string{}

	for _, i := range clbs {
		for _, instance := range i.Instances {
			instanceID := *instance.InstanceId
			if idMap[instanceID] {
				if _, ok := idToCLBs[instanceID]; !ok {
					idToCLBs[instanceID] = []string{}
				}

				idToCLBs[instanceID] = append(idToCLBs[instanceID], *i.LoadBalancerName)
			}
		}
	}

	return idToCLBs, nil
}

func getIdToASGs(svc autoscalingiface.AutoScalingAPI, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	ec2input := &autoscaling.DescribeAutoScalingInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	}
	nodesResult, err := svc.DescribeAutoScalingInstances(ec2input)
	if err != nil {
		return nil, fmt.Errorf("Unable to get description for node %v: %v", ids, err)
	}
	if len(nodesResult.AutoScalingInstances) < 1 {
		return nil, fmt.Errorf("Did not get any autoscaling instances for %v", ids)
	}

	idToASGs := map[string][]string{}

	for _, i := range nodesResult.AutoScalingInstances {
		idToASGs[*i.InstanceId] = append(idToASGs[*i.InstanceId], *i.AutoScalingGroupName)
	}

	return idToASGs, nil
}

func getIdToTGs(svc elbv2iface.ELBV2API, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	tgInput := &elbv2.DescribeTargetGroupsInput{
	}

	tgs := []*elbv2.TargetGroup{}

	err := svc.DescribeTargetGroupsPages(tgInput, func(output *elbv2.DescribeTargetGroupsOutput, lastPage bool) bool {
		tgs = append(tgs, output.TargetGroups...)

		return !lastPage
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to get description for node %v: %v", ids, err)
	}

	idToTGs := map[string][]string{}

	for _, tg := range tgs {
		output, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
			TargetGroupArn: tg.TargetGroupArn,
		})
		if err != nil {
			return nil, err
		}

		for _, desc := range output.TargetHealthDescriptions {
			id := *desc.Target.Id

			if _, ok := idToTGs[id]; !ok {
				idToTGs[id] = []string{}
			}

			idToTGs[id] = append(idToTGs[id], *tg.TargetGroupArn)
		}
	}

	return idToTGs, nil
}

func detachInstancesFromASGs(svc autoscalingiface.AutoScalingAPI, asgName string, instanceIDs []string) error {
	input := &autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(asgName),
		InstanceIds:          aws.StringSlice(instanceIDs),
		// On manual drain we should probably keep the desired capacity unchanged(hence this should be set to `false`),
		// but for automated drains like done by Cluster Autoscaler, we should decrement it as the number of desired instances is managed by CA
		//
		// We opts to let admins handle manual drain cases on their own.
		ShouldDecrementDesiredCapacity: aws.Bool(true),
	}

	// See https://docs.aws.amazon.com/autoscaling/ec2/APIReference/API_DetachInstances.html for the API spec
	_, err := svc.DetachInstances(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {

			switch aerr.Code() {
			case autoscaling.ErrCodeResourceContentionFault:
				return fmt.Errorf("Could not detach instances, any resource is in contention, will try in next loop")
			default:
				return fmt.Errorf("Unknown aws error when detaching instances: %v", aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return fmt.Errorf("Unknown non-aws error when terminating old instance: %v", err.Error())
		}
	}
	return nil
}

func deregisterInstancesFromCLBs(svc elbiface.ELBAPI, lbName string, instanceIDs []string) error {
	instances := []*elb.Instance{}

	for _, id := range instanceIDs {
		instances = append(instances, &elb.Instance{
			InstanceId: aws.String(id),
		})
	}

	input := &elb.DeregisterInstancesFromLoadBalancerInput{
		Instances:        instances,
		LoadBalancerName: aws.String(lbName),
	}

	// See https://docs.aws.amazon.com/autoscaling/ec2/APIReference/API_DetachInstances.html for the API spec
	_, err := svc.DeregisterInstancesFromLoadBalancer(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {

			switch aerr.Code() {
			case autoscaling.ErrCodeResourceContentionFault:
				return fmt.Errorf("Could not deregister instances, any resource is in contention, will try in next loop")
			default:
				return fmt.Errorf("Unknown aws error when deregistering instances: %v", aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return fmt.Errorf("Unknown non-aws error when deregistering instances: %v", err.Error())
		}
	}
	return nil
}

func deregisterInstancesFromTGs(svc elbv2iface.ELBV2API, tgName string, instanceIDs []string) error {
	descs := []*elbv2.TargetDescription{}

	for _, id := range instanceIDs {
		descs = append(descs, &elbv2.TargetDescription{
			Id: aws.String(id),
		})
	}

	input := &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgName),
		Targets:        descs,
	}

	// See https://docs.aws.amazon.com/autoscaling/ec2/APIReference/API_DetachInstances.html for the API spec
	_, err := svc.DeregisterTargets(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {

			switch aerr.Code() {
			case autoscaling.ErrCodeResourceContentionFault:
				return fmt.Errorf("Could not deregister targets, any resource is in contention, will try in next loop")
			default:
				return fmt.Errorf("Unknown aws error when deregistering targets: %v", aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return fmt.Errorf("Unknown non-aws error when deregistering targets: %v", err.Error())
		}
	}
	return nil
}

func awsGetServices() (autoscalingiface.AutoScalingAPI, elbiface.ELBAPI, elbv2iface.ELBV2API, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, nil, nil, err
	}
	asgSvc := autoscaling.New(sess)
	elbSvc := elb.New(sess)
	elbv2Svc := elbv2.New(sess)
	return asgSvc, elbSvc, elbv2Svc, nil
}
