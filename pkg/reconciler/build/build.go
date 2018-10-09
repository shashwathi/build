/*
Copyright 2018 The Knative Authors

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

package build

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/knative/pkg/controller"
	"github.com/knative/pkg/logging"
	"github.com/knative/pkg/logging/logkey"

	v1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	"github.com/knative/build/pkg/builder"
	clientset "github.com/knative/build/pkg/client/clientset/versioned"
	buildscheme "github.com/knative/build/pkg/client/clientset/versioned/scheme"

	informers "github.com/knative/build/pkg/client/informers/externalversions"

	listers "github.com/knative/build/pkg/client/listers/build/v1alpha1"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
)

const (
	// SuccessSynced is used as part of the Event 'reason' when a Build is synced
	SuccessSynced       = "Synced"
	controllerAgentName = "build-controller"
	// ErrResourceExists is used as part of the Event 'reason' when a Build fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceSynced is the message used for an Event fired when a Build
	// is synced successfully
	MessageResourceSynced = "Build synced successfully"
)

// Reconciler is the controller.Reconciler implementation for Builds resources
type Reconciler struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// buildclientset is a clientset for our own API group
	buildclientset clientset.Interface

	buildsLister listers.BuildLister

	// Sugared logger is easier to use but is not as performant as the
	// raw logger. In performance critical paths, call logger.Desugar()
	// and use the returned raw logger instead. In addition to the
	// performance benefits, raw logger also preserves type-safety at
	// the expense of slightly greater verbosity.
	Logger *zap.SugaredLogger
	// Recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
	// The builder through which work will be done.
	builder                     builder.Interface
	buildTemplatesLister        listers.BuildTemplateLister
	clusterBuildTemplatesLister listers.ClusterBuildTemplateLister
}

// Check that we implement the controller.Reconciler interface.
var _ controller.Reconciler = (*Reconciler)(nil)

func init() {
	// Add build-controller types to the default Kubernetes Scheme so Events can be
	// logged for build-controller types.
	buildscheme.AddToScheme(scheme.Scheme)
}

// NewController returns a new build template controller
func NewController(
	logger *zap.SugaredLogger,
	kubeclientset kubernetes.Interface,
	buildclientset clientset.Interface,
	buildInformerFactory informers.SharedInformerFactory,
	podInformer corev1informers.PodInformer,
	builder builder.Interface,
) *controller.Impl {

	// Enrich the logs with controller name
	logger = logger.Named(controllerAgentName).With(zap.String(logkey.ControllerType, controllerAgentName))
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	buildInformer := buildInformerFactory.Build().V1alpha1().Builds()
	buildTemplateInformer := buildInformerFactory.Build().V1alpha1().BuildTemplates()
	clusterBuildTemplateInformer := buildInformerFactory.Build().V1alpha1().ClusterBuildTemplates()

	r := &Reconciler{
		kubeclientset:               kubeclientset,
		buildclientset:              buildclientset,
		buildsLister:                buildInformer.Lister(),
		Logger:                      logger,
		builder:                     builder,
		buildTemplatesLister:        buildTemplateInformer.Lister(),
		clusterBuildTemplatesLister: clusterBuildTemplateInformer.Lister(),
		recorder:                    eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName}),
	}
	impl := controller.NewImpl(r, r.Logger, "Builds")

	logger.Info("Setting up event handlers")
	// Set up an event handler for when Build resources change
	buildInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    impl.Enqueue,
		UpdateFunc: controller.PassNew(impl.Enqueue),
		DeleteFunc: impl.Enqueue,
	})

	// TODO(mattmoor): Set up a Pod informer, so that Pod updates
	// trigger Build reconciliations.

	return impl
}

// Reconcile implements controller.Reconciler
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	logger := logging.FromContext(ctx)

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Errorf("invalid resource key: %s", key)
		return nil
	}

	// Get the Build resource with this namespace/name
	original, err := c.buildsLister.Builds(namespace).Get(name)
	if errors.IsNotFound(err) {
		// The Build resource may no longer exist, in which case we stop processing.
		logger.Errorf("build %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}

	// Don't mutate the informer's copy of our object.
	build := original.DeepCopy()

	// TODO(jasonhall): adopt the standard reconcile pattern.
	// For Build this looks something like:
	// podName := names.Pod(build)
	// pod, err := c.podLister.Pods(build.Namespace).Get(podName)
	// if IsNotFound(err) {
	// 	desired := resources.MakePod(build)
	// 	pod, err = c.kubeclientset.V1().Pods(build.Namespace).Create(desired)
	// 	if err != nil {
	// 		return err
	// 	}
	// } else if err != nil {
	// 	return err
	// }
	//
	// // Update build.Status based on pod.Status

	err = c.reconcile(ctx, build)
	if equality.Semantic.DeepEqual(original.Status, build.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
	} else if _, err := c.updateStatus(build); err != nil {
		logger.Infof("Updating Status (-old, +new): %v", cmp.Diff(original, build))
		logger.Warn("Failed to update build status", zap.Error(err))
		return err
	}
	if err != nil {
		logger.Errorf("Error reconciling: %v", err)
	}
	return err

}

func (c *Reconciler) reconcile(ctx context.Context, build *v1alpha1.Build) error {

	// Validate build
	// TODO(shashwathi): Is this the right place?
	if err := c.validateBuild(build); err != nil {
		c.Logger.Errorf("Failed to validate build: %v", err)
		return err
	}

	// If the build's done, then ignore it.
	if !builder.IsDone(&build.Status) {

		// If the build is not done, but is in progress (has an operation), then asynchronously wait for it.
		// TODO(mattmoor): Check whether the Builder matches the kind of our c.builder.
		if build.Status.Builder != "" {
			c.Logger.Infof("REMOVE: Builder idntified: %v for build %s", build.Status.Builder, build.Name)

			op, err := c.builder.OperationFromStatus(&build.Status)
			if err != nil {
				c.Logger.Errorf("Failed to fetch operation from status %v", err)
				return err
			}

			// Check if build has timed out
			if builder.IsTimeout(&build.Status, build.Spec.Timeout) {
				//cleanup operation and update status
				timeoutMsg := fmt.Sprintf("Build %q failed to finish within %q", build.Name, build.Spec.Timeout.Duration.String())

				if err := op.Terminate(); err != nil {
					c.Logger.Errorf("Failed to terminate pod: %v", err)
					return err
				}

				build.Status.SetCondition(&duckv1alpha1.Condition{
					Type:    v1alpha1.BuildSucceeded,
					Status:  corev1.ConditionFalse,
					Reason:  "BuildTimeout",
					Message: timeoutMsg,
				})
				// update build completed time
				build.Status.CompletionTime = metav1.Now()
				c.recorder.Eventf(build, corev1.EventTypeWarning, "BuildTimeout", timeoutMsg)
				c.Logger.Errorf("Timeout: %v", timeoutMsg)
				return nil
			}

			// if not timed out then wait async
			go c.waitForOperation(build, op)
		} else {

			build.Status.Builder = c.builder.Builder()
			c.Logger.Infof("REMOVE: No Builder so attached builder info: %v for build %s", build.Status.Builder, build.Name)

			// If the build hasn't even started, then start it and record the operation in our status.
			// Note that by recording our status, we will trigger a reconciliation, so the wait above
			// will kick in.

			// TODO(shashwathi): Move this to a function
			if build.Spec.Template != nil {
				var tmpl v1alpha1.BuildTemplateInterface
				var err error

				if build.Spec.Template.Kind == v1alpha1.ClusterBuildTemplateKind {
					tmpl, err = c.clusterBuildTemplatesLister.Get(build.Spec.Template.Name)
					if err != nil {
						// The ClusterBuildTemplate resource may not exist.
						if errors.IsNotFound(err) {
							runtime.HandleError(fmt.Errorf("cluster build template %q does not exist", build.Spec.Template.Name))
						}
						return err
					}
				} else {
					tmpl, err = c.buildTemplatesLister.BuildTemplates(build.Namespace).Get(build.Spec.Template.Name)
					if err != nil {
						// The BuildTemplate resource may not exist.
						if errors.IsNotFound(err) {
							runtime.HandleError(fmt.Errorf("build template %q in namespace %q does not exist", build.Spec.Template.Name, build.Namespace))
						}
						return err
					}
				}
				// TODO(shashwathi): Validate template here instead of doing as part of ValidateBuild function
				// It reduces calls to build API
				build, err = builder.ApplyTemplate(build, tmpl)
				if err != nil {
					c.Logger.Errorf("Error applying template: %v", err)
					return err
				}
			}

			// TODO: Validate build except steps+template
			b, err := c.builder.BuildFromSpec(build)
			if err != nil {
				c.Logger.Errorf("Error building from spec")
				return err
			}
			op, err := b.Execute()
			if err != nil {
				build.Status.SetCondition(&duckv1alpha1.Condition{
					Type:    v1alpha1.BuildSucceeded,
					Status:  corev1.ConditionFalse,
					Reason:  "BuildExecuteFailed",
					Message: err.Error(),
				})
				c.Logger.Errorf("Failed to execute: %v for build", err, build.Name)
				c.recorder.Eventf(build, corev1.EventTypeWarning, "BuildExecuteFailed", "Failed to execute Build %q: %v", build.Name, err)
				return err
			}
			if err := op.Checkpoint(build, &build.Status); err != nil {
				c.Logger.Errorf("REMOVE: Failed in operation checking: %v for build", err, build.Name)
				return err
			}
		}
	}
	c.Logger.Infof("Successfully synced '%s'' %s'", build.Name, build.Namespace)
	c.recorder.Event(build, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Reconciler) waitForOperation(build *v1alpha1.Build, op builder.Operation) error {
	status, err := op.Wait()
	if err != nil {
		c.Logger.Errorf("Error while waiting for operation: %v", err)
		return err
	}
	build.Status = *status
	return nil
}

func (c *Reconciler) updateStatus(u *v1alpha1.Build) (*v1alpha1.Build, error) {
	buildClient := c.buildclientset.BuildV1alpha1().Builds(u.Namespace)
	newu, err := buildClient.Get(u.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(newu.Status, u.Status) {
		newu.Status = u.Status
		return buildClient.Update(newu)
	}

	// Until #38113 is merged, we must use Update instead of UpdateStatus to
	// update the Status block of the Build resource. UpdateStatus will not
	// allow changes to the Spec of the resource, which is ideal for ensuring
	// nothing other than resource status has been updated.
	return u, nil
}
