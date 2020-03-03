# node-detacher

`node-detacher` is a Kubernetes controller that watches for `Unschedulable` nodes and immediately detach them from the corresponding `AutoScaling Groups` before they, and their pods, go offline.

This is generally useful when you expose your nodes via `NodePort` and provisions ELBs outside of Kubernetes with e.g. Terraform or CloudFormation.

## Why NodePort?

Why prefer `NodePort` over `LoadBalancer` type services in the first place?

`NodePort` allows you to:

- Avoid recreating ELB/ALB/NLB when you recreate the Kubernetes cluster
  - There's no need to pre-warn your ELB before switching huge production traffic from the old to the new cluster anymore.
  - There's no need to wait for DNS to propagate changes in your endpoint that directs the traffic to the LB anymore.

Why not use `aws-alb-ingress-controller` with the `IP` target mode that directs the traffic to directory to the `aws-vpc-cni-k8s`-managed Pod/ENI IP?

That's because they are prone to sudden failure of pods. When a pod is failed while running, you need to wait for ELB until it finishes a few healthcecks and finally mark te pod IP unhealthy until the traffic stops flowing into the failed pod.

## Why `node-detacher`?

This is a stop-gap for Kubernetes' inability to "wait" for the traffic from ELB/ALB/NLB to stop before the node is finally scheduled for termination.

### `node-detacher` with Cluster Autoscaler

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

## Algorithm

`node-detacher` runs the following steps in a control loop:

- On `Node` resource change -
- Is the node exists?
  - No -> The node is already terminated. We have nothing to do no matter if it's properly detached from LBs or not. Exit this loop.
- (Only in the static mode) If not yet done, label the node with target group ARNs and optionally target ports, and/or CLBs
  - For targets without port overrides, it uses the label `tg.node-detacher.variant.run/<target-group-arn>`
  - For targets with port overrides, it uses the label `tg.node-detacher.variant.run/<target-group-arn>/<port number>`
  - For CLBs, it uses the label `clb.node-detacher.variant.run/<load balancer name>`
- Is the node is unschedulable?
  - i.e. Does it have a `ToBeDeletedByClusterAutoscaler` taint, or `node.spec.Unschedulable` set to `true`?
  - No -> The node is not scheduled for termination. Exit this loop.
- Is the node has condition `NodeBeingDetached` set to `True`?
  - Yes -> The node is already scheduled for detachment/deregistration. All we need is to hold on and wish the node to properly deregistered from LBs in time. Ecit the loop.
- Deregister the node from target groups or CLBs
  - Deregister the node from the target group specified by `tg.node-detacher.variant.run/<target-group-arn>` label
  - Call `DeregisterInstancesFromLoadBalancer` API for the loadbalancer specified by `clb.node-detacher.variant.run/<load balancer name>` label
- Set the node condition `NodeBeingDetached` to `True`, so that in the next loop we won't duplicate the work of de-registering the node

## Recommended Usage

- Run [`aws-asg-roller`](https://github.com/deitch/aws-asg-roller) along with `node-detacher` in order to avoid potential downtime due to ELB not reacting to node termination fast enough
- Run `cluster-autoscaler` along with `node-detacher` in order to avoid downtime on scale down
- Run [`node-problem-detector`](https://github.com/kubernetes/node-problem-detector) to detect unhealthy nodes and [draino](https://github.com/planetlabs/draino) to automatically drain such nodes. Add `node-detacher` so that node termination triggered by `draino` doesn' result in downtime. See https://github.com/kubernetes/node-problem-detector#remedy-systems
  - Optionally add more node-problem-detector rules by referencing [uswitch's prebuilt rules](https://github.com/uswitch/node-problem-detector)

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
                "autoscaling:DescribeAutoScalingInstances",
                "autoscaling:DetachInstances",
            ],
            "Resource": "*"
        }
    ]
}
```

Please consult the AWS documentation for `IAM Role for Pods` in order to provide those permissions via a pod IAM role.

It isn't recommended but you can alternatively create an IAM user and set `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` envvars to provide the permissions.

## Deployment

`node-detacher` is available as a docker image. To run on a machine that is external to your Kubernetes cluster:

```
docker run -d mumoshu/node-detacher:<version>
```

To run in Kubernetes:

```yml
apiVersion: core/v1
kind: ServiceAccount
metadata:
  name: node-detacher
  labels:
    name: node-detacher
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1beta1
kind: ClusterRole
metadata:
  name: node-detacher
  labels:
    name: node-detacher
rules:
  - apiGroups:
      - "*"
    resources:
      - "*"
    verbs:
      - get
      - list
      - watch
  - apiGroups:
      - "*"
    resources:
      - nodes
    verbs:
      - get
      - list
      - watch
---
apiVersion: rbac.authorization.k8s.io/v1beta1
kind: ClusterRoleBinding
metadata:
  name: node-detacher
  labels:
    name: node-detacher
roleRef:
  kind: ClusterRole
  name: node-detacher
  apiGroup: rbac.authorization.k8s.io
subjects:
  - kind: ServiceAccount
    name: node-detacher
    namespace: kube-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: node-detacher
  labels:
    name: node-detacher
  namespace: kube-system
spec:
  replicas: 1
  template:
    metadata:
      labels:
        name: node-detacher
    spec:
      containers:
      - name: node-detacher
        # Remove this `envFrom` field when you rely on the node or pod IAM role
        envFrom:
        - secretRef:
            name: aws-node-detacher
        image: 'mumoshu/node-detacher'
        imagePullPolicy: Always
      restartPolicy: Always
      serviceAccountName: node-detacher
      # to allow it to run on master
      tolerations:
        - effect: NoSchedule
          operator: Exists
      # we specifically want to run on master - remove the remaining lines if you do not care where it runns
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                - key: kubernetes.io/role
                  operator: In
                  values: ["master"]
```

## Configuration

`node-detacher` takes its configuration via command-line flags:

```console
Usage of node-detacher:
  -enable-alb-ingress-integration
    	Enable aws-alb-ingress-controller integration (default true)
  -enable-dynamic-clb-integration type: LoadBalancer
    	Enable integration with classical load balancers (a.k.a ELB v1) managed by type: LoadBalancer services (default true)
  -enable-dynamic-nlb-integration type: LoadBalancer
    	Enable integration with network load balancers (a.k.a ELB v2 NLB) managed by type: LoadBalancer services (default true)
  -enable-leader-election
    	Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.
  -enable-static-clb-integration
    	Enable integration with classical load balancers (a.k.a ELB v1) managed externally to Kubernetes, e.g. by Terraform or CloudFormation (default true)
  -enable-static-tg-integration
    	Enable integration with application load balancers and network load balancers (a.k.a ELB v2 ALBs and NLBs) managed externally to Kubernetes, e.g. by Terraform or CloudFormation (default true)
  -kubeconfig string
    	Paths to a kubeconfig. Only required if out-of-cluster.
  -master --kubeconfig
    	(Deprecated: switch to --kubeconfig) The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.
  -metrics-addr string
    	The address the metric endpoint binds to. (default ":8080")
  -sync-period duration
    	The period in seconds between each forceful iteration over all the nodes (default 10s)
```

## Build Instruction

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

## Test instruction

For Docker for Mac:

```
$ k label node docker-desktop alpha.eksctl.io/instance-id=foobar
$ go run .
```

## Acknowledgements

The initial version of `node-detacher` is proudly based on @deitch's [aws-asg-roller](https://github.com/deitch/aws-asg-roller) which serves a different use-case. Big kudos to @deitch for authoring and sharing the great product! This product wouldn't have born without it.
