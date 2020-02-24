package main

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
)

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

func awsDetachNodes(svc autoscalingiface.AutoScalingAPI, asgName string, instanceIDs []string) error {
	input := &autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(asgName),
		InstanceIds: aws.StringSlice(instanceIDs),
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

func awsGetServices() (autoscalingiface.AutoScalingAPI, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, err
	}
	asgSvc := autoscaling.New(sess)
	return asgSvc, nil
}
