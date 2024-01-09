/*
Copyright 2023.

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

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	esupvgrycapv1 "es.upv.grycap/korgi/api/v1"
)

// Resource represents a resource with name, max value, and available value
type GPUResource struct {
	Name      string
	Count     int
	Allocated int
}

// KorgiJobReconciler reconciles a KorgiJob object
type KorgiJobReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	GPUResources     map[string]GPUResource
	currentConfigMap corev1.ConfigMap
}

//+kubebuilder:rbac:groups=es.upv.grycap.es.upv.grycap,resources=korgijobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=es.upv.grycap.es.upv.grycap,resources=korgijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=es.upv.grycap.es.upv.grycap,resources=korgijobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=jobs/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KorgiJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *KorgiJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get the ConfigMap
	var configMap corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      "korgi-resources-config",
		Namespace: corev1.NamespaceDefault,
	}, &configMap); err != nil {
		log.Error(err, "Unable to get ConfigMap")
		return ctrl.Result{}, err
	}
	// Check if the ConfigMap has changed
	if configMap.ResourceVersion != r.currentConfigMap.ResourceVersion {
		// ConfigMap has changed, perform necessary actions
		log.Info("ConfigMap has changed")
		// Update the recorded version
		r.currentConfigMap = configMap
		// Go through the data of the ConfigMap, creating a list of GPU resources
		for _, value := range configMap.Data {
			fmt.Println("korgi-resources-config value: ", value)
			r.GPUResources[value] = GPUResource{Name: value, Count: 0, Allocated: 0}
			// count how many resources are available in each node
			var nodes corev1.NodeList
			if err := r.List(ctx, &nodes); err != nil {
				log.Error(err, "Unable to list nodes")
				return ctrl.Result{}, err
			}
			for _, node := range nodes.Items {
				fmt.Println("Node: ", node.GetName())
				for name, _ := range node.Status.Allocatable {
					if name.String() == value {
						temp := r.GPUResources[value]
						temp.Count++
						r.GPUResources[value] = temp
						break
					}
				}
			}
		}
	} else {
		// ConfigMap has not changed
		log.Info("ConfigMap has not changed")
	}

	// Update resources
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		log.Error(err, "Unable to list nodes")
		return ctrl.Result{}, err
	}

	//log.Info("Reconciling KorgiJob: ", "namespace", req.Namespace, "name ", req.Name)

	// Get the current state of the object from the API server
	var korgiJob = &esupvgrycapv1.KorgiJob{}
	if err := r.Client.Get(ctx, req.NamespacedName, korgiJob); err != nil {
		log.Error(err, "Unable to fetch KorgiJob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//log.Info("KorgiJob: " + korgiJob.GetName())

	switch korgiJob.GetStatus() {
	case "":

		// Update GPUResources
		UpdateGPUResources(r, ctx, log)

		// Change status to Pending
		korgiJob.Status.Status = esupvgrycapv1.KorgiJobPending
		if err := r.Client.Status().Update(ctx, korgiJob); err != nil {
			log.Error(err, "Status update failed (to pending ) 010")
			return ctrl.Result{}, err
		}

	case esupvgrycapv1.KorgiJobPending:
		// Create associated job
		log.Info("KorgiJob", korgiJob.GetName(), "Status = PENDING")
		//log.Info("Creating job from KorgiJob")
		jobName := fmt.Sprintf("%s-%d", korgiJob.Name+"-subjob", time.Now().Unix())

		// Get label "resource" from korgiJob namespace
		korgiJobNamespace := &corev1.Namespace{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: req.Namespace}, korgiJobNamespace); err != nil {
			log.Error(err, "Unable to fetch KorgiJobNamespace")
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		var labelName string
		// call GetBestAllocatableGPU, labelName is the first return value if no error is found
		labelName, err := GetBestAllocatableGPU(r, ctx, log)
		if err != nil {
			log.Error(err, "Unable to get best allocatable GPU")
			return ctrl.Result{}, err
		}
		//log.Info("KorgiJobNamespace: " + korgiJobNamespace.GetName())
		/* resourceLabel := korgiJobNamespace.GetLabels() */
		//log.Info("KorgiJobNamespace.Labels: " + fmt.Sprintf("%v", resourceLabel))
		//labelName gets the resource from namespace label
		/* 		for key, value := range resourceLabel {
			if value == "resource" {
				labelName = key
				break
			}
		} */
		//log.Info("labelName: " + labelName)

		job := batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: korgiJob.Namespace,
				Labels:    korgiJob.Labels,
			},

			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{ //TO DO: make it configurable
							corev1.Volume{
								Name: "dataset-volume",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/hdd500/data",
									},
								},
							},
							corev1.Volume{
								Name: "tensorboard-volume",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/hdd500/tensorboard",
									},
								},
							},
						},
						Containers: []corev1.Container{
							corev1.Container{
								Name:    korgiJob.Name + "-container",
								Image:   korgiJob.Spec.Image,
								Command: korgiJob.Spec.Command,
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceName(labelName): resource.MustParse("1"),
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									corev1.VolumeMount{
										Name:      "dataset-volume",
										MountPath: "/data",
									},
									corev1.VolumeMount{
										Name:      "tensorboard-volume",
										MountPath: "/tensorboard",
									},
								},
							},
						},
						RestartPolicy: "Never",
					},
				},
			},
		}
		if _, err := ctrl.CreateOrUpdate(ctx, r.Client, &job, func() error {
			//log.Info("Job created", "job", job.Name)
			ctrl.SetControllerReference(korgiJob, &job, r.Scheme)

			// Update KorgiJob status to Running
			korgiJob.Status.Status = esupvgrycapv1.KorgiJobRunning
			if err2 := r.Client.Status().Update(ctx, korgiJob); err2 != nil {
				log.Error(err2, "Status update failed (from pending to running)")
				return err2
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}

	case esupvgrycapv1.KorgiJobRunning:
		// Check Job Status
		// if Active 	-> wait
		// if Succeded 	-> change status to Completed
		// if Failed  	-> change status to Failed
		//log.Info("KorgiJob.Status = RUNNING")

		var childJobs batchv1.JobList
		if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace)); err != nil {
			log.Error(err, "Unable to list child Jobs")
			return ctrl.Result{}, err
		}
		if childJobs.Size() > 0 {
			active := childJobs.Items[0].Status.Active
			succeeded := childJobs.Items[0].Status.Succeeded
			failed := childJobs.Items[0].Status.Failed
			//log.Info("", "Active: ", active, "Succeeded: ", succeeded, "Failed: ", failed)
			if !(active == 0 && failed == 0 && succeeded == 0) {
				if failed > 0 {
					korgiJob.Status.Status = esupvgrycapv1.KorgiJobFailed
				}
				if succeeded > 0 {
					korgiJob.Status.Status = esupvgrycapv1.KorgiJobCompleted
				}
				if err := r.Client.Status().Update(ctx, korgiJob); err != nil {
					log.Error(err, "Status update failed")
					return ctrl.Result{}, err
				}
			}
		}

	case esupvgrycapv1.KorgiJobCompleted:
		// --
		//log.Info("KorgiJob.Status = COMPLETED")

	case esupvgrycapv1.KorgiJobFailed:
		//log.Info("KorgiJob.Status = FAILED")
	}

	return ctrl.Result{}, nil
}

func UpdateGPUResources(r *KorgiJobReconciler, ctx context.Context, log logr.Logger) (reconcile.Result, error) {
	//panic("unimplemented")
	// Go through all pods (all namespaces) and update GPU resources
	fmt.Println("Listing pod resources in all namespaces")
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		log.Error(err, "Unable to list pods")
		return ctrl.Result{}, err
	}
	// Reset GPU Resources allocated to 0
	for _, resource := range r.GPUResources {
		resource := resource
		resource.Allocated = 0
		r.GPUResources[resource.Name] = resource
	}
	for _, pod := range pods.Items {
		// get spec.containers.resources.limits key/value
		// Print pod name
		// fmt.Println("Pod: ", pod.GetName())
		// for each value in r.currentConfigMap.Data values
		for _, value := range r.currentConfigMap.Data {
			// fmt.Println("korgi-resources-config value: ", value)
			// if this value resource exists in the current pod add 1 to allocated
			if len(pod.Spec.Containers) > 0 {
				for _, container := range pod.Spec.Containers {
					// if the limit is set for this resource
					if _, exists := container.Resources.Limits[corev1.ResourceName(value)]; exists {
						resource := r.GPUResources[value]
						resource.Allocated++
						fmt.Println("Pod", pod.GetName(), "using ", value)
						r.GPUResources[value] = resource
					}
				}
			}
		}
	}
	// return ok
	return ctrl.Result{}, nil
}

// GetBestAllocatableGPU returns the best GPU resource to allocate
func GetBestAllocatableGPU(r *KorgiJobReconciler, ctx context.Context, log logr.Logger) (string, error) {
	// r.GPUResources
	for _, value := range r.currentConfigMap.Data {
		resource := r.GPUResources[value]
		if resource.Allocated < resource.Count {
			return value, nil
		}
	}
	return "", fmt.Errorf("no GPU resource available")
}

// SetupWithManager sets up the controller with the Manager.
func (r *KorgiJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.GPUResources = make(map[string]GPUResource)
	return ctrl.NewControllerManagedBy(mgr).
		For(&esupvgrycapv1.KorgiJob{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
