/*
Copyright 2018 The Volcano Authors.

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

package helpers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/client-go/kubernetes"

	vcbatch "volcano.sh/volcano/pkg/apis/batch/v1alpha1"
	vcbus "volcano.sh/volcano/pkg/apis/bus/v1alpha1"
)

// JobKind  creates job GroupVersionKind
var JobKind = vcbatch.SchemeGroupVersion.WithKind("Job")

// CommandKind  creates command GroupVersionKind
var CommandKind = vcbus.SchemeGroupVersion.WithKind("Command")

// GetController  returns the controller uid
func GetController(obj interface{}) types.UID {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return ""
	}

	controllerRef := metav1.GetControllerOf(accessor)
	if controllerRef != nil {
		return controllerRef.UID
	}

	return ""
}

// ControlledBy  controlled by
func ControlledBy(obj interface{}, gvk schema.GroupVersionKind) bool {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return false
	}

	controllerRef := metav1.GetControllerOf(accessor)
	if controllerRef != nil {
		return controllerRef.Kind == gvk.Kind
	}

	return false
}

// CreateConfigMapIfNotExist  creates config map resource if not present
func CreateConfigMapIfNotExist(job *vcbatch.Job, kubeClients kubernetes.Interface, data map[string]string, cmName string) error {
	// If ConfigMap does not exist, create one for Job.
	cmOld, err := kubeClients.CoreV1().ConfigMaps(job.Namespace).Get(cmName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			glog.V(3).Infof("Failed to get Configmap for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)
			return err
		}

		cm := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: job.Namespace,
				Name:      cmName,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(job, JobKind),
				},
			},
			Data: data,
		}

		if _, err := kubeClients.CoreV1().ConfigMaps(job.Namespace).Create(cm); err != nil {
			glog.V(3).Infof("Failed to create ConfigMap for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)
			return err
		}
		return nil
	}

	cmOld.Data = data
	if _, err := kubeClients.CoreV1().ConfigMaps(job.Namespace).Update(cmOld); err != nil {
		glog.V(3).Infof("Failed to update ConfigMap for Job <%s/%s>: %v",
			job.Namespace, job.Name, err)
		return err
	}

	return nil
}

// DeleteConfigmap  deletes the config map resource
func DeleteConfigmap(job *vcbatch.Job, kubeClients kubernetes.Interface, cmName string) error {
	if _, err := kubeClients.CoreV1().ConfigMaps(job.Namespace).Get(cmName, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			glog.V(3).Infof("Failed to get Configmap for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)
			return err
		}
		return nil

	}

	if err := kubeClients.CoreV1().ConfigMaps(job.Namespace).Delete(cmName, nil); err != nil {
		if !apierrors.IsNotFound(err) {
			glog.Errorf("Failed to delete Configmap of Job %v/%v: %v",
				job.Namespace, job.Name, err)
			return err
		}
	}

	return nil
}

// GeneratePodgroupName  generate podgroup name of normal pod
func GeneratePodgroupName(pod *v1.Pod) string {
	pgName := vcbatch.PodgroupNamePrefix

	if len(pod.OwnerReferences) != 0 {
		for _, ownerReference := range pod.OwnerReferences {
			if ownerReference.Controller != nil && *ownerReference.Controller == true {
				pgName += string(ownerReference.UID)
				return pgName
			}
		}
	}

	pgName += string(pod.UID)

	return pgName
}

// StartHealthz register healthz interface
func StartHealthz(healthzBindAddress, name string) error {
	listener, err := net.Listen("tcp", healthzBindAddress)
	if err != nil {
		return fmt.Errorf("failed to create listener: %v", err)
	}

	pathRecorderMux := mux.NewPathRecorderMux(name)
	healthz.InstallHandler(pathRecorderMux)

	server := &http.Server{
		Addr:           listener.Addr().String(),
		Handler:        pathRecorderMux,
		MaxHeaderBytes: 1 << 20,
	}

	return runServer(server, listener)
}

func runServer(server *http.Server, ln net.Listener) error {
	if ln == nil || server == nil {
		return fmt.Errorf("listener and server must not be nil")
	}

	stopCh := make(chan os.Signal)
	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-stopCh
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		server.Shutdown(ctx)
		cancel()
	}()

	go func() {
		defer utilruntime.HandleCrash()

		var listener net.Listener
		listener = tcpKeepAliveListener{ln.(*net.TCPListener)}

		err := server.Serve(listener)
		msg := fmt.Sprintf("Stopped listening on %s", listener.Addr().String())
		select {
		case <-stopCh:
			glog.Info(msg)
		default:
			glog.Fatalf("%s due to error: %v", msg, err)
		}
	}()

	return nil
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

// Accept waits for and returns the next connection to the listener.
func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}
