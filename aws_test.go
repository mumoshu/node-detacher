package main

import (
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"testing"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
)

type mockElbSvc struct {
	elbiface.ELBAPI
	err     error
	counter funcCounter
}

type mockELbV2Svc struct {
	elbv2iface.ELBV2API
	err     error
	counter funcCounter
}

type mockAsgSvc struct {
	autoscalingiface.AutoScalingAPI
	err     error
	counter funcCounter
	groups  map[string]*autoscaling.Group
}

func (m *mockAsgSvc) TerminateInstanceInAutoScalingGroup(in *autoscaling.TerminateInstanceInAutoScalingGroupInput) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	m.counter.add("TerminateInstanceInAutoScalingGroup", in)
	ret := &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}
	return ret, m.err
}
func (m *mockAsgSvc) DescribeAutoScalingGroups(in *autoscaling.DescribeAutoScalingGroupsInput) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	m.counter.add("DescribeAutoScalingGroups", in)
	groups := make([]*autoscaling.Group, 0)
	for _, n := range in.AutoScalingGroupNames {
		if group, ok := m.groups[*n]; ok {
			groups = append(groups, group)
		}
	}
	return &autoscaling.DescribeAutoScalingGroupsOutput{
		AutoScalingGroups: groups,
	}, m.err
}
func (m *mockAsgSvc) SetDesiredCapacity(in *autoscaling.SetDesiredCapacityInput) (*autoscaling.SetDesiredCapacityOutput, error) {
	m.counter.add("SetDesiredCapacity", in)
	ret := &autoscaling.SetDesiredCapacityOutput{}
	return ret, m.err
}

func TestAwsGetServices(t *testing.T) {
	asg, elb, elbv2, err := awsGetServices()
	if err != nil {
		t.Fatalf("Unexpected err %v", err)
	}
	if asg == nil {
		t.Fatalf("asg unexpectedly nil")
	}
	if elb == nil {
		t.Fatalf("elb unexpectedly nil")
	}
	if elbv2 == nil {
		t.Fatalf("elbv2 unexpectedly nil")
	}
}
