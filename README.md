# node-detacher

`node-detacher` is a Kubernetes controller that watches for unschedulable nodes and immediately detach them from the corresponding target groups and classical load balancers before they, and their pods, go offline.

## Recommended Usage

`node-detacher` complements famous and useful solutions listed below: 

- Run [`aws-node-termination-handelr](https://github.com/aws/aws-node-termination-handler) along with `node-detacher`. The termination handler cordons the node(=marks the node "unschedulable") on termination notice. The detacher detects the cordoned node and detaches it before termination.
- Run [`aws-asg-roller`](https://github.com/deitch/aws-asg-roller) along with `node-detacher` in order to avoid potential downtime due to ELB not reacting to node termination fast enough
- Run `cluster-autoscaler` along with `node-detacher` in order to avoid downtime on scale down
- Run [`node-problem-detector`](https://github.com/kubernetes/node-problem-detector) to detect unhealthy nodes and [draino](https://github.com/planetlabs/draino) to automatically drain such nodes. Add `node-detacher` so that node termination triggered by `draino` doesn' result in downtime. See https://github.com/kubernetes/node-problem-detector#remedy-systems
  - Optionally add more node-problem-detector rules by referencing [uswitch's prebuilt rules](https://github.com/uswitch/node-problem-detector)
- Ingress controller like [contour](https://github.com/projectcontour/contour) as a daemonset. When a rolling-update on the daemonset began, `node-detacher` immediately start detaching the node where the `Terminating` pod is running, which reduces downtime due to rolling-update.

## Why `node-detacher`?

This is a stop-gap for Kubernetes' inability to "wait" for the traffic from ELB/ALB/NLB to stop before the node is finally scheduled for termination.

It is generally useful when you expose your nodes via

- ALBs managed by `aws-alb-ingress-controller`
- ELBs managed by `type: LoadBalancer` services
- `NodePort` and provisions ELBs outside of Kubernetes with e.g. Terraform or CloudFormation

In this case, `node-detacher` avoids short(but depends on the situation as AWS is eventual consistent :) ) downtime after the EC2 instance is terminated and before ELB(s) finally stops sending traffic.

It is even more useful when you run any TCP server as DaemonSet behind services whose `externalTrafficPolicy` is set to `Local`.
In this case, `node-detacher` avoids downtime after all the daemonset pods on the node terminated and before ELB(s) finally stops sending traffic to the node.

### FAQ

Here's the set of common questions that may provide you better understanding of where `node-detacher` is helpful.

> Why `externalTrafficPolicy: Local`?
>
> It removes an extra hop between the node received the packet on NodePort, and the node that is running the backend pod.

> Why not use `aws-alb-ingress-controller` with the `IP` target mode that directs the traffic to directory to the `aws-vpc-cni-k8s`-managed Pod/ENI IP?
>
> That's because they are prone to sudden failure of pods. When a pod is failed while running, you need to wait for ELB until it finishes a few healthcecks and finally mark te pod IP unhealthy until the traffic stops flowing into the failed pod.

### With Cluster Autoscaler

With this application in place, the overall node shutdown process with Cluster Autocaler or any other Kubernetes operator that involves terminating nodes would look like this:

- Cluster Autoscaler starts draining the node (on scale down)
  - It [adds a `ToBeDeletedByClusterAutoscaler` taint](https://github.com/kubernetes/autoscaler/blob/912d923484b826b6986046405d243f9083ceb764/cluster-autoscaler/utils/deletetaint/delete.go#L59-L62) to the node to tell the scheduler to stop scheduling new pods onto the node.
- `node-detacher` detects the node become unschedulable, so it tries to detach the node from corresponding ASGs ASAP.
- ELB(s) still gradually stop directing the traffic to the nodes. The backend Kubernetes service and pods will starst to receive less and less traffic.
- ELB(s) stops directing traffic as the EC2 instances are detached. Application processes running inside pods can safely terminates

### With `draino`

- `node-problem-detector` marks a node as problematic.
- `draino` makes the problematic node unschedulable by [setting `spec.Unschedulable` to `true`](https://github.com/planetlabs/draino/blob/a877e7f0852fc510e74f416bcce7e8569e213141/internal/kubernetes/drainer.go#L148-L162).
- `node-detacher` detaches the node from corresponding load balancers.
- Pods being evicted by `draino` gradually stops receiving traffic through the problematic node's NodePorts
- Pod grace period passes and pods gets terminated.
- The node gets terminated. As the pods are already terminated and the node is not receiving traffic from LBs, it incurs no downtime.

### With `type: LoadBalancer` services

`node-detacher` allows you to gracefully terminate your nodes without down time due to that `cluster-autoscaler` and `draino` and other Kubernetes controllers and operators are doesn't interoprate with ELBs which is necessary for `externalTrafficPolicy: Local` services.

### With `type: NodePort` services

`node-detacher` allows you to gracefully terminate your nodes without down time due to that `cluster-autoscaler` and `draino` and other Kubernetes controllers and operators are doesn't interoprate with ELBs which is necessary for `externalTrafficPolicy: Local` services.

> Why prefer `NodePort` over `LoadBalancer` type services in the first place?
>
> `NodePort` allows you to:
>
> - Avoid recreating ELB/ALB/NLB when you recreate the Kubernetes cluster
>   - There's no need to pre-warn your ELB before switching huge production traffic from the old to the new cluster anymore.
>   - There's no need to wait for DNS to propagate changes in your endpoint that directs the traffic to the LB anymore.

## Algorithm

`node-detacher` runs the following steps in a control loop:

### For Nodes

- Watch for Kubernetes `Node` resource change
- Does the node still exist?
  - No
    - Description: The node is already terminated. It may have properly detached from LBs by `node-detacher`, or may not. But we have nothing to do at this point.
    - Action: Exit this loop.
- (Only in the static mode) If not yet done, cache the target group ARNs and ports, and/or CLBs associated to the node.
  - See the definition of `node-detacher.variant.run/Attachment` custom resource for more information and the data structure.
- Is the node already being detached?
  - i.e. Does the node have a condition `NodeBeingDetached=True` or an annotation `node-detacher.variant.run/detaching=true`?
- Is the node is unschedulable?
  - i.e. Does it have a `ToBeDeletedByClusterAutoscaler` taint, or `node.spec.Unschedulable` set to `true`?
- Is the node being detached AND is schedulable?
  - Yes
    - Description: The node was scheduled for detachment, but it is now schedublale.
    - Action: Re-attach the node to target groups and CLBs. Then exit the loop.
- Is the node being detached AND is unschedulable?
  - Yes
    - Description: The node is already scheduled for detachment/deregistration. All we need is to hold on and wish the node to properly deregistered from LBs in time
    - Action: Exit the loop.
- Is the node schedulable?
  - Yes
    - Description: The node is not scheduled for termination
    - Action: Exit this loop.
- (At this point, we know that the node is not being detaching AND is unschedulable)
- Deregister the node from target groups or CLBs
  - Deregister the node from the target group specified by `attachment.spec.awsTargets[]`.
  - Deregister the node from the CLBs specified by `attachment.spec.awsLoadBalancers[]`
- Mark the node as "being detached"
  - So that in the next loop we won't duplicate the work of de-registering the node
  - More concretely, set the node condition `NodeBeingDetached=True` and a node annotation `node-detacher.variant.run/detaching=true`

For caching node target groups and CLBs, `node-detacher` uses a specific Kubernetes custom resource.

- For target group targets, it uses `spec.awsTargets[].arn` and `spec.awsTargets[].port`
- For CLBs, it uses `spec.awsLoadBalancers[].name`

### For DaemonSet Pods

- On `Pod` resource change...
- Is the pod managed by the target daemonset?
  - No -> Exit this loop.
- Detach the node the terminating pod is running
  - (The same algorithm for nodes explained above)

## Requirements

**IAM Permissions**:

`node-detacher` need access to certain resources and actions.

Please provide the following policy document the IAM user or role used by the pods running `node-detacher`:

```
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "elasticloadbalancing:DescribeLoadBalancers",
                "elasticloadbalancing:RegisterInstancesWithLoadBalancer",
                "elasticloadbalancing:DeregisterInstancesFromLoadBalancer",
                "elasticloadbalancing:DescribeTargetGroups",
                "elasticloadbalancing:DescribeTargetHealth",
                "elasticloadbalancing:RegisterTargets",
                "elasticloadbalancing:DeregisterTargets"
            ],
            "Resource": "*"
        }
    ]
}
```

## Deployment

`node-detacher` is available as a docker image. To run on a machine that is external to your Kubernetes cluster:

```console
docker run -d mumoshu/node-detacher:<version>
```

To run in Kubernetes:

```console
make deploy
```

`make deploy` calls `kustomize` and `kubectl apply` under the hood:

```console
cd config/manager && kustomize edit set image controller=${NAME}:${VERSION}
kustomize build config/default | kubectl apply -f -
```

For testing purpose, the image can be just `mumoshu/node-detacher:latest` so that the `kustomize edit` command would look like:

```console
kustomize edit set image controller=mumoshu/node-detacher:latest
```

## Running on AWS

If you're trying to use IAM roles for Pods,
try using `eksctl` to fully configure your cluster:

```
$ cat <<EOC > cluster.yaml
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig

metadata:
  name: ${CLUSTER_NAME}
  region: us-east-2

nodeGroups:
  - name: mynodegroup
    desiredCapacity: 2
    minSize: 2
    maxSize: 12
    instancesDistribution:
      maxPrice: 0.08
      instanceTypes:
        - c5.xlarge
        - c4.xlarge
      onDemandBaseCapacity: 0
      onDemandPercentageAboveBaseCapacity: 50
      spotInstancePools: 4
    volumeSize: 100
    kubeletExtraConfig:
      cpuCFSQuota: false
    iam:
      withAddonPolicies:
        imageBuilder: true
        autoScaler: true
      attachPolicyARNs:
        - arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy
        - arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy
        - arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCor\
e  # https://github.com/mumoshu/kube-ssm-agent
        - arn:aws:iam::${ACCOUNT_ID}:policy/node-detacher
EOC

$ eksctl create cluster -f cluster.yaml

$ make deploy

$ eksctl utils associate-iam-oidc-provider \
  --cluster ${CLUSTER} \
  --approve

$ eksctl create iamserviceaccount \
  --override-existing-serviceaccounts \
  --cluster ${CLUSTER} \
  --namespace node-detacher-system \
  --name default \
  --attach-policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/node-detacher \
  --approve

# Once done, remove the cfn stack for iamserviceaccount

$ eksctl delete iamserviceaccount \
  --cluster ${CLUSTER} \
  --namespace node-detacher-system \
  --name default
```

Please consult the AWS documentation for `IAM Role for Pods` in order to provide those permissions via a pod IAM role.

It isn't recommended but you can alternatively create an IAM user and set `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` envvars to provide the permissions.


## Configuration

`node-detacher` takes its configuration via command-line flags:

```console
Usage of node-detacher:
  -daemonset [NAMESPACE/]NAME
    	Enable daemonset integration by specifying target daemonset(s). This flag can be specified multiple times to target two or more daemonsets.
    	Example: --daemonsets contour --daemonsets anotherns/nginx-ingress ([NAMESPACE/]NAME)
  -enable-alb-ingress-integration [true|false]
    	Enable aws-alb-ingress-controller integration
    	Possible values are [true|false] (default true)
  -enable-dynamic-clb-integration [true|false]
    	Enable integration with classical load balancers (a.k.a ELB v1) managed by "type: LoadBalancer" services
    	Possible values are [true|false] (default true)
  -enable-dynamic-nlb-integration [true|false]
    	Enable integration with network load balancers (a.k.a ELB v2 NLB) managed by "type: LoadBalancer" services
    	Possible values are [true|false] (default true)
  -enable-leader-election
    	Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.
  -enable-static-clb-integration [true|false]
    	Enable integration with classical load balancers (a.k.a ELB v1) managed externally to Kubernetes, e.g. by Terraform or CloudFormation
    	Possible values are [true|false] (default true)
  -enable-static-tg-integration [true|false]
    	Enable integration with application load balancers and network load balancers (a.k.a ELB v2 ALBs and NLBs) managed externally to Kubernetes, e.g. by Terraform or CloudFormation.
    	Possible values are [true|false] (default true)
  -kubeconfig string
    	Paths to a kubeconfig. Only required if out-of-cluster.
  -master --kubeconfig
    	(Deprecated: switch to --kubeconfig) The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.
  -metrics-addr string
    	The address the metric endpoint binds to. (default ":8080")
  -sync-period duration
    	The period in seconds between each forceful iteration over all the nodes (default 10s)
```

## Contributing

`node-detacher` currently supports only Kubernetes on AWS.

### Add support for more cloud providers

If you'd like to make this work on another cloud, I'm more than happy to discuss, review and accept pull requests.

When you're adding a support for another cloud provider, I'd also appreciate if you could give me cloud credit or a test account for the cloud provider, so that I can test it myself.

## Developing

If you'd like contributing to this project, please refer to the below guide for test and build instructions.

### Build Instruction

The only pre-requisite for building is [docker](https://docker.com). All builds take place inside a docker container. If you want, you _may_ build locally using locally installed go. It requires go version 1.13+.

To build:

```sh
$ make build      # builds the binary via docker in `bin/node-detacher-${OS}-${ARCH}
$ make image      # builds the docker image
```

To build locally:

```sh
$ make build BUILD=local     # builds the binary via locally installed go in `bin/node-detacher-${OS}-${ARCH}
$ make image BUILD=local     # builds the docker image
```

### Test instruction

For Docker for Mac:

```
$ k label node docker-desktop alpha.eksctl.io/instance-id=foobar
$ go run .
```

To easily test the cluster-autoscaler support, use `kubectl taint`:

```console
# Tainting the node the CA's taint key would triggger node detachment

$ k taint node ip-192-168-8-195.us-east-2.compute.internal ToBeDeletedByClusterAutoscaler=:NoSchedule

# Removing the taint triggers re-attachment of the node

$ k untaint node ip-192-168-8-195.us-east-2.compute.internal ToBeDeletedByClusterAutoscaler=:NoSchedule
```

To easily test the draino support, use `kubectl cordon` and `uncordon`:

```
# Cordoning the node results in setting node.spec.unschedulable=true that triggers node detachment

$ k cordon ip-192-168-8-195.us-east-2.compute.internal

# Uncordnoning the node results in setting node.spec.unschedulable=false that triggers node reattachment

$ k uncordon ip-192-168-8-195.us-east-2.compute.internal
```

## Acknowledgements

The initial version of `node-detacher` is proudly based on @deitch's [aws-asg-roller](https://github.com/deitch/aws-asg-roller) which serves a different use-case. Big kudos to @deitch for authoring and sharing the great product! This product wouldn't have born without it.
