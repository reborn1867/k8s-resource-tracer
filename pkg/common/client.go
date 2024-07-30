package common

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	utilerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

//go:generate mockgen --build_flags=--mod=mod -package mocks -destination mocks/client_mock.go -source=client.go WrappedClient
type WrappedClient interface {
	CreateOrUpdate(ctx context.Context, obj client.Object, f func() error) (controllerutil.OperationResult, error)
	CreateOrPatch(ctx context.Context, obj client.Object, f func() error) (controllerutil.OperationResult, error)
	GetAndUpdate(ctx context.Context, obj client.Object, f func() error) (controllerutil.OperationResult, error)
	GetAndPatch(ctx context.Context, obj client.Object, f func() error) (controllerutil.OperationResult, error)
	CreateOrPatchWithJsonMerge(ctx context.Context, obj client.Object, f func() error) (controllerutil.OperationResult, error)
	CreateIfNotExist(ctx context.Context, obj client.Object) error
	UpdateStatus(ctx context.Context, obj client.Object) error
	// TODO we might need to pass the structure SecretRef as the parameter instead of name, namespace and field
	GetNonEmptySecretField(ctx context.Context, namespace, name, field string) ([]byte, error)
	GetNonEmptyConfigMapField(ctx context.Context, namespace, name, field string) (string, error)
	GetConfigMapFieldYamlUnmarshal(ctx context.Context, namespace, name, field string, obj interface{}) error
}

type ClientOptions struct {
	Backoff wait.Backoff
}

type ClientOption func(*ClientOptions)

type Client interface {
	client.Client
	WrappedClient
	ApplyOptions(...ClientOption)
}

type richClient struct {
	client.Client
	ClientOptions
}

var (
	gardenerSecretProjectField = "project"
	richClientLog              = ctrl.Log.WithName("richClient")
	ErrEmptyKubeconfig         = errors.New("empty kubeconfig field data")
	errEmptyGardenerProject    = errors.New("empty project field data")

	defaultBackoff = wait.Backoff{
		Steps:    5,
		Duration: 1 * time.Second,
		Factor:   1.5,
		Jitter:   0.5,
	}
)

func NewClient(c client.Client, opts ...ClientOption) Client {
	client := &richClient{
		Client: c,
		ClientOptions: ClientOptions{
			Backoff: defaultBackoff,
		}}
	client.ApplyOptions(opts...)
	return client
}

func (c *richClient) ApplyOptions(opts ...ClientOption) {
	for _, opt := range opts {
		opt(&c.ClientOptions)
	}
}

// copy from sigs.k8s.io/controller-runtime/pkg/controller/controllerutil/controllerutil.go
// mutate wraps a func() error and applies validation to its result.
func mutate(f func() error, key client.ObjectKey, obj client.Object) error {
	if err := f(); err != nil {
		return err
	}
	if newKey := client.ObjectKeyFromObject(obj); key != newKey {
		return fmt.Errorf("func() error cannot mutate object name and/or object namespace")
	}
	return nil
}

// this method is copied and changed based on CreateOrUpdate from sigs.k8s.io/controller-runtime/pkg/controller/controllerutil/controllerutil.go
// if the object is there, apply the changes and write the new object to apiserver
// caller should check if the error is about object not found
func getAndUpdate(ctx context.Context, c client.Client, obj client.Object, f func() error) (controllerutil.OperationResult, error) {
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}

	existing := obj.DeepCopyObject() //nolint
	if err := mutate(f, key, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}

	if equality.Semantic.DeepEqual(existing, obj) {
		return controllerutil.OperationResultNone, nil
	}

	if err := c.Update(ctx, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}

func getAndPatch(ctx context.Context, c client.Client, obj client.Object, f func() error) (controllerutil.OperationResult, error) {
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}

	// Create patches for the object and its possible status.
	objPatch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	statusPatch := client.MergeFrom(obj.DeepCopyObject().(client.Object))

	// Create a copy of the original object as well as converting that copy to
	// unstructured data.
	before, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj.DeepCopyObject())
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// Attempt to extract the status from the resource for easier comparison later
	beforeStatus, hasBeforeStatus, err := unstructured.NestedFieldCopy(before, "status")
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// If the resource contains a status then remove it from the unstructured
	// copy to avoid unnecessary patching later.
	if hasBeforeStatus {
		unstructured.RemoveNestedField(before, "status")
	}

	// Mutate the original object.
	if f != nil {
		if err := mutate(f, key, obj); err != nil {
			return controllerutil.OperationResultNone, err
		}
	}

	// Convert the resource to unstructured to compare against our before copy.
	after, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// Attempt to extract the status from the resource for easier comparison later
	afterStatus, hasAfterStatus, err := unstructured.NestedFieldCopy(after, "status")
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// If the resource contains a status then remove it from the unstructured
	// copy to avoid unnecessary patching later.
	if hasAfterStatus {
		unstructured.RemoveNestedField(after, "status")
	}

	result := controllerutil.OperationResultNone

	if !reflect.DeepEqual(before, after) {
		// Only issue a Patch if the before and after resources (minus status) differ
		if err := c.Patch(ctx, obj, objPatch); err != nil {
			return result, err
		}
		result = controllerutil.OperationResultUpdated
	}

	if (hasBeforeStatus || hasAfterStatus) && !reflect.DeepEqual(beforeStatus, afterStatus) {
		// Only issue a Status Patch if the resource has a status and the beforeStatus
		// and afterStatus copies differ
		if result == controllerutil.OperationResultUpdated {
			// If Status was replaced by Patch before, set it to afterStatus
			objectAfterPatch, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				return result, err
			}
			if err = unstructured.SetNestedField(objectAfterPatch, afterStatus, "status"); err != nil {
				return result, err
			}
			// If Status was replaced by Patch before, restore patched structure to the obj
			if err = runtime.DefaultUnstructuredConverter.FromUnstructured(objectAfterPatch, obj); err != nil {
				return result, err
			}
		}
		if err := c.Status().Patch(ctx, obj, statusPatch); err != nil {
			return result, err
		}
		if result == controllerutil.OperationResultUpdated {
			result = controllerutil.OperationResultUpdatedStatus
		} else {
			result = controllerutil.OperationResultUpdatedStatusOnly
		}
	}

	return result, nil
}

func createOrPatchWithJsonMerge(ctx context.Context, c client.Client, obj client.Object, f func() error) (controllerutil.OperationResult, error) {
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		if !utilerrors.IsNotFound(err) {
			return controllerutil.OperationResultNone, err
		}
		if err := mutate(f, key, obj); err != nil {
			return controllerutil.OperationResultNone, err
		}
		if err := c.Create(ctx, obj); err != nil {
			return controllerutil.OperationResultNone, err
		}
		return controllerutil.OperationResultCreated, nil
	}

	existing := obj.DeepCopyObject() //nolint
	if err := mutate(f, key, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}

	if equality.Semantic.DeepEqual(existing, obj) {
		return controllerutil.OperationResultNone, nil
	}

	if err := c.Patch(ctx, obj, client.Merge); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}

func (c *richClient) GetAndUpdate(ctx context.Context, obj client.Object, f func() error) (result controllerutil.OperationResult, err error) {
	err = retry.RetryOnConflict(c.Backoff, func() (err error) {
		result, err = getAndUpdate(ctx, c.Client, obj, f)
		return err
	})

	return result, err
}

func (c *richClient) CreateOrPatchWithJsonMerge(ctx context.Context, obj client.Object, f func() error) (result controllerutil.OperationResult, err error) {
	err = retry.RetryOnConflict(c.Backoff, func() (err error) {
		result, err = createOrPatchWithJsonMerge(ctx, c.Client, obj, f)
		return err
	})

	return result, err
}

func (c *richClient) GetAndPatch(ctx context.Context, obj client.Object, f func() error) (result controllerutil.OperationResult, err error) {
	err = retry.RetryOnConflict(c.Backoff, func() (err error) {
		result, err = getAndPatch(ctx, c.Client, obj, f)
		return err
	})
	return result, err
}

func (c *richClient) CreateOrUpdate(ctx context.Context, obj client.Object, f func() error) (result controllerutil.OperationResult, err error) {
	err = retry.RetryOnConflict(c.Backoff, func() (err error) {
		result, err = controllerutil.CreateOrUpdate(ctx, c.Client, obj, f)
		return err
	})
	return result, err
}

func (c *richClient) CreateOrPatch(ctx context.Context, obj client.Object, f func() error) (result controllerutil.OperationResult, err error) {
	err = retry.RetryOnConflict(c.Backoff, func() (err error) {
		result, err = controllerutil.CreateOrPatch(ctx, c.Client, obj, f)
		return err
	})
	return result, err
}

func (c *richClient) CreateIfNotExist(ctx context.Context, obj client.Object) error {
	return retry.RetryOnConflict(c.Backoff, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if err := c.Create(ctx, obj); err != nil && !utilerrors.IsAlreadyExists(err) {
				return err
			}
		}
		return nil
	})
}

func (c *richClient) UpdateStatus(ctx context.Context, obj client.Object) error {
	return retry.RetryOnConflict(c.Backoff, func() error {
		err := c.Status().Update(ctx, obj)
		return err
	})
}

func (c *richClient) GetNonEmptySecretField(ctx context.Context, namespace, name, field string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, err
	}

	if secret.Data == nil {
		return nil, fmt.Errorf("secret %s is empty", name)
	}

	if len(secret.Data[field]) == 0 {
		return nil, fmt.Errorf("empty field %s in secret %s", field, name)
	}
	return secret.Data[field], nil
}

func (c *richClient) GetNonEmptyConfigMapField(ctx context.Context, namespace, name, field string) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm); err != nil {
		return "", err
	}

	if len(cm.Data[field]) == 0 {
		return "", fmt.Errorf("empty field %s in configmap %s", field, name)
	}
	return cm.Data[field], nil
}

func (c *richClient) GetConfigMapFieldYamlUnmarshal(ctx context.Context, namespace, name, field string, obj interface{}) error {
	s, err := c.GetNonEmptyConfigMapField(ctx, namespace, name, field)
	if err != nil {
		return err
	}

	return yaml.Unmarshal([]byte(s), obj)
}
