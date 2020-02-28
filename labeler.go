package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NodeLabelPrefix = "node-detacher.variant.run"
)

func labelAllNodes(client client.Client, asgSvc autoscalingiface.AutoScalingAPI, elbSvc elbiface.ELBAPI, elbv2Svc elbv2iface.ELBV2API) error {
	nodes := corev1.NodeList{
		Items: []corev1.Node{},
	}

	if err := client.List(context.Background(), &nodes); err != nil {
		return err
	}

	return labelNodes(client, asgSvc, elbSvc, elbv2Svc, nodes.Items)
}

func labelNodes(client client.Client, asgSvc autoscalingiface.AutoScalingAPI, elbSvc elbiface.ELBAPI, elbv2Svc elbv2iface.ELBV2API, nodes []corev1.Node) error {
	nodeToInstance := map[string]string{}

	var instanceIDs []string

	for _, node := range nodes {
		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		nodeToInstance[node.Name] = instanceId

		instanceIDs = append(instanceIDs, instanceId)
	}

	instanceToASGs, err := getIdToASGs(asgSvc, instanceIDs)
	if err != nil {
		return err
	}

	instanceToCLBs, err := getIdToCLBs(elbSvc, instanceIDs)
	if err != nil {
		return err
	}

	instanceToTGs, err := getIdToTGs(elbv2Svc, instanceIDs)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		instance := nodeToInstance[node.Name]
		asgs := instanceToASGs[instance]
		clbs := instanceToCLBs[instance]
		tgs := instanceToTGs[instance]

		var latest corev1.Node

		ctx := context.Background()

		if err := client.Get(ctx, types.NamespacedName{Name: node.Name}, &latest); err != nil {
			return err
		}

		for _, asg := range asgs {
			latest.Labels[fmt.Sprintf("asg.%s/%s", NodeLabelPrefix, asg)] = ""
		}

		for _, tg := range tgs {
			latest.Labels[fmt.Sprintf("tg.%s/%s", NodeLabelPrefix, tg)] = ""
		}

		for _, clb := range clbs {
			latest.Labels[fmt.Sprintf("clb.%s/%s", NodeLabelPrefix, clb)] = ""
		}

		latest.Labels[fmt.Sprintf("%s/labeled", NodeLabelPrefix)] = "true"

		if err := client.Update(ctx, &latest); err != nil {
			return err
		}
	}

	return nil
}
