/*
Copyright 2022 The Kubeflow Authors.

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

/*
Katib-controller is a controller (operator) for Experiments and Trials
*/
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	configv1beta1 "github.com/kubeflow/katib/pkg/apis/config/v1beta1"
	apis "github.com/kubeflow/katib/pkg/apis/controller"
	cert "github.com/kubeflow/katib/pkg/certgenerator/v1beta1"
	"github.com/kubeflow/katib/pkg/controller.v1beta1"
	"github.com/kubeflow/katib/pkg/controller.v1beta1/consts"
	"github.com/kubeflow/katib/pkg/util/v1beta1/katibconfig"
	webhookv1beta1 "github.com/kubeflow/katib/pkg/webhook/v1beta1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	scheme = runtime.NewScheme()
	log    = logf.Log.WithName("entrypoint")
)

func init() {
	utilruntime.Must(apis.AddToScheme(scheme))
	utilruntime.Must(configv1beta1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	logf.SetLogger(zap.New())

	var katibConfigFile string
	flag.StringVar(&katibConfigFile, "katib-config", "",
		"The katib-controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. ")
	flag.Parse()

	initConfig, err := katibconfig.GetInitConfigData(scheme, katibConfigFile)
	if err != nil {
		log.Error(err, "Failed to get KatibConfig")
		os.Exit(1)
	}

	// Set the config in viper.
	viper.Set(consts.ConfigExperimentSuggestionName, initConfig.ControllerConfig.ExperimentSuggestionName)
	viper.Set(consts.ConfigInjectSecurityContext, initConfig.ControllerConfig.InjectSecurityContext)
	viper.Set(consts.ConfigEnableGRPCProbeInSuggestion, initConfig.ControllerConfig.EnableGRPCProbeInSuggestion)

	trialGVKs, err := katibconfig.TrialResourcesToGVKs(initConfig.ControllerConfig.TrialResources)
	if err != nil {
		log.Error(err, "Failed to parse trialResources")
		os.Exit(1)
	}
	viper.Set(consts.ConfigTrialResources, trialGVKs)

	log.Info("Config:",
		consts.ConfigExperimentSuggestionName,
		viper.GetString(consts.ConfigExperimentSuggestionName),
		"webhook-port",
		initConfig.ControllerConfig.WebhookPort,
		"metrics-addr",
		initConfig.ControllerConfig.MetricsAddr,
		"healthz-addr",
		initConfig.ControllerConfig.HealthzAddr,
		consts.ConfigInjectSecurityContext,
		viper.GetBool(consts.ConfigInjectSecurityContext),
		consts.ConfigEnableGRPCProbeInSuggestion,
		viper.GetBool(consts.ConfigEnableGRPCProbeInSuggestion),
		"trial-resources",
		viper.Get(consts.ConfigTrialResources),
	)

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "Fail to get the config")
		os.Exit(1)
	}

	// Create a new katib controller to provide shared dependencies and start components
	mgr, err := manager.New(cfg, manager.Options{
		Metrics: metricsserver.Options{
			BindAddress: initConfig.ControllerConfig.MetricsAddr,
		},
		HealthProbeBindAddress: initConfig.ControllerConfig.HealthzAddr,
		LeaderElection:         initConfig.ControllerConfig.EnableLeaderElection,
		LeaderElectionID:       initConfig.ControllerConfig.LeaderElectionID,
		Scheme:                 scheme,
	})
	if err != nil {
		log.Error(err, "Failed to create the manager")
		os.Exit(1)
	}

	log.Info("Registering Components.")

	// Create a webhook server.
	hookServer := webhook.NewServer(webhook.Options{
		Port:    *initConfig.ControllerConfig.WebhookPort,
		CertDir: consts.CertDir,
	})

	if err := mgr.AddReadyzCheck("webhook", hookServer.StartedChecker()); err != nil {
		log.Error(err, "Unable to add readyz webhook cert")
		os.Exit(1)
	}

	if initConfig.CertGeneratorConfig.Enable {
		certsReady := make(chan struct{})
		err := mgr.AddReadyzCheck("cert", func(_ *http.Request) error {
			select {
			case <-certsReady:
				return nil
			default:
				return fmt.Errorf("cert not ready")
			}
		})
		if err != nil {
			log.Error(err, "Unable to add readyz check cert")
			os.Exit(1)
		}

		if err := cert.AddToManager(mgr, initConfig.CertGeneratorConfig, certsReady); err != nil {
			log.Error(err, "Failed to set up cert-generator")
			os.Exit(1)
		}

		go func() {
			// The certsReady blocks to register controllers until generated certs.
			<-certsReady
			log.Info("Certs ready")
			setupControllers(mgr, hookServer)
		}()
	} else {
		setupControllers(mgr, hookServer)
	}

	log.Info("Setting up health checker.")
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Add webhook server health checker to the manager failed")
		os.Exit(1)
	}

	// Start the Cmd
	log.Info("Starting the manager.")
	ctx := signals.SetupSignalHandler()
	if err = mgr.Start(ctx); err != nil {
		log.Error(err, "Unable to run the manager")
		os.Exit(1)
	}
}

// setupControllers will register controllers to the manager
// after generated certs for the admission webhooks.
func setupControllers(mgr manager.Manager, hookServer webhook.Server) {
	// Setup all Controllers
	log.Info("Setting up controller.")
	if err := controller.AddToManager(mgr); err != nil {
		log.Error(err, "Unable to register controllers to the manager")
		os.Exit(1)
	}

	log.Info("Setting up webhooks.")
	if err := webhookv1beta1.AddToManager(mgr, hookServer); err != nil {
		log.Error(err, "Unable to register webhooks to the manager")
		os.Exit(1)
	}
}
