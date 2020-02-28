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

With this application in place, the overall node shutdown process with Cluster Autocaler or any other Kubernetes operator that involves terminating nodes would look like this:

- Cluster Autoscaler starts draining the node (on scale down). `Node.Spec.Unschdulable` is set to `true`.
- `node-detacher` detects the node become `Unschedulable`, so it tries to detach the node from corresponding ASGs ASAP.
- ELB(s) still gradually stop directing the traffic to the nodes. The backend Kubernetes service and pods will starst to receive less and less traffic.
- ELB(s) stops directing traffic as the EC2 instances are detached. Application processes running inside pods can safely terminates

## Recommended Usage

- Run `aws-asg-roller` along with `node-detacher` in order to avoid potential downtime due to ELB not reacting to node termination fast enough
- Run `cluster-autoscaler` along with `node-detacher` in order to avoid downtime on scale down
- Run `node-problem-detector` to detect unhealthy nodes and [draino](https://github.com/planetlabs/draino) to automatically drain such nodes. Add `node-detacher` so that node termination triggered by `draino` doesn' result in downtime. See https://github.com/kubernetes/node-problem-detector#remedy-systems
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

`node-detacher` takes its configuration via environment variables. All environment variables that affect ASG Roller begin with `ROLLER_`.

* `RESYNC_INTERNAL`: Seconds between syncs.

* `VERBOSE`: If set to `true`, will increase verbosity of logs.

* `KUBECONFIG`: Path to kubernetes config file for authenticating to the kubernetes cluster. Required only if `ROLLER_KUBERNETES` is `true` and we are not operating in a kubernetes cluster. If omitted, in-cluster configuration is used.

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

## Acknowledgements

The initial version of `node-detacher` is proudly based on @deitch's [aws-asg-roller](https://github.com/deitch/aws-asg-roller) which serves a different use-case. Big kudos to @deitch for authoring and sharing the great product! This product wouldn't have born without it.
