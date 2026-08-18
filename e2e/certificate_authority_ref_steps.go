package main

import (
	"context"
	"fmt"
	"time"

	"github.com/cucumber/godog"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/cert-manager/aws-privateca-issuer/pkg/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var caGVR = schema.GroupVersionResource{
	Group:    "acmpca.services.k8s.aws",
	Version:  "v1alpha1",
	Resource: "certificateauthorities",
}

type CertificateAuthorityRefContext struct {
	caName       string
	caNamespace  string
	dynamicClient dynamic.Interface
}

func newCertificateAuthorityRefContext() *CertificateAuthorityRefContext {
	clientConfig, err := clientcmd.BuildConfigFromFlags("", KubeConfigPath)
	if err != nil {
		panic("failed to build kubeconfig: " + err.Error())
	}

	dynClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		panic("failed to create dynamic client: " + err.Error())
	}

	return &CertificateAuthorityRefContext{
		dynamicClient: dynClient,
	}
}

func (caCtx *CertificateAuthorityRefContext) createACKCertificateAuthority(ctx context.Context) error {
	return caCtx.createACKCertificateAuthorityInNamespace(ctx, "default")
}

func (caCtx *CertificateAuthorityRefContext) createACKCertificateAuthorityInNamespace(ctx context.Context, namespace string) error {
	caCtx.caName = "e2e-ca-" + uuid.New().String()[:8]
	caCtx.caNamespace = namespace

	ca := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "acmpca.services.k8s.aws/v1alpha1",
			"kind":       "CertificateAuthority",
			"metadata": map[string]interface{}{
				"name":      caCtx.caName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"certificateAuthorityConfiguration": map[string]interface{}{
					"keyAlgorithm":     "RSA_2048",
					"signingAlgorithm": "SHA256WITHRSA",
					"subject": map[string]interface{}{
						"commonName": "e2e-test-ca",
					},
				},
				"certificateAuthorityType": "ROOT",
				"usageMode":                "SHORT_LIVED_CERTIFICATE",
				"tags": []interface{}{
					map[string]interface{}{
						"key":   "ManagedBy",
						"value": "e2e-test",
					},
				},
			},
		},
	}

	_, err := caCtx.dynamicClient.Resource(caGVR).Namespace(namespace).Create(ctx, ca, metav1.CreateOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Failed to create ACK CertificateAuthority: "+err.Error())
	}

	// Wait for the CA ARN to be populated by the ACK controller
	err = caCtx.waitForCAArn(ctx)
	if err != nil {
		assert.FailNow(godog.T(ctx), "ACK CertificateAuthority ARN was not populated: "+err.Error())
	}

	return nil
}

func (caCtx *CertificateAuthorityRefContext) createACKCertificateAuthorityWithName(ctx context.Context) error {
	// caName is already set by the issuer step that created a ref to a non-existent CA
	caCtx.caNamespace = "default"

	ca := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "acmpca.services.k8s.aws/v1alpha1",
			"kind":       "CertificateAuthority",
			"metadata": map[string]interface{}{
				"name":      caCtx.caName,
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"certificateAuthorityConfiguration": map[string]interface{}{
					"keyAlgorithm":     "RSA_2048",
					"signingAlgorithm": "SHA256WITHRSA",
					"subject": map[string]interface{}{
						"commonName": "e2e-test-ca-deferred",
					},
				},
				"certificateAuthorityType": "ROOT",
				"usageMode":                "SHORT_LIVED_CERTIFICATE",
			},
		},
	}

	_, err := caCtx.dynamicClient.Resource(caGVR).Namespace("default").Create(ctx, ca, metav1.CreateOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Failed to create ACK CertificateAuthority: "+err.Error())
	}

	return nil
}

func (caCtx *CertificateAuthorityRefContext) waitForCAArn(ctx context.Context) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ca, err := caCtx.dynamicClient.Resource(caGVR).Namespace(caCtx.caNamespace).Get(ctx, caCtx.caName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}

			arn, found, _ := unstructured.NestedString(ca.Object, "status", "ackResourceMetadata", "arn")
			return found && arn != "", nil
		})
}

func (caCtx *CertificateAuthorityRefContext) deleteACKCertificateAuthority(ctx context.Context) {
	if caCtx.caName != "" {
		caCtx.dynamicClient.Resource(caGVR).Namespace(caCtx.caNamespace).Delete(ctx, caCtx.caName, metav1.DeleteOptions{})
	}
}

func (issCtx *IssuerContext) createClusterIssuerWithRef(ctx context.Context, caCtx *CertificateAuthorityRefContext) error {
	issCtx.issuerName = uuid.New().String() + "--cluster-issuer--ref"
	issCtx.issuerType = "AWSPCAClusterIssuer"

	issSpec := v1beta1.AWSPCAClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: issCtx.issuerName},
		Spec: v1beta1.AWSPCAIssuerSpec{
			CertificateAuthorityRef: &v1beta1.CertificateAuthorityReference{
				Name:      caCtx.caName,
				Namespace: caCtx.caNamespace,
			},
			Region: testContext.region,
		},
	}

	_, err := testContext.iclient.AWSPCAClusterIssuers().Create(ctx, &issSpec, metav1.CreateOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Could not create ClusterIssuer with ref: "+err.Error())
	}

	err = waitForClusterIssuerReady(ctx, testContext.iclient, issCtx.issuerName)
	if err != nil {
		assert.FailNow(godog.T(ctx), "ClusterIssuer with ref did not reach ready state: "+err.Error())
	}

	return nil
}

func (issCtx *IssuerContext) createClusterIssuerRefToNonExistent(ctx context.Context, caCtx *CertificateAuthorityRefContext) error {
	caCtx.caName = "deferred-ca-" + uuid.New().String()[:8]
	issCtx.issuerName = uuid.New().String() + "--cluster-issuer--deferred"
	issCtx.issuerType = "AWSPCAClusterIssuer"

	issSpec := v1beta1.AWSPCAClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: issCtx.issuerName},
		Spec: v1beta1.AWSPCAIssuerSpec{
			CertificateAuthorityRef: &v1beta1.CertificateAuthorityReference{
				Name: caCtx.caName,
			},
			Region: testContext.region,
		},
	}

	_, err := testContext.iclient.AWSPCAClusterIssuers().Create(ctx, &issSpec, metav1.CreateOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Could not create ClusterIssuer: "+err.Error())
	}

	return nil
}

func (issCtx *IssuerContext) createNamespaceIssuerWithRef(ctx context.Context, caCtx *CertificateAuthorityRefContext) error {
	issCtx.issuerName = uuid.New().String() + "--issuer--ref"
	issCtx.issuerType = "AWSPCAIssuer"

	issSpec := v1beta1.AWSPCAIssuer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      issCtx.issuerName,
			Namespace: issCtx.namespace,
		},
		Spec: v1beta1.AWSPCAIssuerSpec{
			CertificateAuthorityRef: &v1beta1.CertificateAuthorityReference{
				Name: caCtx.caName,
			},
			Region: testContext.region,
		},
	}

	_, err := testContext.iclient.AWSPCAIssuers(issCtx.namespace).Create(ctx, &issSpec, metav1.CreateOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Could not create namespaced Issuer with ref: "+err.Error())
	}

	err = waitForIssuerReady(ctx, testContext.iclient, issCtx.issuerName, issCtx.namespace)
	if err != nil {
		assert.FailNow(godog.T(ctx), "Namespaced issuer with ref did not reach ready state: "+err.Error())
	}

	return nil
}

func (issCtx *IssuerContext) verifyIssuerStatus(ctx context.Context, expectedReason string) error {
	issuer, err := testContext.iclient.AWSPCAClusterIssuers().Get(ctx, issCtx.issuerName, metav1.GetOptions{})
	if err != nil {
		assert.FailNow(godog.T(ctx), "Could not get ClusterIssuer: "+err.Error())
	}

	for _, condition := range issuer.Status.Conditions {
		if condition.Type == "Ready" && condition.Reason == expectedReason {
			return nil
		}
	}

	return fmt.Errorf("issuer does not have expected status reason %q", expectedReason)
}

func (issCtx *IssuerContext) waitForIssuerReadyWithTimeout(ctx context.Context, timeoutSeconds int) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Duration(timeoutSeconds)*time.Second, true,
		func(ctx context.Context) (bool, error) {
			issuer, err := testContext.iclient.AWSPCAClusterIssuers().Get(ctx, issCtx.issuerName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}

			for _, condition := range issuer.Status.Conditions {
				if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		})
}

func InitializeCertificateAuthorityRefScenario(ctx *godog.ScenarioContext) {
	caCtx := newCertificateAuthorityRefContext()
	issuerContext := &IssuerContext{
		namespace: "default",
		secretRef: v1beta1.AWSCredentialsSecretReference{},
	}

	ctx.Step(`^I create an ACK CertificateAuthority resource$`, func(ctx context.Context) error {
		return caCtx.createACKCertificateAuthority(ctx)
	})
	ctx.Step(`^I create an ACK CertificateAuthority resource in the namespace$`, func(ctx context.Context) error {
		return caCtx.createACKCertificateAuthorityInNamespace(ctx, issuerContext.namespace)
	})
	ctx.Step(`^I create an ACK CertificateAuthority resource with the referenced name$`, func(ctx context.Context) error {
		return caCtx.createACKCertificateAuthorityWithName(ctx)
	})
	ctx.Step(`^I create an AWSPCAClusterIssuer using a certificateAuthorityRef$`, func(ctx context.Context) error {
		return issuerContext.createClusterIssuerWithRef(ctx, caCtx)
	})
	ctx.Step(`^I create an AWSPCAClusterIssuer referencing a non-existent CA$`, func(ctx context.Context) error {
		return issuerContext.createClusterIssuerRefToNonExistent(ctx, caCtx)
	})
	ctx.Step(`^I create an AWSPCAIssuer using a certificateAuthorityRef$`, func(ctx context.Context) error {
		return issuerContext.createNamespaceIssuerWithRef(ctx, caCtx)
	})
	ctx.Step(`^the issuer should have status Waiting$`, func(ctx context.Context) error {
		// Give the controller a moment to reconcile
		time.Sleep(3 * time.Second)
		return issuerContext.verifyIssuerStatus(ctx, "Waiting")
	})
	ctx.Step(`^the issuer should become ready within (\d+) seconds$`, func(ctx context.Context, timeout int) error {
		return issuerContext.waitForIssuerReadyWithTimeout(ctx, timeout)
	})

	// Reuse existing steps for certificate issuance
	ctx.Step(`^I create a namespace$`, issuerContext.createNamespace)
	ctx.Step(`^I issue a (SHORT_VALIDITY|RSA|ECDSA|CA) certificate$`, issuerContext.issueCertificate)
	ctx.Step(`^the certificate should be issued successfully$`, issuerContext.verifyCertificateIssued)

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		// Cleanup ACK CA
		caCtx.deleteACKCertificateAuthority(ctx)

		// Cleanup issuer
		switch issuerContext.issuerType {
		case "AWSPCAClusterIssuer":
			testContext.iclient.AWSPCAClusterIssuers().Delete(ctx, issuerContext.issuerName, metav1.DeleteOptions{})
		case "AWSPCAIssuer":
			testContext.iclient.AWSPCAIssuers(issuerContext.namespace).Delete(ctx, issuerContext.issuerName, metav1.DeleteOptions{})
		}

		// Cleanup cert
		if issuerContext.certName != "" {
			testContext.cmClient.Certificates(issuerContext.namespace).Delete(ctx, issuerContext.certName, metav1.DeleteOptions{})
			testContext.clientset.CoreV1().Secrets(issuerContext.namespace).Delete(ctx, issuerContext.certName+"-cert-secret", metav1.DeleteOptions{})
		}

		// Cleanup namespace
		if issuerContext.namespace != "default" {
			testContext.clientset.CoreV1().Namespaces().Delete(ctx, issuerContext.namespace, metav1.DeleteOptions{})
		}

		return ctx, nil
	})
}
