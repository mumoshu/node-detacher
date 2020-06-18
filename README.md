# node-detacher

`node-detacher` is a Kubernetes controller that detects and detaches unschedulable or to-be-terminated nodes from external load balancers.

## Why?

it is a stop-gap for Kubernetes' inability to "wait" for the traffic from external load balancers to "immediately" stop flowing before the node is finally scheduled for termination.

As the node gets detached before even the pod termination grace period begins, your load balancer gains more time to gracefully stop traffic.

Have you ever tried the combination of `externalTrafficPolicy: Local` and `healthCheckNodePort` and configured your load balancer to have shorter grace period than your pods, for high availability on node roll? With `node-detacher`, your load balancer doesn't even need to wait for several health checks before the detachment starts. It starts detaching immediately, so that you can usually set the same length of grace periods for both `healthCheckNodePort` and the load balancer.

It should always be useful whenever you expose your pods and nodes via node ports and external load balancers.

External load balancers can be anything, from ALBs managed by `aws-alb-ingress-controller`, ELBs managed by `type: LoadBalancer` services, `NodePort` and provisions ELBs outside of Kubernetes with e.g. Terraform or CloudFormation.

`node-detacher` avoids "short" downtime after the EC2 instance is terminated and before load balancers finally stops sending traffic.

Note that the length of downtime can theoretically depend on the cloud provier, as cloud providers like AWS are usually eventual consistent.

## Use-cases

`node-detacher` complements the following to keep your production services highly available and reliable:

- Graceful daemonset pod stop on scale down
- [aws-node-termination-handler](#aws-node-termination-handler)
- [aws-aws-roller](#aws-aws-roller)
- [cluster-autoscaler](#cluster-autoscaler)
- [node-problem-detector + draino](#node-problem-detector-and-draino)
- [ingress controllers like contour](#ingress-controllers)
- [`type: LadBalancer` services](#type-loadbalancer-services)
- [`type: NodePort` services]($type-nodeport-services)

### Graceful daemonset pod stop on scale down

There're two target use-case here, including: 

1. Scale down by Cluster Autoscaler
  - CA doesn't gracefully stop daemonset pods on scale down
2. Scale down by node-termination-handler variant

The first use-case is important. Without `node-detacher` in place, there's no way to e.g. gracefully stop fluentd to wait for buffer flush on CA scale down.

The second use-case is also important, because draining node before (AWS spot|GCP preemptive instance) termination notice handler doesn't fully cover it.

[GCP/k8s-node-termination-handler](https://github.com/GoogleCloudPlatform/k8s-node-termination-handler) works relatively nice, but it still misses ability to control the deletion order within user pods and system pods.

What if you have `istio` and `fluentd` in respective namespaces `istio-system` and `logging`, where your fluentd doesn't depend on `istio` and hence you want `istio` to be deleted earlier than `fluentd` so that `fluentd` can flush all the logs from `istio` until it shuts down? No way. But configuring the termination handler to not drain but cordon the node and adding `node-detacher` in your cluster gives you the flexible deletion order.  

[awslabs/ec2-spot-labs](https://github.com/awslabs/ec2-spot-labs) just skip deleting daemonset pods. `node-detacher` can help you by deleting daemonset pods after the termination handler deleted other pods.

[aws/aws-node-termination-handler](https://github.com/aws/aws-node-termination-handler/blob/0d528d632f8d3e9712da0fe678fc11240acc2fa4/pkg/node/node.go#L408) supports deleting daemonset pods by setting `--ignore-daemon-sets=false`(See [this issue](https://github.com/aws/aws-node-termination-handler/issues/20)), but as similar as the GCP solution, it doesn't allow granular control of deletion order. `node-detacher` gives that.

Give your pods deletion priorities via integer values in pod annotation `node-detacher.variant.run/deletion-priority`. `node-detacher` deletes pods in the descending order of priorities, regardless of the pod is managed by DeamonSet or anything else(Deployment, Statefulset, and so on).

### [`aws-node-termination-handelr`](https://github.com/aws/aws-node-termination-handler)

Avoid potential downtime due to node termination.
 
- The termination handler cordons the node(=marks the node "unschedulable") on termination notice.
- But with `exetrnalTrafficPolicy: Cluster`, the loadbalancer keep flowing trafiic to node's NodePort until it finally terminates, which incur a little downtime
- With `externalTrafficPolicy: Local` with properly configured `healthCheckNodePort` and a health-check endpoint provided the your application, the loadbalancer is unable to stop traffic flowing until the node finally terminates.
- The detacher detects the cordoned node and detaches it far before termination, so that there is less downtime

### [`aws-asg-roller`](https://github.com/deitch/aws-asg-roller)

Avoid potential downtime due to ELB not reacting to node termination fast enough.

The targeted scenario is the same as that for aws-node-termination-handler above.

### `cluster-autoscaler`

Avoid downtime on scale down.

The targeted scenario is the same as that for aws-node-termination-handler above.

With this application in place, the overall node shutdown process with Cluster Autocaler or any other Kubernetes operator that involves terminating nodes would look like this:

- Cluster Autoscaler starts draining the node (on scale down)
  - It [adds a `ToBeDeletedByClusterAutoscaler` taint](https://github.com/kubernetes/autoscaler/blob/912d923484b826b6986046405d243f9083ceb764/cluster-autoscaler/utils/deletetaint/delete.go#L59-L62) to the node to tell the scheduler to stop scheduling new pods onto the node.
- `node-detacher` detects the node become unschedulable, so it tries to detach the node from corresponding ASGs ASAP.
- ELB(s) still gradually stop directing the traffic to the nodes. The backend Kubernetes service and pods will starst to receive less and less traffic.
- ELB(s) stops directing traffic as the EC2 instances are detached. Application processes running inside pods can safely terminates

### [`node-problem-detector`](https://github.com/kubernetes/node-problem-detector) and [draino](https://github.com/planetlabs/draino)

Use-case: Avoid downtime on drain

- `node-problem-detector` detects various node problems and marks the problematic nodes in node conditions. (Also see [uswitch's prebuilt rules](https://github.com/uswitch/node-problem-detector) for more detection rules)
- `draino` detects node conditions and drains the nodes. See See https://github.com/kubernetes/node-problem-detector#remedy-systems.
- `node-detacher` detects and deregisters drained(cordoned) nodes.

Notes:

- `draino` makes the problematic node unschedulable by [setting `spec.Unschedulable` to `true`](https://github.com/planetlabs/draino/blob/a877e7f0852fc510e74f416bcce7e8569e213141/internal/kubernetes/drainer.go#L148-L162)
- Pods being evicted by `draino` gradually stops receiving traffic through the problematic node's NodePorts, before passing several load balancer health checks, as `node-detacher` detaches the node from load balancers.
- The node gets terminated after pods got terminated after grace period. As the pods are already terminated and the node is not receiving traffic from LBs, it incurs no downtime.

### `Ingress Controllers`

Use-case: Avoid downtime on node drain/termination

- An ingress controller like [contour](https://github.com/projectcontour/contour) is often deployed as a daemonset with NodePort with `externalTrafficPolicy: Local` and hostPort.
- When a rolling-update on the daemonset begins, `node-detacher` detaches the node where the `Terminating` pod is running, which prevents downtime

### `type: LoadBalancer` services

`node-detacher` allows you to gracefully terminate your nodes without down time due to that `cluster-autoscaler` and `draino` and other Kubernetes controllers and operators are doesn't interoprate with ELBs which is necessary for `externalTrafficPolicy: Local` services.

### `type: NodePort` services

`node-detacher` allows you to gracefully terminate your nodes without down time due to that `cluster-autoscaler` and `draino` and other Kubernetes controllers and operators are doesn't interoprate with ELBs which is necessary for `externalTrafficPolicy: Local` services.

## FAQ

Here's the set of common questions that may provide you better understanding of where `node-detacher` is helpful.

> Why prefer `NodePort` over `LoadBalancer` type services in the first place?
>
> `NodePort` allows you to:
>
> - Avoid recreating ELB/ALB/NLB when you recreate the Kubernetes cluster
>   - There's no need to pre-warn your ELB before switching huge production traffic from the old to the new cluster anymore.
>   - There's no need to wait for DNS to propagate changes in your endpoint that directs the traffic to the LB anymore.

> Why `externalTrafficPolicy: Local`?
>
> It removes an extra hop between the node received the packet on NodePort, and the node that is running the backend pod.

> Why not use `aws-alb-ingress-controller` with the `IP` target mode that directs the traffic to directory to the `aws-vpc-cni-k8s`-managed Pod/ENI IP?
>
> That's because they are prone to sudden failure of pods. When a pod is failed while running, you need to wait for ELB until it finishes a few healthcecks and finally mark te pod IP unhealthy until the traffic stops flowing into the failed pod.

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

### For Ingress DaemonSet Pods

- On `Pod` resource change...
- Is the pod managed by the target daemonset?
  - No -> Exit this loop.
- Detach the node the terminating pod is running
  - (The same algorithm for nodes explained above)

### Deleting DaemonSet pods on scale down

Once `node-detacher` finds any node that became unschedulable, it:

1. Queries daemonsets and their pods scheduled onto the node
2. Sort the pods in the decreasing order of `node-detacher.variant.run/pod-deletion-priority` annotation value as specified in the owner daemonset
3. Deletes pods in the order

To avoid the pod deleted in the step 3 resurrected by K8s, your daemonset pod should MUST NOT have a toleration against `node-detacher.variant.run/detaching`.

## Requirements

**IAM Permissions**:

When running on AWS and you want ELB integration to work,`node-detacher` needs access to certain resources and actions.

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
Usage of ./node-detacher:
  -daemonset [NAMESPACE/]NAME
    	Specifies target daemonsets to be processed by node-detacher. Used only when either -manage-daemonsets or -manage-daemonset-pods is enabled. This flag can be specified multiple times to target two or more daemonsets.
    	Example: --daemonsets contour --daemonsets anotherns/nginx-ingress ([NAMESPACE/]NAME)
  -enable-alb-ingress-integration [true|false]
    	Enable aws-alb-ingress-controller integration
    	Possible values are [true|false] (default true)
  -enable-aws
    	Enable AWS support including ELB v1, ELB v2(target group) integrations. Also specify enable-(static|dynamic)(alb|clb|nlb)-integration flags for detailed configuration (default true)
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
  -log-level string
    	Log level. Must be one of debug, info, warn, error (default "info")
  -manage-daemonset-pods --daemonsets
    	Detaches the node when one of the daemonset pods on the pod started terminating. Also specify --daemonsets or annotate daemonsets with node-detaher.variant.run/managed-by=NAME
  -manage-daemonsets
    	Detaches the node one by one when the targeted daemonset with RollingUpdate.Policy set to OnDelete became OUTDATED. Also specify --daemonsets to limit the daemonsets which triggers rolls, or annotate daemonsets with node-detacher.variant.run/managed-by=NAME
  -master --kubeconfig
    	(Deprecated: switch to --kubeconfig) The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.
  -metrics-addr string
    	The address the metric endpoint binds to. (default ":8080")
  -name string
    	NAME of this node-detacher, used to distinguish one of node-detacher instances and specified in the annotation node-detacher.variant.run/managed-by (default "node-detacher")
  -namespace string
    	NAMESPACE to watch resources for
  -sync-period duration
    	The period in seconds between each forceful iteration over all the nodes (default 10s)
```

For production deployment with standard usage, you'll usually use the following set of flags:

```
node-detacher -enable-leader-election
```

This gives you:

- High-availability of node-detacher (with replicas >= 2)
- ELB v1/v2 integration
- Ordered deletion of daemonset pods before node termination

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
