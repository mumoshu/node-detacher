/*
Copyright 2020 The node-detacher authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"github.com/mumoshu/node-detacher/api/v1alpha1"
	zap2 "go.uber.org/zap"
	"k8s.io/klog"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = v1alpha1.AddToScheme(scheme)

	// +kubebuilder:scaffold:scheme
}

type StringSlice []string

func (ss *StringSlice) String() string {
	return fmt.Sprintf("%v", *ss)
}
func (ss *StringSlice) Set(v string) error {
	*ss = append(*ss, v)
	return nil
}

func main() {
	// Prevents the following error when fsGroup is set to 65534 for pod iam roles:
	//   I0309 12:01:54.222632       1 leaderelection.go:241] attempting to acquire leader lease  node-detacher-system/controller-leader-election-helper...
	//   log: exiting because of error: log: cannot create log: open /tmp/manager.controller-manager-5f7bd48566-mzkgz.unknownuser.log.INFO.20200309-120154.1: no such file or directory
	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	klogFlags.Set("logtostderr", "true")
	klogFlags.Parse([]string{})

	var (
		syncPeriod           time.Duration
		metricsAddr          string
		enableLeaderElection bool

		albIngress  bool
		dynamicCLBs bool
		dynamicNLBs bool
		staticCLBs  bool
		staticTGs   bool

		daemonsets StringSlice

		manageDaemonSets    bool
		manageDaemonSetPods bool

		name string

		namespace string

		logLevel string
	)

	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Second, "The period in seconds between each forceful iteration over all the nodes")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&albIngress, "enable-alb-ingress-integration", true,
		"Enable aws-alb-ingress-controller integration\nPossible values are `[true|false]`",
	)
	flag.BoolVar(&dynamicCLBs, "enable-dynamic-clb-integration", true,
		"Enable integration with classical load balancers (a.k.a ELB v1) managed by \"type: LoadBalancer\" services\nPossible values are `[true|false]`",
	)
	flag.BoolVar(&dynamicNLBs, "enable-dynamic-nlb-integration", true,
		"Enable integration with network load balancers (a.k.a ELB v2 NLB) managed by \"type: LoadBalancer\" services\nPossible values are `[true|false]`",
	)
	flag.BoolVar(&staticCLBs, "enable-static-clb-integration", true,
		"Enable integration with classical load balancers (a.k.a ELB v1) managed externally to Kubernetes, e.g. by Terraform or CloudFormation\nPossible values are `[true|false]`",
	)
	flag.BoolVar(&staticTGs, "enable-static-tg-integration", true,
		"Enable integration with application load balancers and network load balancers (a.k.a ELB v2 ALBs and NLBs) managed externally to Kubernetes, e.g. by Terraform or CloudFormation.\nPossible values are `[true|false]`")
	flag.Var(&daemonsets, "daemonset", "Specifies target daemonsets to be processed by node-detacher. Used only when either -manage-daemonsets or -manage-daemonset-pods is enabled. This flag can be specified multiple times to target two or more daemonsets.\nExample: --daemonsets contour --daemonsets anotherns/nginx-ingress (`[NAMESPACE/]NAME`)")
	flag.BoolVar(&manageDaemonSetPods, "manage-daemonset-pods", false,
		"Detaches the node when one of the daemonset pods on the pod started terminating. Also specify `--daemonsets` or annotate daemonsets with node-detaher.variant.run/managed-by=NAME")
	flag.BoolVar(&manageDaemonSets, "manage-daemonsets", false,
		"Detaches the node one by one when the targeted daemonset with RollingUpdate.Policy set to OnDelete became OUTDATED. Also specify --daemonsets to limit the daemonsets which triggers rolls, or annotate daemonsets with node-detacher.variant.run/managed-by=NAME")
	flag.StringVar(&name, "name", "node-detacher", "NAME of this node-detacher, used to distinguish one of node-detacher instances and specified in the annotation node-detacher.variant.run/managed-by")
	flag.StringVar(&namespace, "namespace", "", "NAMESPACE to watch resources for")
	flag.StringVar(&logLevel, "log-level", "info", "Log level. Must be one of debug, info, warn, error")
	flag.Parse()

	ctrl.SetLogger(zap.New(func(o *zap.Options) {
		o.Development = true
		lvl := zap2.NewAtomicLevelAt(stringToZapLogLevel(logLevel))
		o.Level = &lvl
	}))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		LeaderElection:     enableLeaderElection,
		SyncPeriod:         &syncPeriod,
		Port:               9443,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// get the AWS sessions
	asgSvc, elbSvc, elbv2Svc, err := awsGetServices()
	if err != nil {
		setupLog.Error(err, "Unable to create an AWS session")
		os.Exit(1)
	}

	ns := os.Getenv("POD_NAMESPACE")

	if os.Getenv("WATCH_NAMESPACE") != "" {
		ns = os.Getenv("WATCH_NAMESPACE")
	}

	if namespace != "" {
		ns = namespace
	}

	nodeController := NodeController{
		Name:                                name,
		Client:                              mgr.GetClient(),
		Log:                                 ctrl.Log.WithName("controllers").WithName("Node"),
		Scheme:                              mgr.GetScheme(),
		ALBIngressIntegrationEnabled:        albIngress,
		DynamicNLBIntegrationEnabled:        dynamicNLBs,
		DynamicCLBIntegrationEnabled:        dynamicCLBs,
		StaticTargetGroupIntegrationEnabled: staticTGs,
		StaticCLBIntegrationEnabled:         staticCLBs,
		Namespace:                           ns,
		asgSvc:                              asgSvc,
		elbSvc:                              elbSvc,
		elbv2Svc:                            elbv2Svc,
	}

	if err = nodeController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	// Our daemonsets support has the ability to mark outdated daemonset's pods to be detached.
	// This requires the daemonset pod reconciler to be enabled, hence this block enables the daemonset pod reconciler
	// when only the daemonset reconciler is explicitly required.
	if manageDaemonSets || manageDaemonSetPods {
		podController := PodController{
			Name:       name,
			Client:     mgr.GetClient(),
			Log:        ctrl.Log.WithName("controllers").WithName("Pod"),
			DaemonSets: daemonsets,
			Namespace:  ns,
		}

		if err = podController.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Pod")
			os.Exit(1)
		}
	}

	if manageDaemonSets {
		daemonsetController := DaemonsetController{
			Name:       name,
			Client:     mgr.GetClient(),
			Log:        ctrl.Log.WithName("controllers").WithName("DaemonSet"),
			DaemonSets: daemonsets,
			Namespace:  ns,
		}

		if err = daemonsetController.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Pod")
			os.Exit(1)
		}
	}

	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
