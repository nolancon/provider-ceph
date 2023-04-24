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

	"golang.org/x/sync/errgroup"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane/provider-ceph/apis/provider-ceph/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-ceph/apis/v1alpha1"
	"github.com/crossplane/provider-ceph/internal/backendstore"
	"github.com/crossplane/provider-ceph/internal/controller/features"
	s3internal "github.com/crossplane/provider-ceph/internal/s3"
)

const (
	errNotBucket            = "managed resource is not a Bucket custom resource"
	errTrackPCUsage         = "cannot track ProviderConfig usage"
	errGetPC                = "cannot get ProviderConfig"
	errListPC               = "cannot list ProviderConfigs"
	errGetBucket            = "cannot get Bucket"
	errListBuckets          = "cannot list Buckets"
	errCreateBucket         = "cannot create Bucket"
	errDeleteBucket         = "cannot delete Bucket"
	errGetCreds             = "cannot get credentials"
	errBackendNotStored     = "s3 backend is not stored"
	errNoS3BackendsStored   = "no s3 backends stored"
	errCodeBucketNotFound   = "NotFound"
	errFailedToCreateClient = "failed to create s3 client"

	defaultPC = "default"
)

// A NoOpService does nothing.
type NoOpService struct{}

var (
	newNoOpService = func(_ []byte) (interface{}, error) { return &NoOpService{}, nil }
)

// Setup adds a controller that reconciles Bucket managed resources.
func Setup(mgr ctrl.Manager, o controller.Options, s *backendstore.BackendStore) error {
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
			newServiceFn: newNoOpService,
			backendStore: s,
			log:          o.Logger.WithValues("controller", name),
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.Bucket{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(creds []byte) (interface{}, error)
	backendStore *backendstore.BackendStore
	log          logging.Logger
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	return &external{backendStore: c.backendStore.GetBackendStore(), log: c.log}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	backendStore *backendstore.BackendStore
	log          logging.Logger
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	bucket, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotBucket)
	}
	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// observed only on this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if bucket.GetProviderConfigReference() != nil && bucket.GetProviderConfigReference().Name != defaultPC {
		return c.observe(ctx, bucket.Name, bucket.GetProviderConfigReference().Name)
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that this bucket is to
	// be observed on all S3 Backends.
	if !c.backendStore.BackendsAreStored() {
		return managed.ExternalObservation{}, errors.New(errNoS3BackendsStored)
	}

	return c.observeAll(ctx, bucket.Name)
}

func (c *external) observe(ctx context.Context, bucketName, backendName string) (managed.ExternalObservation, error) {
	bucketExists, err := c.bucketExists(ctx, backendName, bucketName)
	if err != nil {
		return managed.ExternalObservation{}, err
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

func (c *external) observeAll(ctx context.Context, bucketName string) (managed.ExternalObservation, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type bucketExistsResult struct {
		bucketExists bool
		err          error
	}

	bucketExistsResults := make(chan bucketExistsResult)

	// Check for the bucket on each backend in a separate go routine
	allBackends := c.backendStore.GetAllBackends()
	for s3BackendName := range allBackends {
		go func(backendName, bucketName string) {
			bucketExists, err := c.bucketExists(ctx, backendName, bucketName)
			bucketExistsResults <- bucketExistsResult{bucketExists, err}
		}(s3BackendName, bucketName)
	}

	// Wait for any go routine to finish, if the bucket exists anywhere
	// return 'ResourceExists: true' as resulting calls to Create or Delete
	// are idempotent.
	for i := 0; i < len(allBackends); i++ {
		result := <-bucketExistsResults
		if result.err != nil {
			c.log.Info(errors.Wrap(result.err, errGetBucket).Error())

			continue
		}

		if result.bucketExists {
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
	}

	// bucket not found anywhere.
	return managed.ExternalObservation{
		// Return false when the external resource does not exist. This lets
		// the managed resource reconciler know that it needs to call Create to
		// (re)create the resource, or that it has successfully been deleted.
		ResourceExists: false,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	bucket, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotBucket)
	}

	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// created only on this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if bucket.GetProviderConfigReference() != nil && bucket.GetProviderConfigReference().Name != defaultPC {
		return c.create(ctx, bucket)
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that this bucket is to
	// be created on all S3 Backends.
	return c.createAll(ctx, bucket)
}

func (c *external) create(ctx context.Context, bucket *v1alpha1.Bucket) (managed.ExternalCreation, error) {
	s3Backend, err := c.getStoredBackend(bucket.GetProviderConfigReference().Name)
	if err != nil {
		return managed.ExternalCreation{}, err
	}

	c.log.Info("Creating bucket on single s3 backend", "bucket name", bucket.Name, "backend name", bucket.GetProviderConfigReference().Name)
	bucket.Status.SetConditions(xpv1.Creating())

	_, err = s3Backend.CreateBucket(ctx, s3internal.BucketToCreateBucketInput(bucket))
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateBucket)
	}

	bucket.Status.SetConditions(xpv1.Available())

	return managed.ExternalCreation{}, nil
}

func (c *external) createAll(ctx context.Context, bucket *v1alpha1.Bucket) (managed.ExternalCreation, error) {
	if !c.backendStore.BackendsAreStored() {
		return managed.ExternalCreation{}, errors.New(errNoS3BackendsStored)
	}

	c.log.Info("Creating bucket on all available s3 backends", "bucket name", bucket.Name)
	bucket.Status.SetConditions(xpv1.Creating())

	g := new(errgroup.Group)
	for _, client := range c.backendStore.GetAllBackends() {
		cl := client
		g.Go(func() error {
			_, err := cl.CreateBucket(ctx, s3internal.BucketToCreateBucketInput(bucket))

			return err
		})
	}
	if err := g.Wait(); err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateBucket)
	}

	bucket.Status.SetConditions(xpv1.Available())

	return managed.ExternalCreation{}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	bucket, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotBucket)
	}
	bucket.Status.SetConditions(xpv1.Available())

	return managed.ExternalUpdate{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	bucket, ok := mg.(*v1alpha1.Bucket)
	if !ok {
		return errors.New(errNotBucket)
	}

	// Where a bucket has a ProviderConfigReference Name, we can infer that this bucket is to be
	// deleted only from this S3 Backend. An empty config reference name will be automatically set
	// to "default".
	if bucket.GetProviderConfigReference() != nil && bucket.GetProviderConfigReference().Name != defaultPC {
		backendName := bucket.GetProviderConfigReference().Name
		s3Backend, err := c.getStoredBackend(backendName)
		if err != nil {
			return err
		}

		c.log.Info("Deleting bucket on single s3 backend", "bucket name", bucket.Name, "backend name", backendName)
		bucket.Status.SetConditions(xpv1.Deleting())

		return c.delete(ctx, bucket.Name, s3Backend)
	}

	// No ProviderConfigReference Name specified for bucket, we can infer that his bucket is to
	// be deleted from all S3 Backends.
	bucket.Status.SetConditions(xpv1.Deleting())

	return c.deleteAll(ctx, bucket.Name)
}

func (c *external) delete(ctx context.Context, bucketName string, s3Backend *s3.Client) error {
	_, err := s3Backend.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucketName)})
	if err != nil {
		var noSuchBucketErr *s3types.NotFound
		if errors.As(err, &noSuchBucketErr) {
			return nil
		}

		return errors.Wrap(err, errDeleteBucket)
	}

	return nil
}

func (c *external) deleteAll(ctx context.Context, bucketName string) error {
	if !c.backendStore.BackendsAreStored() {
		return errors.New(errNoS3BackendsStored)
	}

	c.log.Info("Deleting bucket on all available s3 backends", "bucket name", bucketName)

	g := new(errgroup.Group)
	for _, client := range c.backendStore.GetAllBackends() {
		cl := client
		g.Go(func() error {
			return c.delete(ctx, bucketName, cl)
		})
	}
	if err := g.Wait(); err != nil {
		return errors.Wrap(err, errCreateBucket)
	}

	return nil
}

func (c *external) bucketExists(ctx context.Context, s3BackendName, bucketName string) (bool, error) {
	s3Backend, err := c.getStoredBackend(s3BackendName)
	if err != nil {
		return false, err
	}
	_, err = s3Backend.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucketName)})
	if err != nil {
		var notFoundErr *s3types.NotFound
		if errors.As(err, &notFoundErr) {
			// Bucket does not exist, log error and return false.
			return false, nil
		}
		// Some other error occurred, return false with error
		// as we cannot verify the bucket exists.
		return false, err
	}
	// Bucket exists, return true with no error.
	return true, nil
}

func (c *external) getStoredBackend(s3BackendName string) (*s3.Client, error) {
	s3Backend := c.backendStore.GetBackend(s3BackendName)
	if s3Backend != nil {
		return s3Backend, nil
	}

	return nil, errors.New(errBackendNotStored)
}
