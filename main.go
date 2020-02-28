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

	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		syncPeriod           time.Duration
		metricsAddr          string
		enableLeaderElection bool
	)

	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Second, "The period in seconds between each forceful iteration over all the nodes")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.Parse()

	ctrl.SetLogger(zap.New(func(o *zap.Options) {
		o.Development = true
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

	nodeReconciler := &NodeReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Runner"),
		Scheme: mgr.GetScheme(),
		nodes: &Nodes{
			Log:      ctrl.Log.WithName("models").WithName("Nodes"),
			client:   mgr.GetClient(),
			asgSvc:   asgSvc,
			elbSvc:   elbSvc,
			elbv2Svc: elbv2Svc,
		},
		detachingNodes:        map[string]map[string]bool{},
		deregisteringNodes:    map[string]map[string]bool{},
		deregisteringNodesCLB: map[string]map[string]bool{},
	}

	if err = nodeReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
