/*
Copyright 2024 The Kubernetes Authors.

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
	"testing"
	"time"

	logrtesting "github.com/go-logr/logr/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "k8s.io/api/core/v1"

	issuerapi "github.com/cert-manager/aws-privateca-issuer/pkg/api/v1beta1"
	awspca "github.com/cert-manager/aws-privateca-issuer/pkg/aws"
)

// TestCertificateAuthorityRefIntegration tests the full lifecycle:
// 1. Issuer created with certificateAuthorityRef -> requeues (CA not ready)
// 2. CA gets ARN populated in status -> issuer re-reconciles -> becomes Ready
// 3. Issuer with ARN provided directly -> immediately Ready
func TestCertificateAuthorityRefIntegration(t *testing.T) {
	origAWSDefaultRegion := awsDefaultRegion
	awsDefaultRegion = ""
	t.Cleanup(func() {
		awsDefaultRegion = origAWSDefaultRegion
		awspca.ClearProvisioners()
	})

	caGVK := schema.GroupVersionKind{
		Group:   "acmpca.services.k8s.aws",
		Version: "v1alpha1",
		Kind:    "CertificateAuthority",
	}

	scheme := runtime.NewScheme()
	require.NoError(t, issuerapi.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	scheme.AddKnownTypeWithName(caGVK, &unstructured.Unstructured{})

	t.Run("full lifecycle: CA not ready -> CA ready -> issuer ready", func(t *testing.T) {
		// Step 1: Create a CertificateAuthority without an ARN in status (simulates ACK just created it)
		ca := &unstructured.Unstructured{}
		ca.SetGroupVersionKind(caGVK)
		ca.SetName("my-ca")
		ca.SetNamespace("ns1")
		ca.Object["spec"] = map[string]interface{}{
			"certificateAuthorityConfiguration": map[string]interface{}{
				"keyAlgorithm":     "RSA_2048",
				"signingAlgorithm": "SHA256WITHRSA",
			},
		}

		// Create issuer referencing the CA
		issuer := &issuerapi.AWSPCAIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "test-issuer", Namespace: "ns1"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				CertificateAuthorityRef: &issuerapi.CertificateAuthorityReference{
					Name: "my-ca",
				},
				Region: "us-east-1",
			},
			Status: issuerapi.AWSPCAIssuerStatus{
				Conditions: []metav1.Condition{
					{Type: issuerapi.ConditionTypeReady, Status: metav1.ConditionUnknown},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(issuer, ca).
			WithStatusSubresource(issuer).
			Build()

		controller := GenericIssuerReconciler{
			Client:   fakeClient,
			Log:      logrtesting.NewTestLogger(t),
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		ctx := context.TODO()
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-issuer", Namespace: "ns1"}}

		// Reconcile #1: CA has no ARN yet -> should requeue
		require.NoError(t, controller.Client.Get(ctx, req.NamespacedName, issuer))
		result, err := controller.Reconcile(ctx, req, issuer)

		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, result)
		assert.Equal(t, metav1.ConditionFalse, issuer.Status.Conditions[0].Status)
		assert.Equal(t, "Waiting", issuer.Status.Conditions[0].Reason)

		// Step 2: Simulate ACK controller populating the CA's ARN in status
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "my-ca", Namespace: "ns1"}, ca))
		unstructured.SetNestedMap(ca.Object, map[string]interface{}{
			"arn": "arn:aws:acm-pca:us-east-1:123456789012:certificate-authority/abc-def-123",
		}, "status", "ackResourceMetadata")
		require.NoError(t, fakeClient.Update(ctx, ca))

		// Reconcile #2: CA now has ARN -> issuer should become Ready
		require.NoError(t, controller.Client.Get(ctx, req.NamespacedName, issuer))
		result, err = controller.Reconcile(ctx, req, issuer)

		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)
		assert.Equal(t, metav1.ConditionTrue, issuer.Status.Conditions[0].Status)
		assert.Equal(t, "Verified", issuer.Status.Conditions[0].Reason)
	})

	t.Run("CA does not exist -> error", func(t *testing.T) {
		issuer := &issuerapi.AWSPCAIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-issuer", Namespace: "ns1"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				CertificateAuthorityRef: &issuerapi.CertificateAuthorityReference{
					Name: "nonexistent-ca",
				},
				Region: "us-east-1",
			},
			Status: issuerapi.AWSPCAIssuerStatus{
				Conditions: []metav1.Condition{
					{Type: issuerapi.ConditionTypeReady, Status: metav1.ConditionUnknown},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(issuer).
			WithStatusSubresource(issuer).
			Build()

		controller := GenericIssuerReconciler{
			Client:   fakeClient,
			Log:      logrtesting.NewTestLogger(t),
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		ctx := context.TODO()
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "orphan-issuer", Namespace: "ns1"}}

		require.NoError(t, controller.Client.Get(ctx, req.NamespacedName, issuer))
		result, err := controller.Reconcile(ctx, req, issuer)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get CertificateAuthority")
		assert.Equal(t, ctrl.Result{}, result)
		assert.Equal(t, metav1.ConditionFalse, issuer.Status.Conditions[0].Status)
	})

	t.Run("cluster issuer with ref resolves across namespaces", func(t *testing.T) {
		ca := &unstructured.Unstructured{}
		ca.SetGroupVersionKind(caGVK)
		ca.SetName("shared-ca")
		ca.SetNamespace("infra")
		unstructured.SetNestedMap(ca.Object, map[string]interface{}{
			"arn": "arn:aws:acm-pca:us-east-1:123456789012:certificate-authority/shared-ca-id",
		}, "status", "ackResourceMetadata")

		clusterIssuer := &issuerapi.AWSPCAClusterIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "global-issuer"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				CertificateAuthorityRef: &issuerapi.CertificateAuthorityReference{
					Name:      "shared-ca",
					Namespace: "infra",
				},
				Region: "us-east-1",
			},
			Status: issuerapi.AWSPCAIssuerStatus{
				Conditions: []metav1.Condition{
					{Type: issuerapi.ConditionTypeReady, Status: metav1.ConditionUnknown},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(clusterIssuer, ca).
			WithStatusSubresource(clusterIssuer).
			Build()

		controller := GenericIssuerReconciler{
			Client:   fakeClient,
			Log:      logrtesting.NewTestLogger(t),
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		ctx := context.TODO()
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "global-issuer"}}

		require.NoError(t, controller.Client.Get(ctx, req.NamespacedName, clusterIssuer))
		result, err := controller.Reconcile(ctx, req, clusterIssuer)

		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, result)
		assert.Equal(t, metav1.ConditionTrue, clusterIssuer.Status.Conditions[0].Status)
	})

	t.Run("enqueue map function finds referencing issuers", func(t *testing.T) {
		// Create two issuers: one referencing "target-ca", one referencing something else
		issuer1 := &issuerapi.AWSPCAIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "ref-issuer", Namespace: "ns1"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				CertificateAuthorityRef: &issuerapi.CertificateAuthorityReference{Name: "target-ca"},
				Region:                  "us-east-1",
			},
		}
		issuer2 := &issuerapi.AWSPCAIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "other-issuer", Namespace: "ns1"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				CertificateAuthorityRef: &issuerapi.CertificateAuthorityReference{Name: "different-ca"},
				Region:                  "us-east-1",
			},
		}
		issuer3 := &issuerapi.AWSPCAIssuer{
			ObjectMeta: metav1.ObjectMeta{Name: "arn-issuer", Namespace: "ns1"},
			Spec: issuerapi.AWSPCAIssuerSpec{
				Arn:    "arn:aws:acm-pca:us-east-1:123:ca/direct",
				Region: "us-east-1",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(issuer1, issuer2, issuer3).
			Build()

		ca := &unstructured.Unstructured{}
		ca.SetGroupVersionKind(caGVK)
		ca.SetName("target-ca")
		ca.SetNamespace("ns1")

		mapFn := enqueueCertificateAuthorityRefIssuers(fakeClient, false)
		requests := mapFn(context.TODO(), ca)

		assert.Len(t, requests, 1)
		assert.Equal(t, "ref-issuer", requests[0].Name)
		assert.Equal(t, "ns1", requests[0].Namespace)
	})
}
