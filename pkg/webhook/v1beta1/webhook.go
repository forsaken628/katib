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

package webhook

import (
	"context"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configv1beta1 "github.com/kubeflow/katib/pkg/apis/config/v1beta1"
	cert "github.com/kubeflow/katib/pkg/certgenerator/v1beta1"
	"github.com/kubeflow/katib/pkg/webhook/v1beta1/experiment"
	"github.com/kubeflow/katib/pkg/webhook/v1beta1/pod"
)

func AddToManager(mgr manager.Manager, hookServer webhook.Server, certsCfg configv1beta1.CertGeneratorConfig) error {
	log := mgr.GetLogger()

	certsReady := make(chan struct{})
	if certsCfg.Enable {
		err := mgr.AddReadyzCheck("cert", func(_ *http.Request) error {
			select {
			case <-certsReady:
				return nil
			default:
				return fmt.Errorf("cert not ready")
			}
		})
		if err != nil {
			return err
		}
		err = cert.AddToManager(mgr, certsCfg, certsReady)
		if err != nil {
			log.Error(err, "Failed to set up cert-generator")
			return err
		}
	} else {
		close(certsReady)
	}

	decoder := admission.NewDecoder(mgr.GetScheme())
	experimentValidator := experiment.NewExperimentValidator(mgr.GetClient(), decoder)
	experimentDefaulter := experiment.NewExperimentDefaulter(mgr.GetClient(), decoder)
	sidecarInjector := pod.NewSidecarInjector(mgr.GetClient(), decoder)

	hookServer.Register("/validate-experiment", &webhook.Admission{Handler: experimentValidator})
	hookServer.Register("/mutate-experiment", &webhook.Admission{Handler: experimentDefaulter})
	hookServer.Register("/mutate-pod", &webhook.Admission{Handler: sidecarInjector})

	if err := mgr.AddReadyzCheck("webhook", hookServer.StartedChecker()); err != nil {
		log.Error(err, "Unable to add readyz endpoint to the manager")
		return err
	}

	err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-certsReady:
			return hookServer.Start(ctx)
		}
	}))
	if err != nil {
		return fmt.Errorf("add webhook server to the manager failed: %v", err)
	}
	return nil
}
