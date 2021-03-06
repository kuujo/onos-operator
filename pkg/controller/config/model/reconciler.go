// Copyright 2019-present Open Networking Foundation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"context"
	"fmt"
	"github.com/onosproject/onos-api/go/onos/configmodel"
	"github.com/onosproject/onos-lib-go/pkg/errors"
	"github.com/onosproject/onos-lib-go/pkg/logging"
	"github.com/onosproject/onos-operator/pkg/apis/config/v1beta1"
	"github.com/onosproject/onos-operator/pkg/controller/config/registry"
	"github.com/onosproject/onos-operator/pkg/controller/config/util"
	"github.com/onosproject/onos-operator/pkg/controller/util/grpc"
	"github.com/onosproject/onos-operator/pkg/controller/util/k8s"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"time"
)

var log = logging.GetLogger("controller", "config", "model")

const configFinalizer = "config"
const retryDuration = time.Second

// Add creates a new Database controller and adds it to the Manager. The Manager will set fields on the
// controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r := &Reconciler{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		config: mgr.GetConfig(),
	}

	// Create a new controller
	c, err := controller.New("config-model-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Model
	err = c.Watch(&source.Kind{Type: &v1beta1.Model{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Reconcile all Models when a Model is changed
	err = c.Watch(&source.Kind{Type: &v1beta1.Model{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: &modelMapper{mgr.GetClient()},
	})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Pod
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: &modelMapper{mgr.GetClient()},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &Reconciler{}

// Reconciler reconciles a Model object
type Reconciler struct {
	client client.Client
	scheme *runtime.Scheme
	config *rest.Config
}

// Reconcile reads that state of the cluster for a Model object and makes changes based on the state read
// and what is in the Model.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Infof("Reconciling Model %s/%s", request.Namespace, request.Name)

	// Fetch the Model instance
	model := &v1beta1.Model{}
	err := r.client.Get(context.TODO(), request.NamespacedName, model)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if model.DeletionTimestamp == nil {
		return r.reconcileCreate(model)
	}
	return r.reconcileDelete(model)
}

func (r *Reconciler) reconcileCreate(model *v1beta1.Model) (reconcile.Result, error) {
	// If the model does not define a plugin configuration, it doesn't need to be injected into any pods
	if model.Spec.Plugin == nil {
		return reconcile.Result{}, nil
	}

	// Add the finalizer to the model if necessary
	if !k8s.HasFinalizer(model, configFinalizer) {
		log.Debugf("Adding '%s' finalizer to Model '%s/%s'", configFinalizer, model.Namespace, model.Name)
		k8s.AddFinalizer(model, configFinalizer)
		err := r.client.Update(context.TODO(), model)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Find all pods into which the model can be injected
	pods := &corev1.PodList{}
	podOpts := &client.ListOptions{
		Namespace: model.Namespace,
	}
	if err := r.client.List(context.TODO(), pods, podOpts); err != nil {
		return reconcile.Result{}, err
	}

	// Install the model to each registry
	requeue := false
	for _, pod := range pods.Items {
		if pod.Annotations[registry.RegistryInjectStatusAnnotation] == registry.RegistryInjectStatusInjected {
			var podIndex int
			var podStatus *v1beta1.RegistryStatus
			var foundStatus v1beta1.RegistryStatus
			for i, reg := range model.Status.RegistryStatuses {
				if reg.PodName == pod.Name {
					podIndex = i
					foundStatus = reg
					podStatus = &foundStatus
					break
				}
			}

			if podStatus == nil {
				log.Debugf("Initializing Model '%s/%s' status for Pod '%s'", model.Namespace, model.Name, pod.Name)
				podStatus = &v1beta1.RegistryStatus{
					PodName: pod.Name,
					Phase:   v1beta1.ModelPending,
				}
				model.Status.RegistryStatuses = append(model.Status.RegistryStatuses, *podStatus)
				if err := r.client.Status().Update(context.TODO(), model); err != nil {
					log.Warnf("Failed to update status for Model '%s/%s': %s", model.Namespace, model.Name, err)
					return reconcile.Result{}, err
				}
				return reconcile.Result{}, nil
			}

			switch podStatus.Phase {
			case v1beta1.ModelPending:
				if pod.Status.PodIP != "" {
					ready := true
					for _, containerStatus := range pod.Status.ContainerStatuses {
						if containerStatus.Name == "model-registry" && !containerStatus.Ready {
							ready = false
						}
					}
					if !ready {
						requeue = true
						continue
					}
					log.Debugf("Installing Model '%s/%s' into Pod '%s' registry", model.Namespace, model.Name, pod.Name)
					model.Status.RegistryStatuses[podIndex] = v1beta1.RegistryStatus{
						PodName: pod.Name,
						Phase:   v1beta1.ModelInstalling,
					}
					if err := r.client.Status().Update(context.TODO(), model); err != nil {
						log.Warnf("Failed to update status for Model '%s/%s': %s", model.Namespace, model.Name, err)
						return reconcile.Result{}, err
					}
					return reconcile.Result{}, nil
				}
			case v1beta1.ModelInstalling:
				ready := true
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.Name == "model-registry" && !containerStatus.Ready {
						ready = false
					}
				}
				if !ready {
					requeue = true
					continue
				}
				conn, err := grpc.ConnectAddress(fmt.Sprintf("%s:5151", pod.Status.PodIP))
				if err != nil {
					return reconcile.Result{}, err
				}
				defer conn.Close()
				client := configmodel.NewConfigModelRegistryServiceClient(conn)
				modules := make([]*configmodel.ConfigModule, 0)
				for _, module := range model.Spec.Modules {
					modules = append(modules, &configmodel.ConfigModule{
						Name:         module.Name,
						Organization: module.Organization,
						Revision:     module.Revision,
						File:         util.NormalizeFileName(module.File),
					})
				}

				files := make(map[string]string)
				for name, data := range model.Spec.Files {
					files[util.NormalizeFileName(name)] = data
				}

				var getStateMode configmodel.GetStateMode
				switch model.Spec.Plugin.GetStateMode {
				case v1beta1.GetStateNone:
					getStateMode = configmodel.GetStateMode_NONE
				case v1beta1.GetStateOpState:
					getStateMode = configmodel.GetStateMode_OP_STATE
				case v1beta1.GetStateExplicitRoPaths:
					getStateMode = configmodel.GetStateMode_EXPLICIT_RO_PATHS
				case v1beta1.GetStateExplicitRoPathsExpandWildcards:
					getStateMode = configmodel.GetStateMode_EXPLICIT_RO_PATHS_EXPAND_WILDCARDS
				}

				request := &configmodel.PushModelRequest{
					Model: &configmodel.ConfigModel{
						Name:         model.Spec.Plugin.Type,
						Version:      model.Spec.Plugin.Version,
						GetStateMode: getStateMode,
						Modules:      modules,
						Files:        files,
					},
				}
				_, err = client.PushModel(context.TODO(), request)
				if err != nil {
					errStatus, _ := status.FromError(err)
					if !errors.IsAlreadyExists(errors.FromStatus(errStatus)) {
						log.Errorf("PushModel failed for Model '%s/%s': %s", model.Namespace, model.Name, err.Error())
						return reconcile.Result{}, err
					}
					log.Debugf("Model '%s/%s' is already installed in Pod '%s' registry", model.Namespace, model.Name, pod.Name)
				} else {
					log.Debugf("Installed Model '%s/%s' into Pod '%s' registry", model.Namespace, model.Name, pod.Name)
				}

				model.Status.RegistryStatuses[podIndex] = v1beta1.RegistryStatus{
					PodName: pod.Name,
					Phase:   v1beta1.ModelInstalled,
				}
				if err := r.client.Status().Update(context.TODO(), model); err != nil {
					log.Warnf("Failed to update status for Model '%s/%s': %s", model.Namespace, model.Name, err)
					return reconcile.Result{}, err
				}
				return reconcile.Result{}, nil
			case v1beta1.ModelInstalled:
			}
		}
	}

	// Update the status for deleted pods
	for _, podStatus := range model.Status.RegistryStatuses {
		pod := &corev1.Pod{}
		podName := types.NamespacedName{
			Namespace: model.Namespace,
			Name:      podStatus.PodName,
		}
		if err := r.client.Get(context.TODO(), podName, pod); err != nil && k8serrors.IsNotFound(err) {
			log.Debugf("Forgetting Model '%s/%s' status for Pod '%s'", model.Namespace, model.Name, pod.Name)
			podStatuses := make([]v1beta1.RegistryStatus, 0, len(model.Status.RegistryStatuses)-1)
			for _, s := range model.Status.RegistryStatuses {
				if s.PodName != pod.Name {
					podStatuses = append(podStatuses, s)
				}
			}
			model.Status.RegistryStatuses = podStatuses
			if err := r.client.Status().Update(context.TODO(), model); err != nil {
				log.Warnf("Failed to update status for Model '%s/%s': %s", model.Namespace, model.Name, err)
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, nil
		}
	}

	if requeue {
		return reconcile.Result{RequeueAfter: retryDuration}, nil
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileDelete(model *v1beta1.Model) (reconcile.Result, error) {
	// If the model does not define a plugin configuration, it shouldn't have been injected into any pods
	if model.Spec.Plugin == nil {
		return reconcile.Result{}, nil
	}

	// If the model has already been finalized, exit reconciliation
	if !k8s.HasFinalizer(model, configFinalizer) {
		return reconcile.Result{}, nil
	}
	log.Debugf("Finalizing Model '%s/%s'", model.Namespace, model.Name)

	// Find all pods into which the model can be injected
	pods := &corev1.PodList{}
	podOpts := &client.ListOptions{
		Namespace: model.Namespace,
	}
	if err := r.client.List(context.TODO(), pods, podOpts); err != nil {
		return reconcile.Result{}, err
	}

	// Install the model to each registry
	for _, pod := range pods.Items {
		if pod.Annotations[registry.RegistryInjectStatusAnnotation] == registry.RegistryInjectStatusInjected {
			log.Debugf("Deleting Model '%s/%s' from Pod '%s'", model.Namespace, model.Name, pod.Name)
			conn, err := grpc.ConnectAddress(fmt.Sprintf("%s:5151", pod.Status.PodIP))
			if err != nil {
				return reconcile.Result{}, err
			}
			defer conn.Close()
			client := configmodel.NewConfigModelRegistryServiceClient(conn)
			request := &configmodel.DeleteModelRequest{
				Name:    model.Spec.Plugin.Type,
				Version: model.Spec.Plugin.Version,
			}
			_, err = client.DeleteModel(context.TODO(), request)
			if err != nil {
				errStatus, _ := status.FromError(err)
				if !errors.IsNotFound(errors.FromStatus(errStatus)) {
					log.Errorf("Failed to delete Model '%s/%s' from Pod '%s': %s", model.Namespace, model.Name, pod.Name, err.Error())
					return reconcile.Result{}, err
				}
			}
		}
	}

	// Once the model has been deleted, remove the topology finalizer
	log.Debugf("Model '%s/%s' finalized", model.Namespace, model.Name)
	k8s.RemoveFinalizer(model, configFinalizer)
	if err := r.client.Update(context.TODO(), model); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

type modelMapper struct {
	client client.Client
}

func (m *modelMapper) Map(object handler.MapObject) []reconcile.Request {
	if pod, ok := object.Object.(*corev1.Pod); ok &&
		pod.Annotations[registry.RegistryInjectStatusAnnotation] != registry.RegistryInjectStatusInjected {
		return []reconcile.Request{}
	}

	models := &v1beta1.ModelList{}
	modelOpts := &client.ListOptions{
		Namespace: object.Meta.GetNamespace(),
	}
	err := m.client.List(context.TODO(), models, modelOpts)
	if err != nil {
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, len(models.Items))
	for i, model := range models.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: model.Namespace,
				Name:      model.Name,
			},
		}
	}
	return requests
}
