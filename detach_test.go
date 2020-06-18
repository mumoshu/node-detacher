package main

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/autoscaling"
)

type testKubernetesService struct {
	unscheduableNodes []corev1.Node
	err               error
}

func (t *testKubernetesService) getUnschedulableNodes() ([]corev1.Node, error) {
	return t.unscheduableNodes, t.err
}

func TestLoop(t *testing.T) {
	tests := []struct {
		desc                    string
		asgs                    []string
		handler                 KubernetesService
		err                     error
		oldIds                  map[string][]string
		newIds                  map[string][]string
		asgOriginalDesired      map[string]int64
		originalDesired         map[string]map[string]bool
		newOriginalDesired      map[string]int64
		newDesired              map[string]int64
		expectedOriginalDesired map[string]int64
		terminate               []string
	}{
		{
			"2 asgs adjust in progress",
			[]string{"myasg", "anotherasg"},
			nil,
			nil,
			map[string][]string{
				"myasg":      {"1"},
				"anotherasg": {},
			},
			map[string][]string{
				"myasg":      {"2", "3"},
				"anotherasg": {"8", "9", "10"},
			},
			map[string]int64{"myasg": 2, "anotherasg": 10},
			map[string]map[string]bool{"node1": map[string]bool{"myasg": true}},
			map[string]int64{"myasg": 2, "anotherasg": 0},
			map[string]int64{"myasg": 2},
			map[string]int64{"myasg": 2, "anotherasg": 0},
			[]string{"1"},
		},
		{
			"2 asgs adjust first run",
			[]string{"myasg", "anotherasg"},
			nil,
			nil,
			map[string][]string{
				"myasg":      {"1"},
				"anotherasg": {},
			},
			map[string][]string{
				"myasg":      {"2", "3"},
				"anotherasg": {"8", "9", "10"},
			},
			map[string]int64{"myasg": 2},
			map[string]map[string]bool{},
			map[string]int64{"myasg": 2},
			map[string]int64{"myasg": 3},
			map[string]int64{"myasg": 2},
			[]string{},
		},
	}

	for i, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			validGroups := map[string]*autoscaling.Group{}
			for _, n := range tt.asgs {
				name := n
				lcName := "lconfig"
				oldLcName := fmt.Sprintf("old%s", lcName)
				myHealthy := healthy
				desired := tt.asgOriginalDesired[name]
				instances := make([]*autoscaling.Instance, 0)
				for _, id := range tt.oldIds[name] {
					idd := id
					instances = append(instances, &autoscaling.Instance{
						InstanceId:              &idd,
						LaunchConfigurationName: &oldLcName,
						HealthStatus:            &myHealthy,
					})
				}
				for _, id := range tt.newIds[name] {
					idd := id
					instances = append(instances, &autoscaling.Instance{
						InstanceId:              &idd,
						LaunchConfigurationName: &lcName,
						HealthStatus:            &myHealthy,
					})
				}
				// construct the Group we will pass
				validGroups[n] = &autoscaling.Group{
					AutoScalingGroupName:    &name,
					DesiredCapacity:         &desired,
					Instances:               instances,
					LaunchConfigurationName: &lcName,
				}
			}
			asgSvc := &mockAsgSvc{
				groups: validGroups,
			}

			elbSvc := &mockElbSvc{}

			elbv2Svc := &mockELbV2Svc{}

			err := detachUnschedulables(asgSvc, elbSvc, elbv2Svc, tt.handler, tt.originalDesired, tt.originalDesired, tt.originalDesired)
			// what were our last calls to each?
			switch {
			case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
				t.Errorf("%d: mismatched errors, actual then expected", i)
				t.Logf("%v", err)
				t.Logf("%v", tt.err)
			case !testStringInt64MapEq(tt.newOriginalDesired, tt.expectedOriginalDesired):
				t.Errorf("%d: Mismatched desired, actual then expected", i)
				t.Logf("%v", tt.originalDesired)
				t.Logf("%v", tt.newOriginalDesired)
			}

			// check each svc with its correct calls
			desiredCalls := asgSvc.counter.filterByName("SetDesiredCapacity")
			if len(desiredCalls) != len(tt.newDesired) {
				t.Errorf("%d: Expected %d SetDesiredCapacity calls but had %d", i, len(tt.newDesired), len(desiredCalls))
			}
			// sort through by the relevant inputs
			for _, d := range desiredCalls {
				asg := d.params[0].(*autoscaling.SetDesiredCapacityInput)
				name := asg.AutoScalingGroupName
				if *asg.DesiredCapacity != tt.newDesired[*name] {
					t.Errorf("%d: Mismatched call to set capacity for ASG '%s': actual %d, expected %d", i, *name, *asg.DesiredCapacity, tt.newDesired[*name])
				}
			}
			// convert list of terminations into map
			ids := map[string]bool{}
			for _, id := range tt.terminate {
				ids[id] = true
			}
			terminateCalls := asgSvc.counter.filterByName("TerminateInstanceInAutoScalingGroup")
			if len(terminateCalls) != len(tt.terminate) {
				t.Errorf("%d: Expected %d Terminate calls but had %d", i, len(tt.terminate), len(terminateCalls))
			}
			for _, d := range terminateCalls {
				in := d.params[0].(*autoscaling.TerminateInstanceInAutoScalingGroupInput)
				id := in.InstanceId
				if _, ok := ids[*id]; !ok {
					t.Errorf("%d: Requested call to terminate instance %s, unexpected", i, *id)
				}
			}

		})
	}
}
