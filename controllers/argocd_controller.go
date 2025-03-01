/*
Copyright 2021.

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

package controllers

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"

	"github.com/go-logr/logr"
	console "github.com/openshift/api/console/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/rakyll/statik/fs"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// register the statik zip content data
	_ "github.com/redhat-developer/gitops-operator/controllers/argocd/statik"
)

const (
	argocdNS           = "openshift-gitops"
	depracatedArgoCDNS = "openshift-pipelines-app-delivery"
	consoleLinkName    = "argocd"
	argocdRouteName    = "openshift-gitops-server"
	iconFilePath       = "/argo.png"
)

//go:generate statik --src ./img -f
var image string

func init() {
	image = imageDataURL(base64.StdEncoding.EncodeToString(readStatikImage()))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReconcileArgoCDRoute) SetupWithManager(mgr ctrl.Manager) error {
	// Watch for changes to argocd-server route in the default argocd instance namespace
	// The ConsoleLink holds the route URL and should be regenerated when route is updated
	return ctrl.NewControllerManagedBy(mgr).
		For(&routev1.Route{}, builder.WithPredicates(filterPredicate(filterArgoCDRoute))).
		Complete(r)
}

func filterPredicate(assert func(namespace, name string) bool) predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return assert(e.ObjectNew.GetNamespace(), e.ObjectNew.GetName()) &&
				e.ObjectNew.GetResourceVersion() != e.ObjectOld.GetResourceVersion()
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return assert(e.Object.GetNamespace(), e.Object.GetName())
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return assert(e.Object.GetNamespace(), e.Object.GetName())
		},
	}
}

func filterArgoCDRoute(namespace, name string) bool {
	return namespace == argocdNS && argocdRouteName == name
}

// blank assignment to verify that ReconcileArgoCDRoute implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileArgoCDRoute{}

// ReconcileArgoCDRoute reconciles a ArgoCD Route object
type ReconcileArgoCDRoute struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	Client client.Client
	Scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a ArgoCD Route object and makes changes based on the state read
// and what is in the Route.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileArgoCDRoute) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	var logs = logf.Log.WithName("controller_argocd_route")
	reqLogger := logs.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ArgoCD Route")

	// Fetch ArgoCD server route
	argoCDRoute := &routev1.Route{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: argocdRouteName, Namespace: argocdNS}, argoCDRoute)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("ArgoCD server route not found", "Route.Namespace", argocdNS)
			// if argocd-server route is deleted, remove the ConsoleLink if present
			return reconcile.Result{}, r.deleteConsoleLinkIfPresent(ctx, reqLogger)
		}
		return reconcile.Result{}, err
	}
	reqLogger.Info("Route found for argocd-server", "Route.Host", argoCDRoute.Spec.Host)

	argocCDRouteURL := fmt.Sprintf("https://%s", argoCDRoute.Spec.Host)

	consoleLink := newConsoleLink(argocCDRouteURL, "Cluster Argo CD")

	found := &console.ConsoleLink{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: consoleLink.Name}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("Creating a new ConsoleLink", "ConsoleLink.Name", consoleLink.Name)
			return reconcile.Result{}, r.Client.Create(ctx, consoleLink)
		}
		reqLogger.Error(err, "Failed to create ConsoleLink", "ConsoleLink.Name", consoleLink.Name)
		return reconcile.Result{}, err
	}

	if found.Spec.Href != argocCDRouteURL {
		reqLogger.Info("Updating the existing ConsoleLink", "ConsoleLink.Name", consoleLink.Name)
		found.Spec.Href = argocCDRouteURL
		return reconcile.Result{}, r.Client.Update(ctx, found)
	}

	reqLogger.Info("Skip reconcile: ConsoleLink already exists", "ConsoleLink.Name", consoleLink.Name)
	return reconcile.Result{}, nil
}

func newConsoleLink(href, text string) *console.ConsoleLink {
	return &console.ConsoleLink{
		ObjectMeta: metav1.ObjectMeta{
			Name: consoleLinkName,
		},
		Spec: console.ConsoleLinkSpec{
			Link: console.Link{
				Text: text,
				Href: href,
			},
			Location: console.ApplicationMenu,
			ApplicationMenu: &console.ApplicationMenuSpec{
				Section:  "OpenShift GitOps",
				ImageURL: image,
			},
		},
	}
}

func (r *ReconcileArgoCDRoute) deleteConsoleLinkIfPresent(ctx context.Context, log logr.Logger) error {
	err := r.Client.Get(ctx, types.NamespacedName{Name: consoleLinkName}, &console.ConsoleLink{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	log.Info("Deleting ConsoleLink", "ConsoleLink.Name", consoleLinkName)
	return r.Client.Delete(ctx, &console.ConsoleLink{ObjectMeta: metav1.ObjectMeta{Name: consoleLinkName}})
}

func readStatikImage() []byte {
	statikFs, err := fs.New()
	if err != nil {
		log.Fatalf("Failed to create a new statik filesystem: %v", err)
	}
	file, err := statikFs.Open(iconFilePath)
	if err != nil {
		log.Fatalf("Failed to open ArgoCD icon file: %v", err)
	}
	defer file.Close()
	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Fatalf("Failed to read ArgoCD icon file: %v", err)
	}
	return data
}

func imageDataURL(data string) string {
	return fmt.Sprintf("data:image/png;base64,%s", data)
}
