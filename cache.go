package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/go-logr/logr"
	"github.com/mumoshu/node-detacher/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	NodeLabelPrefix = "node-detacher.variant.run"
)

var NodeLabelKeyCached string

func init() {
	NodeLabelKeyCached = fmt.Sprintf("%s/cached", NodeLabelPrefix)
}

type NodeAttachments struct {
	Log logr.Logger

	client   client.Client
	asgSvc   autoscalingiface.AutoScalingAPI
	elbSvc   elbiface.ELBAPI
	elbv2Svc elbv2iface.ELBV2API

	shouldHandleCLBs bool
	shouldHandleTGs  bool

	namespace string
}

func (n *NodeAttachments) Cached(node corev1.Node) bool {
	return node.Labels[NodeLabelKeyCached] == "true"
}

func (n *NodeAttachments) cacheAllNodeAttachments() error {
	nodes := corev1.NodeList{
		Items: []corev1.Node{},
	}

	if err := n.client.List(context.Background(), &nodes); err != nil {
		return err
	}

	return n.cacheNodeAttachments(nodes.Items)
}

func (n *NodeAttachments) cacheNodeAttachments(nodes []corev1.Node) error {
	nodeToInstance := map[string]string{}

	var instanceIDs []string

	for _, node := range nodes {
		if n.Cached(node) {
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
		n.Log.Info(fmt.Sprintf("%d instances has been already labeled with %q", len(nodes), NodeLabelKeyCached))

		return nil
	}

	//instanceToASGs, err := getIdToASGs(n.asgSvc, instanceIDs)
	//if err != nil {
	//	return err
	//}

	var instanceToCLBs map[string][]string

	if n.shouldHandleCLBs {
		var err error

		instanceToCLBs, err = getIdToCLBs(n.elbSvc, instanceIDs)

		if err != nil {
			return err
		}
	}

	var instanceToTDs map[string]map[string][]elbv2.TargetDescription

	if n.shouldHandleTGs {
		var err error

		_, instanceToTDs, err = getIdToTGs(n.elbv2Svc, instanceIDs)
		if err != nil {
			return err
		}
	}

	for _, node := range nodes {
		var attachment v1alpha1.Attachment

		attachment.Name = node.Name
		attachment.Namespace = n.namespace
		attachment.Spec.NodeName = node.Name

		instance := nodeToInstance[node.Name]
		//asgs := instanceToASGs[instance]
		clbs := instanceToCLBs[instance]
		tds := instanceToTDs[instance]

		ctx := context.Background()

		for arn, tds := range tds {
			for _, td := range tds {
				attachment.Spec.AwsTargets = append(attachment.Spec.AwsTargets, v1alpha1.AwsTarget{
					ARN:  arn,
					Port: td.Port,
				})
			}
		}

		for _, clb := range clbs {
			attachment.Spec.AwsLoadBalancers = append(attachment.Spec.AwsLoadBalancers, v1alpha1.AwsLoadBalancer{
				Name: clb,
			})
		}

		if err := n.client.Create(ctx, &attachment); err != nil {
			if !errors.IsAlreadyExists(err) {
				return err
			}

			var latestAttachment v1alpha1.Attachment

			if err := n.client.Get(ctx, types.NamespacedName{
				Namespace: attachment.Namespace,
				Name:      attachment.Name,
			}, &latestAttachment); err != nil {
				return err
			}

			latestAttachment.Spec = attachment.Spec

			if err := n.client.Update(ctx, &latestAttachment); err != nil {
				return err
			}
		}

		var latestNode corev1.Node

		if err := n.client.Get(ctx, types.NamespacedName{Name: node.Name}, &latestNode); err != nil {
			return err
		}

		latestNode.Labels[NodeLabelKeyCached] = "true"
		latestNode.Annotations[NodeAnnotationKeyDetaching] = "true"

		if err := n.client.Update(ctx, &latestNode); err != nil {
			return err
		}

		n.Log.Info("Sucessfully labeled node", "node", latestNode.Name)
	}

	return nil
}
