/*
Copyright 2021 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	api "github.com/cert-manager/aws-privateca-issuer/pkg/api/v1beta1"
	awspca "github.com/cert-manager/aws-privateca-issuer/pkg/aws"
	"github.com/cert-manager/aws-privateca-issuer/pkg/util"
	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

var (
	errNoArnOrRefInSpec  = errors.New("either arn or certificateAuthorityRef must be specified")
	errBothArnAndRef     = errors.New("arn and certificateAuthorityRef are mutually exclusive")
	errNoRegionInSpec    = errors.New("no Region found in Issuer Spec")
	errRefArnNotReady    = errors.New("referenced CertificateAuthority ARN not yet available")
)

const (
	defaultACKGroup   = "acmpca.services.k8s.aws"
	defaultACKVersion = "v1alpha1"
	defaultACKKind    = "CertificateAuthority"
)

var awsDefaultRegion = os.Getenv("AWS_REGION")

// GenericIssuerReconciler reconciles both AWSPCAIssuer and AWSPCAClusterIssuer objects
type GenericIssuerReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// GetCallerIdentitty should be set to true if you want to call and log the
	// result of sts.GetCallerIdentity.
	// This is useful to verify what AWS user is being authenticated by the Issuer,
	// but can be skipped during unit tests to avoid having a dependency on a
	// live STS service.
	GetCallerIdentity bool
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *GenericIssuerReconciler) Reconcile(ctx context.Context, req ctrl.Request, issuer api.GenericIssuer) (ctrl.Result, error) {
	log := r.Log.WithValues("genericissuer", req.NamespacedName)
	spec := issuer.GetSpec()
	err := validateIssuer(spec)
	if err != nil {
		log.Error(err, "failed to validate issuer")
		_ = r.setStatus(ctx, issuer, metav1.ConditionFalse, "Validation", fmt.Sprintf("Failed to validate resource: %v", err))
		return ctrl.Result{}, err
	}

	if spec.CertificateAuthorityRef != nil {
		resolvedArn, err := r.resolveCertificateAuthorityRef(ctx, spec.CertificateAuthorityRef, req.Namespace)
		if err != nil {
			if errors.Is(err, errRefArnNotReady) {
				log.Info("Waiting for CertificateAuthority ARN to be populated", "ref", spec.CertificateAuthorityRef.Name)
				_ = r.setStatus(ctx, issuer, metav1.ConditionFalse, "Waiting", "Referenced CertificateAuthority ARN not yet available")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			log.Error(err, "failed to resolve certificateAuthorityRef")
			_ = r.setStatus(ctx, issuer, metav1.ConditionFalse, "Error", fmt.Sprintf("Failed to resolve ref: %v", err))
			return ctrl.Result{}, err
		}
		spec.Arn = resolvedArn
	}

	awspca.DeleteProvisioner(ctx, r.Client, req.NamespacedName)
	cfg, err := awspca.GetConfig(ctx, r.Client, spec)
	if err != nil {
		log.Error(err, "Error loading config")
		_ = r.setStatus(ctx, issuer, metav1.ConditionFalse, "Error", err.Error())
		return ctrl.Result{}, err
	}

	if r.GetCallerIdentity {
		id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Error(err, "failed to sts.GetCallerIdentity")
			return ctrl.Result{}, err
		}
		log.Info("sts.GetCallerIdentity", "arn", id.Arn, "account", id.Account, "user_id", id.UserId)
	}

	return ctrl.Result{}, r.setStatus(ctx, issuer, metav1.ConditionTrue, "Verified", "Issuer verified")
}

// resolveCertificateAuthorityRef looks up an ACK CertificateAuthority resource
// and returns the ARN from its status.
func (r *GenericIssuerReconciler) resolveCertificateAuthorityRef(ctx context.Context, ref *api.CertificateAuthorityReference, defaultNamespace string) (string, error) {
	return resolveCertificateAuthorityArn(ctx, r.Client, ref, defaultNamespace)
}

// resolveCertificateAuthorityArn is a shared helper that resolves a CertificateAuthorityReference
// to an ARN by reading the ACK resource's status.
func resolveCertificateAuthorityArn(ctx context.Context, c client.Client, ref *api.CertificateAuthorityReference, defaultNamespace string) (string, error) {
	group := ref.Group
	if group == "" {
		group = defaultACKGroup
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	ca := &unstructured.Unstructured{}
	ca.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Version: defaultACKVersion,
		Kind:    defaultACKKind,
	})

	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}
	if err := c.Get(ctx, key, ca); err != nil {
		return "", fmt.Errorf("failed to get CertificateAuthority %s: %w", key, err)
	}

	ackMetadata, found, err := unstructured.NestedMap(ca.Object, "status", "ackResourceMetadata")
	if err != nil || !found {
		return "", errRefArnNotReady
	}

	arnValue, ok := ackMetadata["arn"].(string)
	if !ok || arnValue == "" {
		return "", errRefArnNotReady
	}

	return arnValue, nil
}

func (r *GenericIssuerReconciler) setStatus(ctx context.Context, issuer api.GenericIssuer, status metav1.ConditionStatus, reason, message string) error {
	log := r.Log.WithValues("genericissuer", issuer.GetName())
	util.SetIssuerCondition(log, issuer, api.ConditionTypeReady, status, reason, message)

	eventType := core.EventTypeNormal
	if status == metav1.ConditionFalse {
		eventType = core.EventTypeWarning
	}
	r.Recorder.Event(issuer, eventType, reason, message)

	return r.Client.Status().Update(ctx, issuer)
}

// enqueueCertificateAuthorityRefIssuers returns a handler that maps a CertificateAuthority
// event to reconcile requests for any issuers that reference it via certificateAuthorityRef.
func enqueueCertificateAuthorityRefIssuers(c client.Client, clusterScoped bool) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		var requests []ctrl.Request

		if clusterScoped {
			var issuers api.AWSPCAClusterIssuerList
			if err := c.List(ctx, &issuers); err != nil {
				return nil
			}
			for _, iss := range issuers.Items {
				if iss.Spec.CertificateAuthorityRef != nil && iss.Spec.CertificateAuthorityRef.Name == obj.GetName() {
					requests = append(requests, ctrl.Request{
						NamespacedName: types.NamespacedName{Name: iss.Name},
					})
				}
			}
		} else {
			var issuers api.AWSPCAIssuerList
			if err := c.List(ctx, &issuers, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			for _, iss := range issuers.Items {
				if iss.Spec.CertificateAuthorityRef != nil && iss.Spec.CertificateAuthorityRef.Name == obj.GetName() {
					requests = append(requests, ctrl.Request{
						NamespacedName: types.NamespacedName{Name: iss.Name, Namespace: iss.Namespace},
					})
				}
			}
		}

		return requests
	}
}

func validateIssuer(spec *api.AWSPCAIssuerSpec) error {
	hasArn := spec.Arn != ""
	hasRef := spec.CertificateAuthorityRef != nil

	switch {
	case hasArn && hasRef:
		return errBothArnAndRef
	case !hasArn && !hasRef:
		return errNoArnOrRefInSpec
	case spec.Region == "" && awsDefaultRegion == "":
		return errNoRegionInSpec
	}
	return nil
}
