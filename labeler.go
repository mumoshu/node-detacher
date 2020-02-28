package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NodeLabelPrefix = "node-detacher.variant.run"
)

var KeyLabeled string

func init() {
	KeyLabeled = fmt.Sprintf("%s/labeled", NodeLabelPrefix)
}

type Nodes struct {
	Log logr.Logger

	client   client.Client
	asgSvc   autoscalingiface.AutoScalingAPI
	elbSvc   elbiface.ELBAPI
	elbv2Svc elbv2iface.ELBV2API
	k8sSvc   KubernetesService
}

func (n *Nodes) labelAllNodes() error {
	nodes := corev1.NodeList{
		Items: []corev1.Node{},
	}

	if err := n.client.List(context.Background(), &nodes); err != nil {
		return err
	}

	return n.labelNodes(nodes.Items)
}

func (n *Nodes) labelNodes(nodes []corev1.Node) error {
	nodeToInstance := map[string]string{}

	var instanceIDs []string

	for _, node := range nodes {
		if node.Labels[KeyLabeled] == "true" {
			continue
		}

		instanceId, err := getInstanceID(node)
		if err != nil {
			return err
		}

		nodeToInstance[node.Name] = instanceId

		instanceIDs = append(instanceIDs, instanceId)
	}

	if len(instanceIDs) == 0 {
		n.Log.Info(fmt.Sprintf("%d instances has been already labeled with %q", len(nodes), KeyLabeled))

		return nil
	}

	instanceToASGs, err := getIdToASGs(n.asgSvc, instanceIDs)
	if err != nil {
		return err
	}

	instanceToCLBs, err := getIdToCLBs(n.elbSvc, instanceIDs)
	if err != nil {
		return err
	}

	instanceToTGs, err := getIdToTGs(n.elbv2Svc, instanceIDs)
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

		if err := n.client.Get(ctx, types.NamespacedName{Name: node.Name}, &latest); err != nil {
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

		latest.Labels[KeyLabeled] = "true"

		if err := n.client.Update(ctx, &latest); err != nil {
			return err
		}
	}

	return nil
}
