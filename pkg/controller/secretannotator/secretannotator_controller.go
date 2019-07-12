/*
Copyright 2018 The OpenShift Authors.

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

package secretannotator

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/aws-sdk-go/aws/credentials"

	ccaws "github.com/openshift/cloud-credential-operator/pkg/aws"
	"github.com/openshift/cloud-credential-operator/pkg/controller/utils"
)

const (
	controllerName = "secretannotator"

	// TODO: dynamically detect which environment we're running on
	CloudCredSecretName      = "aws-creds"
	CloudCredSecretNamespace = "kube-system"

	AnnotationKey = "cloudcredential.openshift.io/mode"

	// MintAnnottation is used whenever it is determined that the cloud creds
	// are sufficient for minting new creds to satisfy a CredentialsRequest
	MintAnnotation = "mint"

	// PassthroughAnnotation is used whenever it is determined that the cloud creds
	// are sufficient for passing through to satisfy a CredentialsRequest.
	// This would be based on having creds that can satisfy the static list of creds
	// found in this repo's manifests/ dir.
	PassthroughAnnotation = "passthrough"

	// InsufficientAnnotation is used to indicate that the creds do not have
	// sufficient permissions for cluster runtime.
	InsufficientAnnotation = "insufficient"

	AwsAccessKeyName       = "aws_access_key_id"
	AwsSecretAccessKeyName = "aws_secret_access_key"

	AzureClientID       = "azure_client_id"
	AzureClientSecret   = "azure_client_secret"
	AzureRegion         = "azure_region"
	AzureResourceGroup  = "azure_resourcegroup"
	AzureResourcePrefix = "azure_resource_prefix"
	AzureSubscriptionID = "azure_subscription_id"
	AzureTenantID       = "azure_tenant_id"
)

func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileCloudCredSecret{
		Client:           mgr.GetClient(),
		logger:           log.WithField("controller", controllerName),
		AWSClientBuilder: ccaws.NewClient,
	}
}

func cloudCredSecretObjectCheck(secret metav1.Object) bool {
	if secret.GetNamespace() == CloudCredSecretNamespace && secret.GetName() == CloudCredSecretName {
		return true
	}
	return false
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to cluster cloud secret
	p := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return cloudCredSecretObjectCheck(e.MetaNew)
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
	}
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}, p)
	if err != nil {
		return err
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileCloudCredSecret{}

type ReconcileCloudCredSecret struct {
	client.Client
	logger           log.FieldLogger
	AWSClientBuilder func(creds *credentials.Value, infraName string) (ccaws.Client, error)
}

// Reconcile will annotate the cloud cred secret to indicate the capabilities of the cred's capabilities:
// 1) 'mint' for indicating that the creds can be used to create new sub-creds
// 2) 'passthrough' for indicating that the creds are capable enough for other components to reuse the creds as-is
// 3) 'insufficient' for indicating that the creds are not usable for the cluster
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;update
func (r *ReconcileCloudCredSecret) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	r.logger.Info("validating cloud cred secret")

	secret := &corev1.Secret{}
	err := r.Get(context.Background(), request.NamespacedName, secret)
	if err != nil {
		r.logger.Debugf("secret not found: %v", err)
		return reconcile.Result{}, err
	}

	err = r.validateCloudCredsSecret(secret)
	if err != nil {
		r.logger.Errorf("error while validating cloud credentials: %v", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileCloudCredSecret) validateCloudCredsSecret(secret *corev1.Secret) error {

	accessKey, ok := secret.Data[AwsAccessKeyName]
	if !ok {
		r.logger.Errorf("Couldn't fetch key containing AWS_ACCESS_KEY_ID from cloud cred secret")
		return r.updateSecretAnnotations(secret, InsufficientAnnotation)
	}

	secretKey, ok := secret.Data[AwsSecretAccessKeyName]
	if !ok {
		r.logger.Errorf("Couldn't fetch key containing AWS_SECRET_ACCESS_KEY from cloud cred secret")
		return r.updateSecretAnnotations(secret, InsufficientAnnotation)
	}

	infraName, err := utils.LoadInfrastructureName(r.Client, r.logger)
	if err != nil {
		return err
	}
	creds := credentials.Value{
		AccessKeyID:     string(accessKey),
		SecretAccessKey: string(secretKey),
	}
	awsClient, err := r.AWSClientBuilder(&creds, infraName)
	if err != nil {
		return fmt.Errorf("error creating aws client: %v", err)
	}

	// Can we mint new creds?
	cloudCheckResult, err := utils.CheckCloudCredCreation(awsClient, r.logger)
	if err != nil {
		r.updateSecretAnnotations(secret, InsufficientAnnotation)
		return fmt.Errorf("failed checking create cloud creds: %v", err)
	}

	if cloudCheckResult {
		r.logger.Info("Verified cloud creds can be used for minting new creds")
		return r.updateSecretAnnotations(secret, MintAnnotation)
	}

	// Else, can we just pass through the current creds?
	cloudCheckResult, err = utils.CheckCloudCredPassthrough(awsClient, r.logger)
	if err != nil {
		r.updateSecretAnnotations(secret, InsufficientAnnotation)
		return fmt.Errorf("failed checking passthrough cloud creds: %v", err)
	}

	if cloudCheckResult {
		r.logger.Info("Verified cloud creds can be used as-is (passthrough)")
		return r.updateSecretAnnotations(secret, PassthroughAnnotation)
	}

	// Else, these creds aren't presently useful
	r.logger.Warning("Cloud creds unable to be used for either minting or passthrough")
	return r.updateSecretAnnotations(secret, InsufficientAnnotation)
}

func (r *ReconcileCloudCredSecret) updateSecretAnnotations(secret *corev1.Secret, value string) error {
	secretAnnotations := secret.GetAnnotations()
	if secretAnnotations == nil {
		secretAnnotations = map[string]string{}
	}

	secretAnnotations[AnnotationKey] = value
	secret.SetAnnotations(secretAnnotations)

	return r.Update(context.Background(), secret)
}
