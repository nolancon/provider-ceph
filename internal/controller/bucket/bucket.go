/*
Copyright 2022 The Crossplane Authors.

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

package bucket

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane/provider-ceph/apis/provider-ceph/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-ceph/apis/v1alpha1"
	"github.com/crossplane/provider-ceph/internal/controller/features"
	s3internal "github.com/crossplane/provider-ceph/internal/s3"
)

const (
	errNotBucket       = "managed resource is not a Bucket custom resource"
	errTrackPCUsage    = "cannot track ProviderConfig usage"
	errGetPC           = "cannot get ProviderConfig"
	errListPC          = "cannot list ProviderConfigs"
	errGetBucket       = "cannot get Bucket"
	errListBuckets     = "cannot list Buckets"
	errCreateBucket    = "cannot create Bucket"
	errDeleteBucket    = "cannot delete Bucket"
	errGetCreds        = "cannot get credentials"
	errBackendNotFound = "s3 backend is not stored"
)

// A NoOpService does nothing.
type NoOpService struct{}

var (
	newNoOpService = func(_ []byte) (interface{}, error) { return &NoOpService{}, nil }
)

// Setup adds a controller that reconciles Bucket managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.BucketGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.BucketGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			newServiceFn: newNoOpService}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.Bucket{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// s3Backends is a map of S3 backend name (eg ceph cluster name) to S3 client.
type s3Backends map[string]*s3.S3

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube               client.Client
	usage              resource.Tracker
	newServiceFn       func(creds []byte) (interface{}, error)
	existingS3Backends s3Backends
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return nil, errors.New(errNotBucket)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Create a new map of S3 Backends if one does not exist.
	if c.existingS3Backends == nil {
		c.existingS3Backends = make(s3Backends)
	}

	// A ProviderConfig in the context of this controller represents an S3 Backend.

	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// observed only on this S3 Backend. Therefore we only need to connect to his S3 Backend.
	// An empty config reference name will be automatically set to "default".
	if cr.GetProviderConfigReference().Name != "default" {
		// An S3 Backend was specified for this bucket, so discover the backend creds
		// via its secret reference.
		pc := &apisv1alpha1.ProviderConfig{}
		if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
			return nil, errors.Wrap(err, errGetPC)
		}
		secret, err := c.getProviderConfigSecret(ctx, pc.Spec.Credentials.SecretRef.Namespace, pc.Spec.Credentials.SecretRef.Name)
		if err != nil {
			return nil, err
		}

		// Create the client for the S3 Backend and update the connector's existing S3 Backends.
		c.existingS3Backends[pc.Name] = s3internal.NewClient(secret.Data, &pc.Spec)

		return &external{s3Backends: c.existingS3Backends}, nil
	}

	// The bucket has no ProviderConfigReference Name, therefore we can infer that this bucket is
	// to be observer on all S3 Backends. Therefore we need to connect to all existing S3 Backends.
	pcList := &apisv1alpha1.ProviderConfigList{}
	if err := c.kube.List(ctx, pcList); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	for _, pc := range pcList.Items {
		secret, err := c.getProviderConfigSecret(ctx, pc.Spec.Credentials.SecretRef.Namespace, pc.Spec.Credentials.SecretRef.Name)
		if err != nil {
			return nil, err
		}

		// Create the client for the S3 Backend and update the connector's existing S3 Backends.
		c.existingS3Backends[pc.Name] = s3internal.NewClient(secret.Data, &pc.Spec)
	}

	return &external{s3Backends: c.existingS3Backends}, nil
}

func (c *connector) getProviderConfigSecret(ctx context.Context, secretNamespace, secretName string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	ns := types.NamespacedName{Namespace: secretNamespace, Name: secretName}
	if err := c.kube.Get(ctx, ns, secret); err != nil {
		return nil, errors.Wrap(err, "cannot get provider secret")
	}
	return secret, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	// A 'client' used to connect to the external resource API. In practice this
	// would be something like an AWS SDK client.
	s3Backends s3Backends
}

func (c *external) bucketExists(ctx context.Context, s3BackendName, bucketName string) (bool, error) {
	_, err := c.s3Backends[s3BackendName].HeadBucketWithContext(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucketName)})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			// Ceph returns "NotFound"
			case s3.ErrCodeNoSuchBucket, "NotFound":
				// Bucket does not exist, log error and return false.
				return false, nil
			default:
				// AWS error occurred, return false with error
				// as we cannot verify the bucket exists.
				return false, aerr
			}

			// Some other error occurred, return false with error
			// as we cannot verify the bucket exists.
			return false, err
		}
	}
	// Bucket exists, return true with no error.
	return true, nil
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotBucket)
	}
	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// observed only on this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if cr.GetProviderConfigReference().Name != "default" {
		bucketExists, err := c.bucketExists(ctx, cr.GetProviderConfigReference().Name, cr.Name)
		if err != nil {
			return managed.ExternalObservation{}, errors.New(errGetBucket)
		}
		if bucketExists {
			return managed.ExternalObservation{
				// Return false when the external resource does not exist. This lets
				// the managed resource reconciler know that it needs to call Create to
				// (re)create the resource, or that it has successfully been deleted.
				ResourceExists: true,

				// Return false when the external resource exists, but it not up to date
				// with the desired managed resource state. This lets the managed
				// resource reconciler know that it needs to call Update.
				ResourceUpToDate: false,

				// Return any details that may be required to connect to the external
				// resource. These will be stored as the connection secret.
				ConnectionDetails: managed.ConnectionDetails{},
			}, nil
		}

		return managed.ExternalObservation{
			// Return false when the external resource does not exist. This lets
			// the managed resource reconciler know that it needs to call Create to
			// (re)create the resource, or that it has successfully been deleted.
			ResourceExists: false,
		}, nil
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that his bucket is to
	// be observed on all S3 Backends.
	for s3BackendName, _ := range c.s3Backends {
		bucketExists, err := c.bucketExists(ctx, s3BackendName, cr.Name)
		if err != nil {
			return managed.ExternalObservation{}, errors.New(errGetBucket)
		}
		if bucketExists {
			return managed.ExternalObservation{
				// Return false when the external resource does not exist. This lets
				// the managed resource reconciler know that it needs to call Create to
				// (re)create the resource, or that it has successfully been deleted.
				ResourceExists: true,

				// Return false when the external resource exists, but it not up to date
				// with the desired managed resource state. This lets the managed
				// resource reconciler know that it needs to call Update.
				ResourceUpToDate: false,

				// Return any details that may be required to connect to the external
				// resource. These will be stored as the connection secret.
				ConnectionDetails: managed.ConnectionDetails{},
			}, nil
		}
		return managed.ExternalObservation{
			// Return false when the external resource does not exist. This lets
			// the managed resource reconciler know that it needs to call Create to
			// (re)create the resource, or that it has successfully been deleted.
			ResourceExists: false,
		}, nil

	}
	return managed.ExternalObservation{
		// Return false when the external resource does not exist. This lets
		// the managed resource reconciler know that it needs to call Create to
		// (re)create the resource, or that it has successfully been deleted.
		ResourceExists: false,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotBucket)
	}

	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// created only on this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if cr.GetProviderConfigReference().Name != "default" {
		_, err := c.s3Backends[cr.GetProviderConfigReference().Name].CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(cr.Name)})
		if err != nil {
			return managed.ExternalCreation{}, errors.Wrap(err, errCreateBucket)
		}

		return managed.ExternalCreation{
			// Optionally return any details that may be required to connect to the
			// external resource. These will be stored as the connection secret.
			ConnectionDetails: managed.ConnectionDetails{},
		}, nil
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that his bucket is to
	// be created on all S3 Backends.
	for _, client := range c.s3Backends {
		_, err := client.CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(cr.Name)})
		if err != nil {
			return managed.ExternalCreation{}, errors.Wrap(err, errCreateBucket)
		}
	}

	return managed.ExternalCreation{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	_, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotBucket)
	}

	return managed.ExternalUpdate{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return errors.New(errNotBucket)
	}

	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// deleted only from this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if cr.GetProviderConfigReference().Name != "default" {
		_, err := c.s3Backends[cr.GetProviderConfigReference().Name].DeleteBucketWithContext(ctx, &s3.DeleteBucketInput{Bucket: aws.String(cr.Name)})

		if err != nil {
			return errors.Wrap(err, errDeleteBucket)
		}

		return nil
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that his bucket is to
	// be deleted from all S3 Backends.
	for _, client := range c.s3Backends {
		_, err := client.DeleteBucketWithContext(ctx, &s3.DeleteBucketInput{Bucket: aws.String(cr.Name)})
		if err != nil {
			return errors.Wrap(err, errCreateBucket)
		}
	}

	return nil
}
